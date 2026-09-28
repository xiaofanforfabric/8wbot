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

use crate::claimqueue::{ClaimQueue, QueueEvent};
use crate::protocol::{EventData, claim_event, log_event, status_event};
use crate::status::{self, Pos, Status};

/// 询问当前子服的间隔。
///
/// 与原 JS 版的 `STATUS_SERVER_MS` 默认值一致（60 秒）。子服切换不频繁，
/// 一分钟一次足够，也不会在服务器上刷出多余的指令。
const SERVER_ASK_INTERVAL: std::time::Duration = std::time::Duration::from_secs(60);

/// 位置推送的最小间隔。
///
/// 与原 JS 版的 `STATUS_POS_MS` 默认值一致（5 秒）。位置【变化】时才真正
/// 发出，所以静止的机器人完全不占带宽。
const POS_PUSH_INTERVAL: std::time::Duration = std::time::Duration::from_secs(5);

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
    ///
    /// 是 `Arc` 而不是裸的 `AtomicBool`：队列需要在自己的闭包里读它，
    /// 而那个闭包活得比构造它的栈帧长。
    alive: Arc<AtomicBool>,
    /// 邦国信息块的累积缓冲。
    kingdom: RwLock<Option<KingdomBuf>>,
    /// 已开拓的区块数，用于统计。
    claims: AtomicU64,
    /// 疆域开拓队列。
    ///
    /// 在 `Bot::new` 里建好并常驻：扩地任务是「持续的」（用户原话：开很久），
    /// 而队列自己管理 running/cursor 状态，每次提交重用同一个实例比反复
    /// 新建更简单，也让「刷新网页后还原进度」能拿到正在跑的那份快照。
    queue: Arc<ClaimQueue>,
    /// 上次发 `/server` 的时间。
    last_server_ask: parking_lot::Mutex<std::time::Instant>,
    /// 上次推送位置的时间。用于限频。
    last_pos_push: parking_lot::Mutex<std::time::Instant>,
    /// 上次推送出去的位置与维度。与当前相同就不重复推。
    last_pos_sent: parking_lot::Mutex<(Option<(i64, i64, i64)>, Option<String>)>,
    /// 是否已经报告过这次死亡。
    ///
    /// Azalea 会把 `Event::Death` 派发多次（实测同一毫秒两次、14 毫秒后
    /// 第三次）。不去重的话同一条死讯会在前端出现三遍，看起来像服务器出了
    /// 问题。重生时清掉。
    death_reported: AtomicBool,
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
        let username = username.into();

        // 队列的事件出口是个同步回调，直接投到机器人的事件总线上。
        //
        // 这里刻意【不】用 channel + 转发任务：那个转发任务需要 spawn，而
        // `Bot::new` 会在没有 `LocalSet` 的地方被调用（测试、以及将来的
        // 其它入口），`spawn_local` 在那里会 panic。回调同步执行，不依赖
        // 任何 runtime。
        let queue_events = events.clone();
        let on_queue_event: Arc<dyn Fn(QueueEvent) + Send + Sync> =
            Arc::new(move |ev: QueueEvent| {
                let _ = queue_events.send(queue_event_to_data(&ev));
            });

        let alive = Arc::new(AtomicBool::new(false));
        let alive_for_queue = alive.clone();
        let queue = ClaimQueue::new(
            on_queue_event,
            Arc::new(move || alive_for_queue.load(Ordering::Relaxed)),
        );

        Arc::new(Self {
            username,
            client: RwLock::new(None),
            status: RwLock::new(Status::default()),
            events,
            alive,
            kingdom: RwLock::new(None),
            claims: AtomicU64::new(0),
            last_server_ask: parking_lot::Mutex::new(std::time::Instant::now()),
            last_pos_push: parking_lot::Mutex::new(std::time::Instant::now()),
            last_pos_sent: parking_lot::Mutex::new((None, None)),
            death_reported: AtomicBool::new(false),
            queue,
            stop_flag: Arc::new(AtomicBool::new(false)),
        })
    }

    /// 取开拓队列。
    pub fn queue(&self) -> Arc<ClaimQueue> {
        self.queue.clone()
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

    /// 定期发 `/server` 查询当前所在子服。
    ///
    /// 原 JS 版每 60 秒发一次（`STATUS_SERVER_MS`），我漏了这一步 ——
    /// 所以 `status.server` 永远是 null，前端「所在子服」一栏一直显示
    /// 「—」。
    ///
    /// 用处不只是显示：`server` 能告诉运维机器人是在主世界、资源世界
    /// 还是卡在认证大厅（认证大厅回的是 `auth`）。排查「机器人为什么不动」
    /// 时这是第一条线索。
    fn maybe_ask_server(&self) {
        let now = std::time::Instant::now();
        let mut last = self.last_server_ask.lock();
        if now.duration_since(*last) < SERVER_ASK_INTERVAL {
            return;
        }
        *last = now;
        drop(last);

        // 只在真的连上之后问。没连上时发消息不会有回应，只是白白刷日志。
        if !self.is_alive() {
            return;
        }
        let _ = self.say("/server");
    }

    /// 位置变化时才推状态帧。
    ///
    /// 每 tick 推是浪费 —— 服务器 20 tick/秒，而人眼和地图都不需要那个
    /// 频率。原 JS 版是 5 秒采一次、变化了才推，这里保持一致。
    ///
    /// **坐标取整**（与原 JS 的 `Math.floor` 一致）：不取整的话浮点末位
    /// 一直在抖，等价于「永远在变化」，限频就白做了；而且地图上显示
    /// `-6991.041460280879` 也没有意义。
    fn maybe_push_position(&self) {
        let now = std::time::Instant::now();
        if now.duration_since(*self.last_pos_push.lock()) < POS_PUSH_INTERVAL {
            return;
        }
        *self.last_pos_push.lock() = now;

        let (pos, dim) = {
            let st = self.status.read();
            (
                st.pos
                    .map(|p| (p.x.floor() as i64, p.y.floor() as i64, p.z.floor() as i64)),
                st.dimension.clone(),
            )
        };

        // 位置和维度都没变就不推 —— 静止的机器人完全不占带宽。
        {
            let mut last = self.last_pos_sent.lock();
            if *last == (pos, dim.clone()) {
                return;
            }
            *last = (pos, dim);
        }

        self.push_status();
    }

    /// 推一条「已离线」事件。
    ///
    /// 这是前端和 Go 判断机器人掉线的**唯一**信号 —— 它们不看 `online`
    /// 字段的持续状态，只看这条一次性事件：
    ///
    /// - 前端 `ConsolePage.vue` 收到它就把状态置为离线、禁用聊天输入框
    /// - `botStream.js` 收到它就把该机器人标记为 offline
    /// - Go 的 `handleNodeEvent` 收到它就推给浏览器
    ///
    /// 我一开始漏了它。后果不只是「掉线时没提示」：前端 `botOnline` 的初值
    /// 是 true 且只由这条事件驱动置 false，所以漏掉它意味着界面【永远显示
    /// 在线】，机器人真掉线了用户也看不出来。
    fn push_offline(&self, reason: &str) {
        let mut m = EventData::new();
        m.insert("bot_offline".into(), true.into());
        m.insert("reason".into(), reason.to_string().into());
        let _ = self.events.send(m);
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
        // 连续多少次「没在寻路」就判定寻路已结束。
        //
        // 这是必要的：寻路器找不到路时会自己停下（天空岛四周是虚空，目标
        // 不可达时的正常结果），而如果只等超时，队列就会为一块根本走不到
        // 的地卡满整个超时（默认 300 秒），用户看到的是「不动了但还在扩地
        // 中」。等 2 秒是为了容错 —— 寻路在两次重规划之间会有短暂的空隙。
        let mut idle_ticks = 0;
        const IDLE_TICKS_TO_GIVE_UP: u32 = 8;

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

            // 死亡会打断寻路，重生后又落在别处 —— 这时继续等下去毫无意义，
            // 因为寻路器的路径是基于死前的位置算的。直接放弃这一块，让队列
            // 去处理下一块（用户在天空岛上明确接受「摔了就重生继续」）。
            if self.death_reported.load(Ordering::Relaxed) {
                self.log("warn", "死亡打断了寻路，放弃这一块");
                break;
            }

            // 寻路器还在干活 = 正在算路径，或者正在走一条路径。
            //
            // 两个都要看：只检查「在走」的话，重规划期间（正在算、还没开始
            // 走）会被误判成已放弃。
            if client.is_executing_path() || client.is_calculating_path() {
                idle_ticks = 0;
            } else {
                idle_ticks += 1;
                if idle_ticks >= IDLE_TICKS_TO_GIVE_UP {
                    self.log(
                        "warn",
                        format!("寻路已停止（未能到达 ({chunk_x}, {chunk_z})），放弃这一块"),
                    );
                    break;
                }
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

/// 把队列事件转成推给前端的一帧。
///
/// **必须包在 `expand` 字段里**：Go 后端的 `handleNodeEvent` 只认
/// `data[].expand`（见 `bots.go`），其余字段一律忽略。我一开始用了
/// `queue`/`claimed`/`failed` 三个平铺字段，那样 Go 收不到任何扩地进度，
/// 而前端会一直显示「扩地中」直到超时 —— 没有报错，只是永远不更新。
///
/// 里面用 `type` 区分子类型，字段名与 JS 版一致，前端不用改解析。
fn queue_event_to_data(ev: &QueueEvent) -> EventData {
    let mut m = EventData::new();
    let payload = match ev {
        QueueEvent::Progress(snap) => {
            let mut v = serde_json::to_value(snap).unwrap_or_default();
            if let Some(obj) = v.as_object_mut() {
                obj.insert("type".into(), "progress".into());
            }
            v
        }
        QueueEvent::Claimed { cx, cz } => {
            serde_json::json!({"type": "claimed", "cx": cx, "cz": cz})
        }
        QueueEvent::Failed { cx, cz, reason } => {
            serde_json::json!({"type": "failed", "cx": cx, "cz": cz, "reason": reason.as_str()})
        }
        QueueEvent::Log(text) => {
            serde_json::json!({"type": "log", "msg": text})
        }
    };
    m.insert("expand".into(), payload);
    m
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
            // 重生也会触发 Spawn，清掉死亡标志 —— 否则复活之后再死一次
            // 就报不出来了。
            bot.death_reported.store(false, Ordering::Relaxed);
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
            // Azalea 会把 Death 派发多次（实测同一毫秒两次、14 毫秒后第三
            // 次），所以这里去重。
            //
            // 不能用「health 从正变零」来判断：Death 到达时 health 已经是 0
            // 了，那个条件永远不成立。用一个显式标志，重生时清掉。
            if !bot.death_reported.swap(true, Ordering::Relaxed) {
                bot.log("warn", "死亡");
            }
        }
        Event::Disconnect(reason) => {
            bot.log("warn", format!("断开连接: {reason:?}"));
            bot.alive.store(false, Ordering::Relaxed);
            bot.push_offline(&describe_reason(&reason));
        }
        Event::ConnectionFailed(e) => {
            bot.log("error", format!("连接失败: {e}"));
            bot.alive.store(false, Ordering::Relaxed);
            bot.push_offline(&e.to_string());
        }
        Event::Tick => {
            // 位置同步跟着 tick 走（内存里保持最新，供 botstatus 与扩地
            // 排序用），但**推送**要限频。
            //
            // 原来这里只 sync 不 push，导致 pos 在内存里是新的、却从来没
            // 发给任何人 —— 前端的实时地图因此永远拿不到坐标。我当时写的
            // 注释是「不必每 tick 都推」，实现出来却成了「永不推」。
            //
            // 现在按原 JS 版的节奏来：每 5 秒看一次，位置或维度【变了】
            // 才推。20 次/秒的状态帧除了占带宽没有意义，而地图需要的是
            // 「移动时能看到它在动」。
            bot.sync_position();
            bot.maybe_push_position();
            bot.maybe_ask_server();
        }
        _ => {}
    }
    Ok(())
}

/// 把断开原因转成一句人能读的话。
///
/// 直接 `{reason:?}` 会把整个 Rust 结构体打出来 —— 实测推给前端的是
/// 800 多字符的 `Some(Text(TextComponent { base: BaseComponent {
/// siblings: [], style: Style { color: Some(TextColor { value: 16733525, ...
/// 而真正的信息只有末尾那句「您已经连接到此代理服务器了！」。
///
/// 用户在前端的掉线提示里看到那一大坨是什么都判断不出来的。
fn describe_reason<T: std::fmt::Display>(reason: &Option<T>) -> String {
    match reason {
        // `FormattedText` 的 Display 输出纯文本，正是用户要看的
        // （例如「您已经连接到此代理服务器了！」）。
        //
        // 写成泛型而不是具体类型：`FormattedText` 的公开路径在
        // `azalea_chat::component` 里，而这个 crate 只直接依赖 `azalea`。
        // 泛型让调用点的类型推导完成这件事，不用把 azalea-chat 提成显式
        // 依赖 —— 那个依赖的版本必须和 azalea 内部用的一致，写死反而脆。
        Some(text) => text.to_string(),
        None => "服务器未给出原因".to_string(),
    }
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
    /// 开始跑已装载的扩地队列。
    ///
    /// 队列本身在 `Bot` 上（`submit` 已把区块装载好），这一条只是让机器人
    /// 线程去 `run` 它 —— 因为队列的执行要调寻路，而寻路只能在机器人自己
    /// 的线程上操作 Azalea 的客户端。
    RunQueue {
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
