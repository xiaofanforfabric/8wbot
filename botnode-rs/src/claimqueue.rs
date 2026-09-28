//! 疆域开拓队列。
//!
//! 移植自 `8wbotforJavaScript/claimqueue.js`（而它又是从 xiaofanbot 的
//! `ClaimQueue.java` 移植来的）。逻辑本身与语言无关，所以这里是等价的
//! 重新实现，而不是重新设计 —— 排序策略、超时、状态机都保持一致，这样
//! 前端和运维的心智模型不用改。
//!
//! 与「全自动扩地」的区别（沿用原注释）：区块列表由网页下发，机器人只负责
//! 按顺序走过去。区块的开拓由服务器自动完成 —— **走过去即开拓**，机器人靠
//! 聊天通知确认结果。
//!
//! 工作流：
//!   1. 网页下发 `{ chunks, occupied }`
//!   2. [`order_square`] 按「接壤优先 + 就近」排序，避免来回横跳
//!   3. 逐块：寻路到区块中心 → 等到达 → 等服务器确认「已为你的邦国开拓」
//!   4. 全程回报进度，可随时中断

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use crate::status::Claim;

/// 单块超时。
///
/// 到达目标与等待服务器通知都用它，与参考实现一致（原值是 300 秒）。
pub const CHUNK_TIMEOUT: Duration = Duration::from_secs(300);

/// 等待服务器确认时的轮询间隔。
///
/// 用轮询而不是事件回调，是因为开拓通知从机器人线程的聊天处理里进来，而队列
/// 在另一个地方跑。共享一个 `HashSet` 加轮询，比拉一条通知通道简单，且这里
/// 的延迟要求是秒级 —— 用户看到的是「走到了 → 开拓了」。
const NOTIFY_POLL: Duration = Duration::from_millis(500);

/// 把区块坐标打包成集合的键。
///
/// 原实现用字符串 `"cx,cz"`，但那在 Rust 里要分配。区块数量是几百到几千，
/// 用 `(i32, i32)` 元组直接哈希没有任何问题，而且不用分配。
type ChunkKey = (i32, i32);

/// 扩地顺序：接壤优先 + 越方越好。
///
/// 贪心算法，与原实现一致：
///   1. 以「已占区块集合」为起点（网页传来的 `occupied`，外加玩家当前区块作
///      种子，保证第一块一定贴着自己扩）
///   2. 每步从待扩集合里选：与已占集合接壤数最多的（4 > 3 > 2 > 1）；并列时
///      选离上一格最近的，避免跳来跳去
///   3. 选中后并入已占集合，继续下一步 —— 自然形成贴边向外的方正形状
///
/// 这个顺序不是可有可无的优化：乱序会让机器人来回横穿整个选区，而「持续开
/// 很久」的扩地模式下，那意味着几倍的移动包和几倍的被反作弊盯上的机会。
pub fn order_square(
    cells: &[(i32, i32)],
    occupied: &[(i32, i32)],
    player_chunk: Option<(i32, i32)>,
) -> Vec<(i32, i32)> {
    if cells.is_empty() {
        return Vec::new();
    }

    let mut occ: HashSet<ChunkKey> = occupied.iter().copied().collect();
    if let Some(pc) = player_chunk {
        occ.insert(pc);
    }

    // 待扩集合。用 HashMap 而不是 HashSet：要按原顺序遍历作为兜底，
    // 而 HashSet 的遍历顺序不确定，会让「绝不丢块」的兜底分支行为不可预期。
    let mut remain: HashMap<ChunkKey, (i32, i32)> =
        cells.iter().map(|c| (*c, *c)).collect();

    // 起点：离玩家最近的格子，避免一上来横穿整个选区。
    let (pcx, pcz) = player_chunk.unwrap_or((0, 0));
    let mut cur = remain
        .values()
        .min_by_key(|c| (c.0 - pcx).abs() as i64 + (c.1 - pcz).abs() as i64)
        .copied();

    if cur.is_none() {
        return Vec::new();
    }

    let mut ordered = Vec::with_capacity(remain.len());

    while !remain.is_empty() {
        let current = cur.expect("循环内 cur 一定有值");

        // 选接壤最多、并列时最近的。
        //
        // 显式遍历并保留第一个最优解，而不是用 `max_by_key`：后者在并列时
        // 返回最后一个，而原实现取的是第一个 —— 那会让排序结果在并列情况下
        // 与 JS 版不一致，前端看到的顺序就变了。
        let mut best: Option<(ChunkKey, i32, i64)> = None;
        for (&key, &c) in remain.iter() {
            let nb = neighbors_in(&occ, c.0, c.1);
            let dist = (c.0 - current.0).abs() as i64 + (c.1 - current.1).abs() as i64;

            let take = match best {
                None => true,
                Some((_, best_nb, best_dist)) => nb > best_nb || (nb == best_nb && dist < best_dist),
            };
            if take {
                best = Some((key, nb, dist));
            }
        }

        let Some((key, _, _)) = best else {
            // 兜底：选不出来就把剩下的按原顺序追加，绝不丢块。
            //
            // 「丢块」在这里是严重故障：用户框选了 100 块，机器人才扩了 90 块
            // 就报完成，而失败信息不会出现 —— 因为从队列的角度它就是跑完了。
            let mut rest: Vec<(i32, i32)> = remain.values().copied().collect();
            rest.sort_by_key(|c| (c.0, c.1)); // 稳定顺序，便于测试与复现
            ordered.extend(rest);
            break;
        };

        let chosen = remain.remove(&key).expect("刚遍历过，一定在");
        occ.insert(key);
        ordered.push(chosen);
        cur = Some(chosen);
    }

    ordered
}

