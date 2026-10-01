//! 机器人管理器：用户名 → 运行中的机器人。
//!
//! ## 为什么每个机器人一个线程
//!
//! Azalea 的 `Swarm` 持有 `Arc<RwLock<World>>`，而 `World` 不是 `Sync`，
//! 所以建立连接的 future 不是 `Send`：它既不能 `tokio::spawn` 到多线程
//! runtime，也不能 `spawn_local` 到一个跟它不共享的 `LocalSet` 上。
//!
//! 一开始的写法是「整个节点跑一个 `LocalSet`，机器人用 `spawn_local` 启动」。
//! 那行不通，因为 `axum::serve` 对每条连接用 `tokio::spawn`（见
//! `axum/src/serve/mod.rs:276`），连接任务跑在 `LocalSet` 之外 —— 于是
//! handler 里调 `spawn_local` 直接 panic。这个错误在单元测试里就暴露了，
//! 没有留到真实服务器上。
//!
//! 现在的做法是：**每个机器人一个专属线程，线程里跑一个 `current_thread`
//! runtime**。Azalea 的 `!Send` future 待在自己那个 runtime 里，从不跨线程；
//! 控制面用 channel 跟它说话。这样控制面可以用普通的多线程 runtime（axum
//! 本来就要求如此），两边各自干净。
//!
//! 代价是每个机器人一个线程。20 个机器人 20 个线程，每个线程大部分时间在
//! 等网络。这是可以接受的交换 —— 换来的是不必和 `!Send` 缠斗。
//!
//! ## 一个刻意的设计：占位先于连接
//!
//! Azalea 的连接要几秒才建立，而 Go 后端在几毫秒内就可能再发一次 startbot；
//! 如果此时还没有占位记录，第二次请求会被当成新启动，于是同一个账号连两遍
//! —— 服务器的登录缓存是按 IP 的，第二次连接会顶掉第一次，表现为机器人反复
//! 上下线。

use parking_lot::Mutex;
use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;
use tokio::sync::mpsc;
use tracing::{info, warn};

use crate::bot::{Bot, Command as BotCommand};

/// 一台节点上所有机器人的集合。
pub struct Manager {
    bots: Mutex<HashMap<String, Arc<BotSlot>>>,
    /// 游戏服务器地址。
    address: String,
    /// 每次启动递增，用来区分「同一个名字的不同一次运行」。
    ///
    /// 连接线程结束时需要把自己从表里摘掉，但它无法知道这中间有没有人用
    /// 同一个名字重新启动过 —— 直接 remove 会把新的那份删掉，于是新机器人
    /// 还在跑，`/botstatus` 却说没有，Go 后端就会再启动一个，循环往复。
    /// 记住自己启动时的编号，只删编号相同的项。
    generation: AtomicU64,
}

/// 一个正在运行的机器人。
pub struct BotSlot {
    pub bot: Arc<Bot>,
    /// 置位后寻路循环会中断。用于 `/stopbot`。
    pub stop: Arc<AtomicBool>,
    /// 发往机器人线程的指令通道。
    ///
    /// 为什么不直接调 `bot.say()`：`Bot` 里的 `Client` 只能在它自己那个
    /// 线程上使用。通道把「想做什么」送过去，由机器人线程自己执行。
    pub commands: mpsc::UnboundedSender<BotCommand>,
    /// 这次运行的编号，见 [`Manager::generation`]。
    generation: u64,
    /// 机器人线程的句柄。
    thread: Mutex<Option<std::thread::JoinHandle<()>>>,
}

impl Manager {
    pub fn new(address: impl Into<String>) -> Arc<Self> {
        Arc::new(Self {
            bots: Mutex::new(HashMap::new()),
            address: address.into(),
            generation: AtomicU64::new(0),
        })
    }

    pub fn address(&self) -> &str {
        &self.address
    }

    /// 正在运行的机器人名字，有序。
    pub fn running(&self) -> Vec<String> {
        let mut names: Vec<String> = self.bots.lock().keys().cloned().collect();
        names.sort();
        names
    }

