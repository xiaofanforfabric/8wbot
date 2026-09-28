//! 单个机器人的运行时。
//!
//! 这里把 Azalea 的客户端包成一个可以被 WebSocket 层驱动的对象：启动、停止、
//! 发聊天、走到某个区块。所有对外动作都通过 [`BotHandle`]，它内部是一个
//! `Arc<Bot>`，因此可以跨线程持有 —— Azalea 的 `Client` 本身就是
//! `Arc<RwLock<World>>` 加一个实体 id，克隆是廉价的。
//!
//! 设计上刻意不做「每秒检查、不动就重设目标」这类轮询：原 JS 版在注释里
//! 记过这个坑 —— 反复重设计划让移动包节奏剧烈抖动，在反作弊眼里比正常走路
//! 可疑得多。Azalea 的寻路自带重规划，直接调用就好。

use azalea::pathfinder::goals::XZGoal;
use azalea::prelude::*;
use azalea::{Event, Vec3};
use parking_lot::RwLock;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use tokio::sync::{broadcast, mpsc};
use tracing::{debug, info};

use crate::protocol::{EventData, claim_event, log_event, status_event};
use crate::status::{self, Pos, Status};

/// 机器人事件总线的容量。
///
/// 订阅者落后超过这个数会丢消息而不是阻塞机器人 —— 一条日志不值得让整个
/// 客户端停下，而卡住的订阅者会被 `broadcast` 标记为 lagged 并自行跳过。
const EVENT_CHANNEL_CAPACITY: usize = 256;

/// 一个机器人的全部可变状态。
///
/// 放在 `Arc` 里是因为 Azalea 的事件回调、WebSocket 处理线程和寻路任务都会
/// 读它，而它们运行在不同的 tokio 任务上。
pub struct Bot {
    pub username: String,
    /// Azalea 客户端。第一次事件到达之前为空。
    client: RwLock<Option<Client>>,
    /// 当前解析出的状态，推给前端用。
    status: RwLock<Status>,
    /// 事件总线：机器人产出的日志、状态、开拓都从这里扇出。
    events: broadcast::Sender<EventData>,
    /// 连接是否还活着。用于 `/botstatus` 和停止后的判断。
    alive: AtomicBool,
    /// 邦国信息块的累积缓冲。
    kingdom: RwLock<Option<KingdomBuf>>,
    /// 已开拓的区块数，用于统计。
    claims: AtomicU64,
    /// 停止标志。由 `Bot::new` 创建，`BotSlot` 复用同一个 `Arc`。
    ///
    /// 放在 `Bot` 上而不是只放在 `BotSlot` 上，是因为寻路循环在机器人线程
    /// 里跑，它需要一个自己的句柄来判断「用户是不是按了停止」—— 绕回
    /// Manager 查表会让机器人线程持控制面的锁。
    stop_flag: Arc<AtomicBool>,
}

/// 正在累积的邦国信息块。
///
/// `/u info` 的回复是多行，服务器逐行发来，所以要先攒着再解析。攒的时候
/// 必须有个上限：一条永远等不到结束标志的块（切服、掉线）会一直涨。
struct KingdomBuf {
    title: String,
    lines: Vec<String>,
    started: std::time::Instant,
}

/// 邦国信息块最多累积多久。
///
/// 超过这个时间就当作残缺块丢弃。收到标题后正文通常在一两秒内到齐，给
/// 十秒已经很宽松，而丢掉一个残块比让缓冲无限增长好。
const KINGDOM_COLLECT_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(10);

/// 邦国信息块最多累积多少行。
const KINGDOM_MAX_LINES: usize = 64;

impl Bot {
    pub fn new(username: impl Into<String>) -> Arc<Self> {
        let (events, _) = broadcast::channel(EVENT_CHANNEL_CAPACITY);
        Arc::new(Self {
            username: username.into(),
            client: RwLock::new(None),
            status: RwLock::new(Status::default()),
            events,
            alive: AtomicBool::new(false),
            kingdom: RwLock::new(None),
            claims: AtomicU64::new(0),
            stop_flag: Arc::new(AtomicBool::new(false)),
        })
    }