/// 与已占集合接壤的边数（四邻，不含对角）。
fn neighbors_in(occ: &HashSet<ChunkKey>, cx: i32, cz: i32) -> i32 {
    let mut n = 0;
    if occ.contains(&(cx + 1, cz)) {
        n += 1;
    }
    if occ.contains(&(cx - 1, cz)) {
        n += 1;
    }
    if occ.contains(&(cx, cz + 1)) {
        n += 1;
    }
    if occ.contains(&(cx, cz - 1)) {
        n += 1;
    }
    n
}

/// 队列对外报告的事件。
///
/// 形状与 JS 版一致，前端不用改解析。
#[derive(Debug, Clone)]
pub enum QueueEvent {
    /// 进度快照。
    Progress(Snapshot),
    /// 某块开拓成功。
    Claimed { cx: i32, cz: i32 },
    /// 某块失败。
    Failed { cx: i32, cz: i32, reason: FailReason },
    /// 一条给用户看的日志。
    Log(String),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FailReason {
    /// 没走到目标。
    NotArrived,
    /// 走到了，但服务器没发开拓通知。
    NotConfirmed,
}

impl FailReason {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::NotArrived => "未到达目标",
            Self::NotConfirmed => "未收到服务器开拓通知",
        }
    }
}

/// 队列状态快照，推给前端。
#[derive(Debug, Clone, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Snapshot {
    pub running: bool,
    /// 给用户看的当前状态文字。
    pub task: String,
    pub total: usize,
    pub done: usize,
    pub remaining: usize,
    /// 当前正在处理的区块。
    pub current: Option<(i32, i32)>,
    /// 剩余待扩区块，网页刷新后用它还原进度。
    ///
    /// `camelCase` 是为了和 JS 版的 `remainingChunks` 一致 —— 前端读的就是
    /// 那个名字，改成分隔线命名会让它在刷新后拿不到剩余列表。
    pub remaining_chunks: Vec<(i32, i32)>,
}