    /// 取一个机器人，不存在则返回 None。
    pub fn get(&self, username: &str) -> Option<Arc<BotSlot>> {
        self.bots.lock().get(username).cloned()
    }

    /// 启动一个机器人。
    ///
    /// 已经有同名机器人在运行时返回 [`StartError::AlreadyRunning`]，调用方
    /// 据此回 409 —— Go 的 `startBotReplacingStale` 靠这个码决定要不要杀掉
    /// 旧实例重试。
    pub fn start(self: &Arc<Self>, username: &str) -> Result<Arc<BotSlot>, StartError> {
        let mut bots = self.bots.lock();
        if bots.contains_key(username) {
            return Err(StartError::AlreadyRunning);
        }

        let generation = self.generation.fetch_add(1, Ordering::Relaxed);
        let bot = Bot::new(username);
        let (commands_tx, commands_rx) = mpsc::unbounded_channel();

        // 线程启动完成的信号。等它能保证 `start` 返回之后线程一定已经开始
        // 跑 —— 否则紧接着的 stop 可能在任务建立之前就发出，而连接随后才
        // 建立，留下一个没人管的会话。
        let (ready_tx, ready_rx) = std::sync::mpsc::channel::<()>();

        let thread_bot = bot.clone();
        let thread_address = self.address.clone();
        let thread_name = username.to_string();
        let manager = self.clone();
        let name = username.to_string();

        let handle = std::thread::Builder::new()
            .name(format!("bot-{username}"))
            .spawn(move || {
                // panic 也要摘除记录。不这么做的话，panic 之后表里留着一个
                // 死机器人：`/botstatus` 说它在，实际线程已经没了，而 Go
                // 后端也就不会重新启动它 —— 表现为「机器人莫名其妙不见了
                // 但系统说它在跑」。
                //
                // 用 `catch_unwind` 包住而不是依赖「reap 在最后一行」：
                // panic 时最后一行根本不会执行。
                let guard = PanicGuard {
                    manager: manager.clone(),
                    name: name.clone(),
                    generation,
                };
                let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                // 每个机器人一个 current_thread runtime：Azalea 的连接
                // future 不是 Send，只能待在单线程 runtime 上。
                let rt = match tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                {
                    Ok(rt) => rt,
                    Err(e) => {
                        // 摘除交给下面的 `PanicGuard` —— 让它成为唯一出口，
                        // 免得将来有人在这里加分支时忘了 reap。
                        warn!(bot = %thread_name, "无法建立 runtime: {e}");
                        let _ = ready_tx.send(());
                        return;
                    }
                };

                let local = tokio::task::LocalSet::new();
                    local.block_on(&rt, async move {
                        let conn = crate::bot::run(thread_bot.clone(), thread_address);
                        let cmds = command_loop(thread_bot.clone(), commands_rx);
                        // 报 ready 之后才开始跑 —— 顺序反过来的话，start()
                        // 可能在连接已经开始建立之后才返回。
                        let _ = ready_tx.send(());
                        // ★ 这里不能只等两个 future 自然结束。
                        //
                        // 「连不上」那条路上它们**都不会**结束：
                        //
                        //   · `conn` —— Azalea 的 `ClientBuilder::start()` 内部
                        //     是 swarm，要等**所有** bot 退出才返回；连不上的那
                        //     个 bot 仍留在 swarm 里，于是 `start()` 一直挂着。
                        //     实测日志里有「连接失败: Connection refused」，
                        //     却从来没有「连接结束」（那行在 `run` 返回之后）。
                        //
                        //   · `cmds` —— `command_loop` 等的是 `commands_rx`，
                        //     而 `commands_tx` 在 `BotSlot` 里；`BotSlot` 由
                        //     `reap` 从表里移除，而 `reap` 在**线程退出时**才跑。
                        //
                        // 两者互等就是死锁：线程永不退出，它那套 runtime
                        // （eventpoll + eventfd + socket）也永不释放。用户看到的
                        //
                        //     WARN 无法建立 runtime: Too many open files (os error 24)
                        //
                        // 就是这么攒出来的 —— 每轮「启动失败」泄漏 3 个 fd，
                        // Go 的自动重连每 60 秒来一次，很快就到上限。
                        //
                        // 所以加第三条退出路径：**连接已经掉了就收工**。
                        //
                        // 宽限期是必要的：`alive` 在 `Event::Login` 之前一直是
                        // false，而建连要几百毫秒。不等就直接判死会把正常的
                        // 连接过程掐断。
                        tokio::select! {
                            _ = conn => {}
                            _ = cmds => {}
                            _ = wait_until_down(&thread_bot) => {
                                tracing::info!(bot = %thread_name, "连接已掉线，收工");
                            }
                        }
                    });
                }));
                // 正常结束和 panic 都走到这里（guard 的 Drop 负责 reap）。
                tracing::info!(bot = %name, "机器人线程即将退出");
                drop(guard);
            })
            .expect("无法建立机器人线程");

        // 等线程报 ready，最多 5 秒。真等不到也要继续 —— 一个起不来的机器人
        // 不该把整个启动请求挂死，让它进表、由后续的 status 查询暴露问题。
        let _ = ready_rx.recv_timeout(Duration::from_secs(5));

        // 先取出停止标志再交出 `bot` —— 顺序反了会「借用已移动的值」。
        let stop = bot.stop_flag();
        let slot = Arc::new(BotSlot {
            bot,
            stop,
            commands: commands_tx,
            generation,
            thread: Mutex::new(Some(handle)),
        });
        bots.insert(username.to_string(), slot.clone());
        info!(username, "机器人已启动");
        Ok(slot)
    }

    /// 连接线程结束后把自己从表里摘掉。
    ///
    /// 编号不匹配说明这期间有人用同一个名字重启了机器人，此时表里那一项属于
    /// 新的运行，不能删。
    fn reap(&self, username: &str, generation: u64) {
        let mut bots = self.bots.lock();
        match bots.get(username) {
            Some(existing) if existing.generation == generation => {
                bots.remove(username);
                info!(username, "连接结束，已从运行列表移除");
            }
            Some(_) => {
                // 已被新的运行取代，什么都不做。
            }
            None => {}
        }
    }

    /// 停止一个机器人。
    ///
    /// 停止是「请求」而不是「保证立刻结束」：先置位 `stop` 让寻路循环自己
    /// 退出，再关掉指令通道。
    pub fn stop(&self, username: &str) -> Result<(), StopError> {
        let slot = {
            let mut bots = self.bots.lock();
            bots.remove(username).ok_or(StopError::NotRunning)?
        };
        slot.stop.store(true, Ordering::Relaxed);
        // 指令通道一关，机器人线程里的 command_loop 就结束，`select!` 随之
        // 结束整个 `block_on` —— 连接任务被 drop，断开包发出去。
        //
        // 不 abort：Azalea 的连接任务里握着 TCP，硬 abort 会让服务端等到
        // 超时才清会话，而登录缓存按 IP 计，下次启动会被这条顶掉。
        //
        // 这里放下的是 slot 自己的发送端；`commands` 字段随 `slot` 一起被
        // drop，因为表里那一项已经 remove 了，没有别的持有者。
        info!(username, "机器人已停止");
        Ok(())
    }

    /// 停止所有机器人。用于节点关停。
    ///
    /// 返回被停止的机器人数量。
    pub fn stop_all(&self) -> usize {
        let slots: Vec<Arc<BotSlot>> = {
            let mut bots = self.bots.lock();
            bots.drain().map(|(_, v)| v).collect()
        };
        let n = slots.len();
        for slot in &slots {
            slot.stop.store(true, Ordering::Relaxed);
            info!(bot = %slot.bot.username, "关停时停止");
        }
        // slots 在这里 drop，发送端随之关闭，机器人线程退出。
        n
    }

    /// 等所有机器人线程退出，最多 `timeout`。
    ///
    /// 关停时调用：不等待就退进程会让连接在服务端看起来是「突然消失」，
    /// 而服务端的登录缓存按 IP 计，下次启动会被那条僵死的会话顶掉。
    pub fn join_all(&self, timeout: Duration) {
        let slots: Vec<Arc<BotSlot>> = {
            let bots = self.bots.lock();
            bots.values().cloned().collect()
        };
        let deadline = std::time::Instant::now() + timeout;
        for slot in slots {
            let Some(handle) = slot.thread.lock().take() else {
                continue;
            };
            // std 的 join 没有超时版本，所以配合 is_finished 自己等。
            // 不为此引入额外依赖。
            while !handle.is_finished() {
                if std::time::Instant::now() >= deadline {
                    warn!(bot = %slot.bot.username, "等待线程退出超时");
                    return;
                }
                std::thread::sleep(Duration::from_millis(50));
            }
            let _ = handle.join();
        }
    }
}