    /// 取停止标志。见 [`Bot::stop_flag`] 字段的说明。
    pub fn stop_flag(&self) -> Arc<AtomicBool> {
        self.stop_flag.clone()
    }

    /// 订阅这个机器人的事件。
    pub fn subscribe(&self) -> broadcast::Receiver<EventData> {
        self.events.subscribe()
    }

    pub fn is_alive(&self) -> bool {
        self.alive.load(Ordering::Relaxed)
    }

    pub fn status(&self) -> Status {
        self.status.read().clone()
    }

    pub fn claim_count(&self) -> u64 {
        self.claims.load(Ordering::Relaxed)
    }

    /// 当前是否已经拿到 Azalea 客户端。
    pub fn client(&self) -> Option<Client> {
        self.client.read().clone()
    }

    fn emit(&self, data: EventData) {
        // 没有订阅者时 `send` 返回 Err，这是正常情况而不是故障：机器人可能
        // 先于任何控制台连接启动。丢掉即可。
        let _ = self.events.send(data);
    }

    fn log(&self, level: &str, text: impl AsRef<str>) {
        let text = text.as_ref();
        self.emit(log_event(level, text));
        match level {
            "error" => tracing::error!(bot = %self.username, "{text}"),
            "warn" => tracing::warn!(bot = %self.username, "{text}"),
            _ => tracing::info!(bot = %self.username, "{text}"),
        }
    }

    fn push_status(&self) {
        let snapshot = self.status();
        self.emit(status_event(&snapshot));
    }

    /// 发送一条聊天消息或命令。
    ///
    /// 以 `/` 开头就是命令 —— 协议里只有一个字段，由服务端区分，所以这里
    /// 也不分开处理。
    pub fn say(&self, text: &str) -> anyhow::Result<()> {
        let client = self
            .client()
            .ok_or_else(|| anyhow::anyhow!("机器人尚未进入游戏"))?;
        client.chat(text);
        // 自己的消息不会作为 Chat 事件回来（服务端只广播给别人），所以本地
        // 记一条，否则日志里看不到机器人说过什么。
        self.log("info", format!("发送: {text}"));
        Ok(())
    }

    /// 走到某个区块的中心。
    ///
    /// 返回的 future 在到达、超时或被停止时结束，结果是是否真的到了。
    /// 判断依据是位置落在目标区块内，而不是「寻路函数返回了」—— 寻路被
    /// 中断时也会返回，那种情况不算到达。
    pub async fn goto_chunk(
        self: &Arc<Self>,
        chunk_x: i32,
        chunk_z: i32,
        timeout: std::time::Duration,
        stop: Arc<AtomicBool>,
    ) -> bool {
        let Some(client) = self.client() else {
            self.log("error", "机器人尚未进入游戏，无法寻路");
            return false;
        };

        let block_x = chunk_x * 16 + 8;
        let block_z = chunk_z * 16 + 8;

        let distance = horizontal_distance(client.position(), block_x, block_z);
        info!(bot = %self.username, chunk_x, chunk_z, distance, "开始寻路");
        self.log(
            "info",
            format!("前往区块 ({chunk_x}, {chunk_z})，距离 {distance:.0} 格"),
        );

        client.start_goto(XZGoal {
            x: block_x,
            z: block_z,
        });

        let deadline = tokio::time::Instant::now() + timeout;
        let mut ticker = tokio::time::interval(std::time::Duration::from_millis(250));
        let mut reached = false;

        loop {
            ticker.tick().await;

            if stop.load(Ordering::Relaxed) {
                self.log("info", "用户停止，中断寻路");
                break;
            }
            if tokio::time::Instant::now() >= deadline {
                self.log("warn", format!("寻路超时（{}s）", timeout.as_secs()));
                break;
            }
            if in_chunk(client.position(), chunk_x, chunk_z) {
                reached = true;
                break;
            }
            if !self.is_alive() {
                self.log("warn", "连接已断开，中断寻路");
                break;
            }
        }

        // 无论成功失败都要停 —— 留下一个还在跑的寻路会让下一个目标从
        // 「上一次还没走完的路径」开始，表现为机器人往错误方向走。
        client.stop_pathfinding();
        client.walk(azalea::WalkDirection::None);

        if reached {
            self.log("info", format!("已到达区块 ({chunk_x}, {chunk_z})"));
        }
        reached
    }