/// 开拓队列。
///
/// 一个机器人一个。所有可变状态在一个锁里 —— 队列的操作都很短（排序、
/// 推进游标），不值得为更细的粒度增加复杂度。
pub struct ClaimQueue {
    inner: parking_lot::Mutex<QueueState>,
    /// 服务器已确认开拓的区块。聊天监听往里塞，`_run` 循环读。
    claimed: parking_lot::Mutex<HashSet<ChunkKey>>,
    /// 事件出口。
    ///
    /// 用回调而不是 channel：channel 需要一个消费它的任务，而那个任务必须
    /// 跑在某个 runtime 上（`spawn` 或 `spawn_local`）。`ClaimQueue` 会被
    /// 在 `LocalSet` 里和不在 `LocalSet` 里的地方都构造出来，一旦内部藏了
    /// `spawn_local`，前者能跑后者 panic —— 这个坑在项目里踩过两次了。
    ///
    /// 回调是同步的，调用方在自己的上下文里决定怎么处理，队列本身不需要
    /// runtime。副作用是回调里不能 await（要等通知仍然走 `wait_notify` 的
    /// 轮询），而队列本来也不需要。
    on_event: Arc<dyn Fn(QueueEvent) + Send + Sync>,
    /// 是否还能干活（在线、未销毁）。由机器人层提供。
    is_usable: Arc<dyn Fn() -> bool + Send + Sync>,
}

struct QueueState {
    queue: Vec<(i32, i32)>,
    cursor: usize,
    running: bool,
    current: Option<(i32, i32)>,
    /// 已占集合，随开拓进度实时扩充，供后续接壤判断。
    occupied: HashSet<ChunkKey>,
}

impl ClaimQueue {
    /// 建一个队列。
    ///
    /// `is_usable` 是个闭包而不是 `Bot` 的引用：队列只需要知道「还能不能
    /// 干活」，把整个机器人传进来会让它持有比需要更多的依赖，也让测试必须
    /// 造一个真机器人。
    pub fn new(
        on_event: Arc<dyn Fn(QueueEvent) + Send + Sync>,
        is_usable: Arc<dyn Fn() -> bool + Send + Sync>,
    ) -> Arc<Self> {
        Arc::new(Self {
            inner: parking_lot::Mutex::new(QueueState {
                queue: Vec::new(),
                cursor: 0,
                running: false,
                current: None,
                occupied: HashSet::new(),
            }),
            claimed: parking_lot::Mutex::new(HashSet::new()),
            on_event,
            is_usable,
        })
    }

    /// 当前状态快照。
    pub fn snapshot(&self) -> Snapshot {
        let st = self.inner.lock();
        let remaining_chunks = st.queue[st.cursor.min(st.queue.len())..].to_vec();
        Snapshot {
            running: st.running,
            task: if st.running { "扩地中" } else { "空闲" }.to_string(),
            total: st.queue.len(),
            done: st.cursor,
            remaining: remaining_chunks.len(),
            current: st.current,
            remaining_chunks,
        }
    }

    /// 聊天监听调用：某区块已被服务器确认开拓。
    ///
    /// 立刻并入已占集合，后续接壤判断才能把新开拓的算进去。
    pub fn mark_claimed(&self, cx: i32, cz: i32) {
        self.claimed.lock().insert((cx, cz));
        self.inner.lock().occupied.insert((cx, cz));
    }

    /// 中断当前任务。返回是否真的中断了一个在跑的任务。
    pub fn stop(&self) -> bool {
        let mut st = self.inner.lock();
        if !st.running {
            return false;
        }
        st.running = false;
        true
    }