/// 机器人线程里的指令循环。
///
/// 通道关闭（`/stopbot` 或关停）就返回，从而结束 `block_on`，让连接任务被
/// drop 掉。
/// 等到「连接建立过、又断掉了」。
///
/// 分两段，因为 `alive` 的生命周期是：
///
/// ```text
///   启动 ──── false ────[Login]──── true ────[断开]──── false ──── 一直 false
/// ```
///
/// 如果只等「`alive` 是 false 就返回」，那启动那一瞬间（还没 Login）就会
/// 立刻返回，把正常连接掐断。所以要先等它变 true，再等它变回 false。
///
/// 两个超时都是**兜底**，不是正常路径：
///   · `UP_TIMEOUT` —— 一直连不上（DNS 失败、服务器关机）时不会走 Login，
///     总不能永远等下去。
///   · `DOWN_TIMEOUT` —— 连上之后一直不掉线，那是正常工作状态；这个分支
///     本来就不该触发，给个足够长的上界只是为了不让 future 永远挂着。
async fn wait_until_down(bot: &Arc<Bot>) {
    use std::time::Duration;

    const UP_TIMEOUT: Duration = Duration::from_secs(30);
    const DOWN_TIMEOUT: Duration = Duration::from_secs(24 * 3600);
    const POLL: Duration = Duration::from_millis(200);

    if !wait_for(bot, true, UP_TIMEOUT, POLL).await {
        return;
    }
    let _ = wait_for(bot, false, DOWN_TIMEOUT, POLL).await;
}