    /// 处理一条服务端发来的聊天文本。
    ///
    /// 这是状态机的入口：`/server` 的回复、`/u info` 的多行块、疆土开拓通知
    /// 都从这里进来。
    fn on_chat(&self, text: &str) {
        debug!(bot = %self.username, "chat: {text}");
        self.log("chat", text);

        // 疆土开拓：走路触发，服务器主动通知。这是唯一能知道「刚开拓了哪一块」
        // 的来源，所以优先识别。
        if let Some(claim) = status::parse_claim(text) {
            self.claims.fetch_add(1, Ordering::Relaxed);
            let mut st = self.status.write();
            st.last_claim = Some(claim);
            st.updated_at = Some(status::now_rfc3339());
            drop(st);
            self.log(
                "info",
                format!(
                    "开拓疆土: 区块 ({}, {}) 方块 ({}, {})",
                    claim.chunk_x, claim.chunk_z, claim.x, claim.z
                ),
            );
            self.emit(claim_event(&claim));
            self.push_status();
            return;
        }

        if let Some(server) = status::parse_server(text) {
            let mut st = self.status.write();
            st.server = Some(server.clone());
            st.updated_at = Some(status::now_rfc3339());
            drop(st);
            self.push_status();
        }

        // 邦国信息块：先看是不是标题，是就开一个新块。
        if let Some(title) = status::parse_kingdom_head(text) {
            *self.kingdom.write() = Some(KingdomBuf {
                title,
                lines: vec![text.to_string()],
                started: std::time::Instant::now(),
            });
            return;
        }

        let mut finished: Option<(String, Vec<String>)> = None;
        {
            let mut guard = self.kingdom.write();
            if let Some(buf) = guard.as_mut() {
                buf.lines.push(text.to_string());

                // 三种结束条件：收到结束标志、攒够行数、超时。前两个立刻
                // 收尾，超时在下面单独处理 —— 否则一个不发结束标志的服务器
                // 会让这个块永远挂着。
                let ended = status::is_kingdom_end(text);
                let too_long = buf.lines.len() >= KINGDOM_MAX_LINES;
                let too_old = buf.started.elapsed() > KINGDOM_COLLECT_TIMEOUT;
                if ended || too_long || too_old {
                    if let Some(buf) = guard.take() {
                        finished = Some((buf.title, buf.lines));
                    }
                }
            }
        }

        if let Some((title, lines)) = finished {
            self.finish_kingdom(&title, &lines);
        }
    }

    fn finish_kingdom(&self, title: &str, lines: &[String]) {
        match status::parse_kingdom_block(title, lines) {
            Some(kingdom) => {
                let mut st = self.status.write();
                st.kingdom = Some(kingdom);
                st.updated_at = Some(status::now_rfc3339());
                drop(st);
                self.push_status();
            }
            None => {
                // 残缺块：只有标题没有正文。丢掉比推一个空面板好。
                debug!(bot = %self.username, "邦国信息块残缺，忽略");
            }
        }
    }

    /// 把 ECS 里的位置和维度同步进状态。
    fn sync_position(&self) {
        let Some(client) = self.client() else {
            return;
        };
        let p = client.position();
        let world = client.world_name();
        let mut st = self.status.write();
        st.pos = Some(Pos {
            x: p.x,
            y: p.y,
            z: p.z,
        });
        st.dimension = Some(normalize_dimension(&world.0.to_string()));
        st.updated_at = Some(status::now_rfc3339());
    }
}