    /// 提交一批区块。只装载状态，**不启动执行**。
    ///
    /// 返回是否装载成功（空队列算失败）。
    ///
    /// 为什么不在这里启动：执行要跑在机器人线程的 `LocalSet` 上（`run` 里
    /// 有 `!Send` 的东西，而且队列本来就是绕着机器人转的），而 `submit`
    /// 会被控制面调用 —— 控制面不在那个 `LocalSet` 里。在这里调
    /// `spawn_local` 会 panic，这个坑在 `manager.rs` 的文件头里记着。
    ///
    /// 所以拆成两步：`submit` 装载，由机器人线程 await 一个 `run` 得来的
    /// future。`run` 本身是普通 async fn，调用方决定在哪儿跑它。
    pub fn submit(&self, ordered: Vec<(i32, i32)>, occupied: Vec<(i32, i32)>) -> bool {
        self.stop();

        let mut st = self.inner.lock();
        st.queue = ordered;
        st.cursor = 0;
        st.current = None;
        st.occupied = occupied.into_iter().collect();

        if st.queue.is_empty() {
            drop(st);
            (self.on_event)(QueueEvent::Log(
                "扩地队列为空：排序后无可用区块，请检查选区".to_string(),
            ));
            return false;
        }

        st.running = true;
        drop(st);

        self.emit_progress();
        true
    }

    fn emit_progress(&self) {
        (self.on_event)(QueueEvent::Progress(self.snapshot()));
    }

    /// 主循环。由调用方在机器人线程上跑（`submit` 之后 await 它）。
    ///
    /// 不在这个函数里 `spawn` —— 见 [`ClaimQueue::submit`] 的说明。
    pub async fn run<F, Fut>(self: Arc<Self>, walk: F)
    where
        F: Fn(i32, i32) -> Fut,
        Fut: std::future::Future<Output = bool>,
    {
        loop {
            let (cx, cz) = {
                let mut st = self.inner.lock();
                if !st.running || st.cursor >= st.queue.len() {
                    break;
                }
                let c = st.queue[st.cursor];
                st.current = Some(c);
                c
            };

            self.emit_progress();

            if !(self.is_usable)() {
                (self.on_event)(QueueEvent::Log("机器人已离线，扩地中止".to_string()));
                break;
            }

            // 先清掉可能残留的旧通知，避免上一块的确认被这一块误收。
            //
            // 这是必要的：服务器有时会在机器人离开区块后才补发通知，那条会
            // 落进集合里。不清的话，下一块刚走到就"确认"了，而实际上服务器
            // 根本没认。
            self.claimed.lock().remove(&(cx, cz));

            let arrived = walk(cx, cz).await;

            if !self.is_running() {
                // 期间被 stop 了，不再等通知。
                let mut st = self.inner.lock();
                st.current = None;
                break;
            }

            let confirmed = if arrived {
                self.wait_notify(cx, cz).await
            } else {
                false
            };

            {
                let mut st = self.inner.lock();
                st.cursor += 1;
                st.current = None;
            }

            if confirmed {
                (self.on_event)(QueueEvent::Claimed { cx, cz });
            } else {
                (self.on_event)(QueueEvent::Failed {
                    cx,
                    cz,
                    reason: if arrived {
                        FailReason::NotConfirmed
                    } else {
                        FailReason::NotArrived
                    },
                });
            }
            self.emit_progress();
        }

        let processed = {
            let mut st = self.inner.lock();
            let d = st.cursor;
            st.running = false;
            st.current = None;
            d
        };
        // 收尾再推一次状态，让网页把任务显示为「空闲」。
        self.emit_progress();
        (self.on_event)(QueueEvent::Log(format!("扩地结束，共处理 {processed} 个区块")));
    }

    fn is_running(&self) -> bool {
        self.inner.lock().running
    }

    /// 等服务器聊天通知，最多 [`CHUNK_TIMEOUT`]。
    async fn wait_notify(&self, cx: i32, cz: i32) -> bool {
        let deadline = tokio::time::Instant::now() + CHUNK_TIMEOUT;
        loop {
            if !self.is_running() {
                return false;
            }
            {
                let mut claimed = self.claimed.lock();
                if claimed.remove(&(cx, cz)) {
                    return true;
                }
            }
            if tokio::time::Instant::now() >= deadline {
                return false;
            }
            tokio::time::sleep(NOTIFY_POLL).await;
        }
    }
}