/// 轮询等 `bot.is_alive()` 变成 `want`。返回是否等到了（false = 超时）。
async fn wait_for(bot: &Arc<Bot>, want: bool, timeout: std::time::Duration, poll: std::time::Duration) -> bool {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if bot.is_alive() == want {
            return true;
        }
        if tokio::time::Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(poll).await;
    }
}

async fn command_loop(bot: Arc<Bot>, mut rx: mpsc::UnboundedReceiver<BotCommand>) {
    while let Some(cmd) = rx.recv().await {
        match cmd {
            BotCommand::Say { text, reply } => {
                let result = bot.say(&text);
                let _ = reply.send(result);
            }
            BotCommand::GotoChunk {
                chunk_x,
                chunk_z,
                timeout,
                reply,
            } => {
                let stop = bot.stop_flag();
                let result = bot.goto_chunk(chunk_x, chunk_z, timeout, stop).await;
                let _ = reply.send(result);
            }
            BotCommand::RunQueue { reply } => {
                // 队列已经由 `submit` 装载好（区块列表、游标、已占集合）。
                // 这里只负责跑它 —— 必须跑在机器人线程上，因为执行要调寻路，
                // 而寻路只能在持有 Azalea 客户端的那个线程上操作。
                let _ = reply.send(true);
                let me = bot.clone();
                let queue = bot.queue();
                // walk 闭包：走到区块中心。`goto_chunk` 是 async，而队列
                // 只要一个 bool，所以这里把超时和停止标志都接上。
                let stop = bot.stop_flag();
                queue
                    .clone()
                    .run(move |cx, cz| {
                        let me = me.clone();
                        let stop = stop.clone();
                        async move {
                            me.goto_chunk(cx, cz, crate::claimqueue::CHUNK_TIMEOUT, stop)
                                .await
                        }
                    })
                    .await;
            }
        }
    }
}