/// 把 Azalea 的世界名换成 mineflayer 的写法。
///
/// mineflayer 给的是不带命名空间的 `overworld` / `the_nether` / `the_end`，
/// 而 Azalea 给的是 `minecraft:overworld` 这样的全名。前端按前者写的，所以
/// 在边界上转一次，让前端不需要为换实现改一行。
fn normalize_dimension(world: &str) -> String {
    let bare = world.rsplit(':').next().unwrap_or(world);
    bare.to_string()
}

/// 位置是否落在某个区块内。
fn in_chunk(pos: Vec3, chunk_x: i32, chunk_z: i32) -> bool {
    const CHUNK: f64 = 16.0;
    let cx = (pos.x / CHUNK).floor() as i32;
    let cz = (pos.z / CHUNK).floor() as i32;
    cx == chunk_x && cz == chunk_z
}

fn horizontal_distance(pos: Vec3, x: i32, z: i32) -> f64 {
    let dx = pos.x - x as f64;
    let dz = pos.z - z as f64;
    (dx * dx + dz * dz).sqrt()
}

/// 启动一个机器人，直到连接结束才返回。
///
/// 这是个长期运行的任务：Azalea 的 `ClientBuilder::start` 会一直跑到断开，
/// 所以调用方应当把它 spawn 出去。
pub async fn run(bot: Arc<Bot>, address: String) -> anyhow::Result<()> {
    info!(bot = %bot.username, address, "正在连接");

    let builder = ClientBuilder::new()
        .set_handler(handle)
        .set_state(HandlerState { bot: Some(bot.clone()) });

    let account = Account::offline(&bot.username);
    bot.alive.store(true, Ordering::Relaxed);

    let exit = builder.start(account, address.as_str()).await;
    bot.alive.store(false, Ordering::Relaxed);
    bot.log("info", format!("连接结束: {exit:?}"));
    Ok(())
}

/// 事件回调的附带状态。
///
/// Azalea 的 handler 必须是 `fn` 指针而不是闭包（`HandleFn<S, Fut> =
/// fn(Client, Event, S) -> Fut`），所以机器人只能从 State 里拿 —— 闭包捕获
/// 在这条签名下编译不过。`Default` 是 `set_handler` 的约束要求的，而真正
/// 的那一份由 `set_state` 覆盖；默认值只是一个不会被用到的空壳。
#[derive(Clone, Component)]
pub struct HandlerState {
    bot: Option<Arc<Bot>>,
}

impl Default for HandlerState {
    fn default() -> Self {
        Self { bot: None }
    }
}

async fn handle(
    client: Client,
    event: Event,
    state: HandlerState,
) -> anyhow::Result<()> {
    // 没有机器人就没有可做的事。这个分支只在 Azalea 用默认 State 调一次
    // 回调时才会走到，正常启动路径上 state 一定带着机器人。
    let Some(bot) = state.bot else {
        return Ok(());
    };
    // 第一次拿到客户端就存下来。Azalea 在每次事件都传同一份克隆，所以这里
    // 只是把第一份留下。
    if bot.client().is_none() {
        *bot.client.write() = Some(client.clone());
    }

    match event {
        Event::Login => {
            bot.log("info", "已登录");
        }
        Event::Spawn => {
            bot.sync_position();
            bot.push_status();
            bot.log("info", format!("已进入世界: {}", describe_position(&bot)));
        }
        Event::Chat(m) => {
            // 自己的消息不会从服务端回来，所以这里收到的都是别人的或系统的。
            let (sender, content) = m.split_sender_and_content();
            let line = match sender {
                Some(who) => format!("<{who}> {content}"),
                None => content,
            };
            bot.on_chat(&line);
        }
        Event::Death(_) => {
            bot.log("warn", "死亡");
        }
        Event::Disconnect(reason) => {
            bot.log("warn", format!("断开连接: {reason:?}"));
            bot.alive.store(false, Ordering::Relaxed);
        }
        Event::ConnectionFailed(e) => {
            bot.log("error", format!("连接失败: {e}"));
            bot.alive.store(false, Ordering::Relaxed);
        }
        Event::Tick => {
            // 位置同步跟着 tick 走，但不必每 tick 都推给前端 —— 20 次/秒的
            // 状态帧除了占带宽没有意义，人眼看不出来。
            bot.sync_position();
        }
        _ => {}
    }
    Ok(())
}