/// 从一条聊天消息里解析开拓通知。
///
/// 这是 [`crate::status::parse_claim`] 的别名，留在这里是因为队列的使用者
/// 会从聊天流里调它，而 `status` 模块的名字对它来说不够直白。
pub fn parse_claim_notify(text: &str) -> Option<Claim> {
    crate::status::parse_claim(text)
}

/// 一个原子的「已停止」标志，用于外部打断长时间操作。
///
/// 独立于队列自己的 `running`：队列的 `running` 在锁里，而这个可以随处
/// 携带（寻路循环就持有一个）。
pub type StopFlag = Arc<AtomicBool>;

/// 造一个停止标志。
pub fn stop_flag() -> StopFlag {
    Arc::new(AtomicBool::new(false))
}

/// 读取停止标志。
pub fn is_stopped(flag: &StopFlag) -> bool {
    flag.load(Ordering::Relaxed)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 空输入返回空() {
        assert!(order_square(&[], &[], None).is_empty());
    }

    #[test]
    fn 单块直接返回() {
        let r = order_square(&[(5, 5)], &[], None);
        assert_eq!(r, vec![(5, 5)]);
    }

    #[test]
    fn 起点是离玩家最近的() {
        // 玩家在 (0,0)，待扩集合里 (1,1) 比 (9,9) 近，所以先走 (1,1)。
        let cells = vec![(9, 9), (1, 1)];
        let r = order_square(&cells, &[], Some((0, 0)));
        assert_eq!(r[0], (1, 1), "第一块必须是最靠近玩家的");
    }

    #[test]
    fn 接壤优先() {
        // 已占 (0,0)。待扩里 (1,0) 与它接壤（1 条边），(5,5) 不接壤。
        // 接壤的必须先走 —— 这就是「贴着边向外扩」而不是「跳着扩」。
        let cells = vec![(5, 5), (1, 0)];
        let r = order_square(&cells, &[(0, 0)], None);
        assert_eq!(r[0], (1, 0), "接壤的必须优先");
    }

    #[test]
    fn 接壤数多的优先() {
        // 已占 (0,0) 和 (1,0)。(0,1) 与 (0,0) 接壤、与 (1,0) 对角不接壤 → 1 条。
        // (2,0) 与 (1,0) 接壤 → 1 条。
        // 加一个 (1,1)：与 (0,1)? 不在已占；与 (1,0) 接壤 → 1 条。
        // 换成能明确分出的情形：已占含 (0,0)(1,0)(2,0)，
        // 待扩 (1,1) 与 (1,0) 接壤 → 1 条；
        //      (0,1) 与 (0,0) 接壤 → 1 条。都是 1，选最近的。
        // 用 (1,-1)：与 (1,0) 接壤 → 1 条。仍都是 1。
        // 真正能分出的是「角」：已占 (0,0)(1,0)(0,1)，
        // 待扩 (1,1) 与 (1,0) 和 (0,1) 都接壤 → 2 条；(5,5) → 0 条。
        let occ = vec![(0, 0), (1, 0), (0, 1)];
        let cells = vec![(5, 5), (1, 1)];
        let r = order_square(&cells, &occ, Some((0, 0)));
        assert_eq!(r[0], (1, 1), "接壤 2 条的要先于接壤 0 条的");
    }

    #[test]
    fn 并列时选最近的() {
        // 已占 (0,0)。待扩 (1,0) 和 (0,1) 都与它接壤 1 条。
        // 玩家在 (10,0)，所以 cur 初始是 (1,0)（更近）。
        // 然后从剩下的 (0,1) 里选 —— 只有一个，直接选它。
        // 要看「并列时选最近」需要一个更构造的例子：
        // 已占 (0,0)。待扩 (1,0)、(0,1)、(-1,0)，都与 (0,0) 接壤 1 条。
        // 玩家在 (5,0) → 第一块选 (1,0)。之后 cur=(1,0)，
        // 剩下 (0,1) 距离 1+1=2、(-1,0) 距离 2+0=2 —— 仍并列。
        // 所以用玩家位置把 cur 定到 (0,1) 那一侧：(0,5)。
        let occ = vec![(0, 0)];
        let cells = vec![(1, 0), (0, 1), (-1, 0)];
        let r = order_square(&cells, &occ, Some((0, 5)));
        assert_eq!(r[0], (0, 1), "玩家在上方，第一块应当是 (0,1)");
        // 第二块：cur=(0,1)。(-1,0) 距离 1+1=2，(1,0) 距离 1+1=2。并列。
        // 并列时保留先遇到的 —— HashMap 顺序不定，所以这里不断言具体是哪个，
        // 只断言三块都在（不丢块）。
        assert_eq!(r.len(), 3, "绝不能丢块");
        assert_eq!(r[0], (0, 1));
    }

    #[test]
    fn 绝不丢块() {
        // 这是最关键的性质：用户框选了 N 块，队列必须恰好处理 N 块。
        // 丢块会让「完成」变成假消息 —— 从队列视角它确实跑完了。
        let cells: Vec<(i32, i32)> = (0..50).map(|i| (i * 3, i * 7)).collect();
        let r = order_square(&cells, &[(0, 0)], Some((0, 0)));
        assert_eq!(r.len(), 50, "排序后数量必须与输入一致");
        let mut sorted_in = cells.clone();
        let mut sorted_out = r.clone();
        sorted_in.sort();
        sorted_out.sort();
        assert_eq!(sorted_in, sorted_out, "内容必须完全一致，不能有增减");
    }

    #[test]
    fn 不重复() {
        let cells = vec![(1, 1), (2, 2), (1, 1), (3, 3)];
        let r = order_square(&cells, &[], None);
        // 输入里有重复 —— 原实现用 Map 去重，这里用 HashMap 也一样。
        assert_eq!(r.len(), 3, "重复的区块应当只出现一次，实际: {r:?}");
    }

    #[test]
    fn 玩家区块自动并入已占() {
        // 玩家在 (5,5) 且它不在 occupied 里时，也要当已占 —— 否则第一块可能
        // 不贴着玩家扩，机器人要横穿选区。
        let cells = vec![(9, 9), (6, 5)];
        // 不传 occupied，只传玩家位置。
        let r = order_square(&cells, &[], Some((5, 5)));
        // (6,5) 与玩家区块接壤 → 优先。
        assert_eq!(r[0], (6, 5));
    }

    #[test]
    fn 接壤边数计算() {
        let mut occ = HashSet::new();
        occ.insert((1, 0));
        occ.insert((-1, 0));
        assert_eq!(neighbors_in(&occ, 0, 0), 2);
        // 对角不算接壤 —— 对角相邻的两块只有角接触，走出去要绕路，
        // 把它算成接壤会让排序偏好错误的形状。
        occ.clear();
        occ.insert((1, 1));
        assert_eq!(neighbors_in(&occ, 0, 0), 0, "对角不算接壤");
        // 四邻全有。
        occ.clear();
        for k in [(1, 0), (-1, 0), (0, 1), (0, -1)] {
            occ.insert(k);
        }
        assert_eq!(neighbors_in(&occ, 0, 0), 4);
    }

    #[test]
    fn 解析开拓通知() {
        let c = parse_claim_notify("疆土 (-456, -721) [-7288, -11528] 已为你的邦国开拓！")
            .expect("应当解析");
        assert_eq!(c.chunk_x, -456);
        assert_eq!(c.chunk_z, -721);
        // 方块坐标是区块中心：-456*16+8 = -7288。
        assert_eq!(c.x, -7288);
        assert_eq!(c.z, -11528);
    }

    #[test]
    fn 停止标志() {
        let f = stop_flag();
        assert!(!is_stopped(&f));
        f.store(true, Ordering::Relaxed);
        assert!(is_stopped(&f));
    }
}