/// 线程退出时（正常或 panic）把自己从运行表里摘掉。
///
/// 用 `Drop` 而不是在函数末尾调 `reap`：panic 时末尾那行不会执行，而
/// 「线程死了但表里还有记录」会让 Go 后端永远不重启这个机器人。
struct PanicGuard {
    manager: Arc<Manager>,
    name: String,
    generation: u64,
}

impl Drop for PanicGuard {
    fn drop(&mut self) {
        // 正在 panic 时 `reap` 里如果也要 panic，会变成双重 panic 而 abort
        // 整个进程。所以这里吞掉 panic —— 摘除失败是可以接受的降级。
        let manager = self.manager.clone();
        let name = self.name.clone();
        let generation = self.generation;
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
            manager.reap(&name, generation);
        }));
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StartError {
    AlreadyRunning,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StopError {
    NotRunning,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 未启动时查询为空() {
        let m = Manager::new("localhost:25565");
        assert!(m.running().is_empty());
        assert!(m.get("nobody").is_none());
    }

    #[test]
    fn 停止不存在的机器人报错() {
        let m = Manager::new("localhost:25565");
        assert_eq!(m.stop("nobody"), Err(StopError::NotRunning));
    }

    #[test]
    fn 地址原样保留() {
        let m = Manager::new("bgjq.simpfun.cn:25565");
        assert_eq!(m.address(), "bgjq.simpfun.cn:25565");
    }

    #[test]
    fn 运行列表有序() {
        // 有序是为了 `/botstatus` 的返回稳定 —— 顺序随机的话，两边的差异
        // 会淹没在噪声里。
        let m = Manager::new("localhost:25565");
        let mut bots = m.bots.lock();
        bots.insert("zebra".into(), dummy_slot(0));
        bots.insert("alpha".into(), dummy_slot(1));
        drop(bots);
        assert_eq!(m.running(), vec!["alpha".to_string(), "zebra".to_string()]);
    }

    #[test]
    fn 摘除只影响同一代() {
        // 这是防「反复重启」的关键：老线程退出时不能把新线程的记录删掉。
        let m = Manager::new("localhost:25565");
        let mut bots = m.bots.lock();
        bots.insert("bot1".into(), dummy_slot(7));
        drop(bots);

        // 不同代：不删。
        m.reap("bot1", 6);
        assert!(m.get("bot1").is_some(), "老一代不该删掉新一代的记录");

        // 同一代：删。
        m.reap("bot1", 7);
        assert!(m.get("bot1").is_none(), "同一代应当被摘除");
    }

    #[test]
    fn panic_也摘除记录() {
        // 这条验证 PanicGuard：线程 panic 时不能留下死记录。
        //
        // 留下死记录的后果很隐蔽 —— `/botstatus` 说机器人在，实际线程已经
        // 没了，而 Go 后端看到「在运行」就不会重启它。用户看到的是机器人
        // 莫名其妙不见了，但系统说一切正常。
        let m = Manager::new("127.0.0.1:1");
        let mut bots = m.bots.lock();
        bots.insert("bot1".into(), dummy_slot(3));
        drop(bots);
        assert!(m.get("bot1").is_some());

        // 模拟线程 panic：guard 被 drop。
        {
            let _guard = PanicGuard {
                manager: m.clone(),
                name: "bot1".into(),
                generation: 3,
            };
        } // <- drop 在这里发生，等同 panic 时的栈展开

        assert!(
            m.get("bot1").is_none(),
            "panic 之后记录必须被摘除，否则 Go 后端不会重启它"
        );
    }

    fn dummy_slot(generation: u64) -> Arc<BotSlot> {
        let (tx, _rx) = mpsc::unbounded_channel();
        let bot = Bot::new("dummy");
        Arc::new(BotSlot {
            stop: bot.stop_flag(),
            bot,
            commands: tx,
            generation,
            thread: Mutex::new(None),
        })
    }
}