fn describe_position(bot: &Bot) -> String {
    match bot.status().pos {
        Some(p) => format!("x={:.2} y={:.2} z={:.2}", p.x, p.y, p.z),
        None => "位置未知".to_string(),
    }
}

/// 发往机器人线程的指令。
///
/// 指令走通道而不是直接调用，是因为 `Bot` 里的 `Client` 只能在机器人自己
/// 那个线程上使用（Azalea 的 `Swarm` 不是 `Send`）。通道把「想做什么」送
/// 过去，由那一边执行。
#[derive(Debug)]
pub enum Command {
    /// 发送一条聊天或命令。
    Say {
        text: String,
        reply: tokio::sync::oneshot::Sender<anyhow::Result<()>>,
    },
    /// 走到某个区块的中心。
    GotoChunk {
        chunk_x: i32,
        chunk_z: i32,
        timeout: std::time::Duration,
        reply: tokio::sync::oneshot::Sender<bool>,
    },
}

/// 一个正在运行的机器人，连同停止它的开关。
pub struct BotSlot {
    pub bot: Arc<Bot>,
    /// 置位后寻路循环会中断。用于 `/stopbot`。
    pub stop: Arc<AtomicBool>,
    /// 机器人任务的句柄。
    pub task: tokio::task::JoinHandle<()>,
    /// 供外部等待任务真正结束。
    pub done: mpsc::Sender<()>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 区块判定() {
        // 区块 (0,0) 覆盖 x,z ∈ [0,16)
        assert!(in_chunk(Vec3::new(0.0, 64.0, 0.0), 0, 0));
        assert!(in_chunk(Vec3::new(15.9, 64.0, 15.9), 0, 0));
        assert!(!in_chunk(Vec3::new(16.0, 64.0, 0.0), 0, 0));
        // 负坐标要向下取整：-0.1 属于区块 -1，不是 0。
        assert!(in_chunk(Vec3::new(-0.1, 64.0, -0.1), -1, -1));
        assert!(!in_chunk(Vec3::new(-0.1, 64.0, -0.1), 0, 0));
    }

    #[test]
    fn 区块坐标换算跟服务器一致() {
        // 服务器给的方块坐标是 chunk*16+8，即区块中心。
        // 这条断言把这个换算钉住：两边算出来的中心必须落在同一个区块里。
        for (cx, cz) in [(0, 0), (12, -34), (-1, -1), (-100, 100)] {
            let bx = cx * 16 + 8;
            let bz = cz * 16 + 8;
            assert!(
                in_chunk(Vec3::new(bx as f64, 64.0, bz as f64), cx, cz),
                "区块 ({cx},{cz}) 的中心 ({bx},{bz}) 应当落在自己里面"
            );
        }
    }

    #[test]
    fn 维度名去掉命名空间() {
        assert_eq!(normalize_dimension("minecraft:overworld"), "overworld");
        assert_eq!(normalize_dimension("minecraft:the_nether"), "the_nether");
        assert_eq!(normalize_dimension("minecraft:the_end"), "the_end");
        // 没有命名空间时原样返回，不能因为 rsplit 把整串吃掉。
        assert_eq!(normalize_dimension("overworld"), "overworld");
    }
}
