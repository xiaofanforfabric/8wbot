//! WebSocket 服务端：实现 Go 后端的四个端点。
//!
//! 端点和报文形状逐字对应 `goservers/wsclient.go`，理由见 `protocol.rs`。
//! 这里是它们落地的地方：
//!
//! - `/ws/api/startbot`   启动，连接保持打开用于推日志
//! - `/ws/api/stopbot`    停止
//! - `/ws/api/botstatus`  查询状态
//! - `/ws/api/botlogs`    订阅日志流
//! - `/ws/api/sendinfo`   下发指令（当前只有聊天）
//!
//! 一个必须遵守的细节（原 JS 版在注释里记过这个坑）：**监听器只挂一次**。
//! 早期实现是每来一个 `/botlogs` 连接就往 bot 上重挂一遍 `bot.on(...)` 且
//! close 时不摘除，于是开 N 个控制台就挂 5N 个监听器，同一条聊天被转发 N 次，
//! 内存只涨不降。这里改成订阅广播通道，订阅者退出时接收端自然 drop。

use anyhow::Result;
use axum::Router;
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::State;
use axum::response::IntoResponse;
use axum::routing::get;
use futures_util::SinkExt;
use std::collections::HashSet;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::broadcast;
use tokio::sync::broadcast::error::RecvError;
use tracing::{debug, info, warn};

use crate::manager::{Manager, StartError, StopError};
use crate::protocol::{
    BotRequest, Command, EventData, EventFrame, EventsRequest, ExpandRequest, Response,
    SendInfoRequest, status_event,
};

/// 事件流重新扫描运行列表的间隔。
///
/// 一秒是个折中：新启动的机器人最多一秒后出现在管理面板里（人眼察觉不到），
/// 而每秒遍历一次几十个名字的开销可以忽略。
const RESCAN_INTERVAL: Duration = Duration::from_secs(1);

/// 共享给所有路由的状态。
#[derive(Clone)]
pub struct AppState {
    pub manager: Arc<Manager>,
    /// 走一个区块的默认超时。
    pub walk_timeout: Duration,
    /// 内部节点密钥。为空表示不校验。
    ///
    /// 这个节点会在公网（`js.xiaofanai.uk`）后面，而它的接口能启停机器人、
    /// 能代替它们说话 —— 不校验等于把控制权开放给任何人。Go 后端通过
    /// `?secret=` 或 `X-Internal-Secret` 头带上它。
    ///
    /// 留「为空则不校验」这条路是为了本地开发方便，但启动时会打一条警告：
    /// 静默地不校验是最危险的默认值。
    pub secret: Option<String>,
}

impl AppState {
    pub fn new(manager: Arc<Manager>) -> Self {
        Self {
            manager,
            walk_timeout: Duration::from_secs(180),
            secret: std::env::var("INTERNAL_NODE_SECRET")
                .ok()
                .filter(|s| !s.is_empty()),
        }
    }

    /// 构造一个不校验的实例。测试用。
    pub fn insecure(manager: Arc<Manager>) -> Self {
        Self {
            manager,
            walk_timeout: Duration::from_secs(180),
            secret: None,
        }
    }

    /// 校验一次请求携带的密钥。
    ///
    /// 未配置密钥时一律放行 —— 见字段说明。
    pub fn authorized(&self, provided: Option<&str>) -> bool {
        match &self.secret {
            None => true,
            Some(expected) => match provided {
                Some(got) => constant_time_eq(got.as_bytes(), expected.as_bytes()),
                None => false,
            },
        }
    }
}

/// 定长时间比较。
///
/// 用 `==` 比较密钥会在第一个不同的字节处提前返回，攻击者能靠响应时间一个
/// 字节一个字节地试出来。这里的循环长度只取决于长度，不取决于内容。
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// 读一条文本消息并解析成 `T`。
///
/// 返回 `Ok(None)` 表示对端关闭或发来非文本帧 —— 两种都是「没有请求」，
/// 而不是错误。
async fn read_json<T: serde::de::DeserializeOwned>(ws: &mut WebSocket) -> Result<Option<T>> {
    loop {
        match ws.recv().await {
            Some(Ok(Message::Text(text))) => {
                let parsed = serde_json::from_str(&text)?;
                return Ok(Some(parsed));
            }
            // 协议只用文本。二进制帧按协议违规处理，回一个错误比静默忽略好
            // —— 静默会让调用方一直等一个不会来的回复。
            Some(Ok(Message::Binary(_))) => return Ok(None),
            Some(Ok(Message::Close(_))) | None => return Ok(None),
            Some(Ok(_)) => continue, // ping/pong
            Some(Err(e)) => return Err(e.into()),
        }
    }
}

/// 发一条 JSON。
async fn send_json<S>(ws: &mut S, value: &impl serde::Serialize) -> Result<()>
where
    S: SinkExt<Message> + Unpin,
    S::Error: std::error::Error + Send + Sync + 'static,
{
    let text = serde_json::to_string(value)?;
    ws.send(Message::Text(text.into())).await?;
    Ok(())
}

/// 组装路由。
pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/ws/api/startbot", get(startbot))
        .route("/ws/api/stopbot", get(stopbot))
        .route("/ws/api/botstatus", get(botstatus))
        .route("/ws/api/botlogs", get(botlogs))
        .route("/ws/api/sendinfo", get(sendinfo))
        .route("/ws/api/expand", get(expand))
        .route("/ws/api/events", get(events))
        .with_state(state)
}

/// 从升级请求的查询串和头部里取密钥。
///
/// 两种都要支持：Go 后端用的是 `?secret=`，而手写测试和将来的服务间调用
/// 更可能用头。
fn extract_secret(headers: &axum::http::HeaderMap, uri: &axum::http::Uri) -> Option<String> {
    headers
        .get("x-internal-secret")
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string())
        .or_else(|| {
            uri.query().and_then(|q| {
                q.split('&').find_map(|pair| {
                    let (k, v) = pair.split_once('=')?;
                    (k == "secret").then(|| urldecode(v))
                })
            })
        })
}

/// 鉴权失败时的收尾：先发一帧 `{"code":401}`，再以 4001 关闭。
///
/// 为什么不在中间件里直接回 HTTP 401：那样更干净，但 Go 侧读的是 WS 帧里的
/// `code`（`wsclient.go` 的 `startResp["code"].(float64)`），看到的是
/// Dial 失败，于是把「密钥不对」报成「节点不可达」。原 JS 节点就是发帧的
/// 写法，这里保持一致，排障时才分得清是哪种故障。
async fn reject_unauthorized(mut ws: WebSocket) {
    let body = crate::protocol::Response::unauthorized(
        "Unauthorized: invalid or missing internal secret",
    );
    let text = serde_json::to_string(&body).unwrap_or_else(|_| r#"{"code":401}"#.to_string());
    let _ = ws.send(Message::Text(text.into())).await;
    let _ = ws.close().await;
}

/// 一个极小的百分号解码。
///
/// 只处理 `%XX` —— 密钥里可能出现的字符（`+` 会被编码成 `%2B`）就够用了，
/// 为此引入一个 URL 库不值得。
fn urldecode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            let hex = std::str::from_utf8(&bytes[i + 1..i + 3]).ok();
            if let Some(b) = hex.and_then(|h| u8::from_str_radix(h, 16).ok()) {
                out.push(b);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

// ════════════════════════════════════════════════════════════════
// /ws/api/startbot
// ════════════════════════════════════════════════════════════════

/// 启动机器人，然后把这条连接当成它的日志订阅。
///
/// Go 侧在收到应答后**不关闭**这条连接，后续该 bot 的日志推给它（launcher
/// 语义）。所以这里应答之后继续转发事件，直到对端断开。
async fn startbot(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 startbot 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_startbot(socket, state).await {
            debug!("startbot 连接结束: {e}");
        }
    })
}

async fn handle_startbot(mut ws: WebSocket, state: AppState) -> Result<()> {
    // Go 侧拨通后会先等 3 秒读一条（`conn.ReadMessage()` 且忽略内容），
    // 用来确认连接建立了。这里主动发一条，避免它白等满 3 秒。
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<BotRequest>(&mut ws).await? else {
        return Ok(());
    };
    info!(username = %req.username, "收到启动请求");

    let slot = match state.manager.start(&req.username) {
        Ok(slot) => slot,
        Err(StartError::AlreadyRunning) => {
            // 409 有特殊含义，Go 的 startBotReplacingStale 靠它决定杀掉旧
            // 实例再试一次。换成别的码，Go 会以为启动成功然后对着一个它没
            // 启动的机器人发指令。
            send_json(&mut ws, &Response::conflict("机器人已在运行")).await?;
            return Ok(());
        }
    };

    send_json(&mut ws, &Response::ok("启动中")).await?;

    // 把这条连接变成该 bot 的事件订阅者，直到对端断开。
    forward_events(&mut ws, &slot.bot).await
}

// ════════════════════════════════════════════════════════════════
// /ws/api/stopbot
// ════════════════════════════════════════════════════════════════

async fn stopbot(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 stopbot 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_stopbot(socket, state).await {
            debug!("stopbot 连接结束: {e}");
        }
    })
}

async fn handle_stopbot(mut ws: WebSocket, state: AppState) -> Result<()> {
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<BotRequest>(&mut ws).await? else {
        return Ok(());
    };
    info!(username = %req.username, "收到停止请求");

    let resp = match state.manager.stop(&req.username) {
        Ok(()) => Response::ok("已停止"),
        Err(StopError::NotRunning) => Response::not_found("机器人未在运行"),
    };
    send_json(&mut ws, &resp).await?;
    Ok(())
}

// ════════════════════════════════════════════════════════════════
// /ws/api/botstatus
// ════════════════════════════════════════════════════════════════

async fn botstatus(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 botstatus 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_botstatus(socket, state).await {
            debug!("botstatus 连接结束: {e}");
        }
    })
}

async fn handle_botstatus(mut ws: WebSocket, state: AppState) -> Result<()> {
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<BotRequest>(&mut ws).await? else {
        return Ok(());
    };

    let resp = match state.manager.get(&req.username) {
        Some(slot) => {
            let online = slot.bot.is_alive();
            let status = slot.bot.status();
            Response::ok("ok")
                .with("online", online)
                .with("botname", req.username.clone())
                .with("claims", slot.bot.claim_count())
                .with("status", serde_json::to_value(&status)?)
        }
        None => Response::ok("not running").with("online", false),
    };
    send_json(&mut ws, &resp).await?;
    Ok(())
}

// ════════════════════════════════════════════════════════════════
// /ws/api/botlogs
// ════════════════════════════════════════════════════════════════

async fn botlogs(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 botlogs 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_botlogs(socket, state).await {
            debug!("botlogs 连接结束: {e}");
        }
    })
}

async fn handle_botlogs(mut ws: WebSocket, state: AppState) -> Result<()> {
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<BotRequest>(&mut ws).await? else {
        return Ok(());
    };
    info!(username = %req.username, "收到日志订阅");

    let Some(slot) = state.manager.get(&req.username) else {
        send_json(&mut ws, &Response::not_found("机器人未在运行")).await?;
        return Ok(());
    };

    // ⚠️ 这个 200 必须是【请求之后的第一帧】。
    //
    // Go 的 `BotLogs`（wsclient.go）在发完 username 之后只读一条消息，并且
    // 要求它的 `code == 200`，否则返回 "botlogs: bot not running"。而
    // `connectbot` 把那个错误当成「机器人离线」，直接给浏览器推
    // `{"online": false}` —— 控制台于是显示离线并【禁用聊天输入框】。
    //
    // 我原来在这里先补了一帧 status，结果 Go 读到的是 status（没有 code
    // 字段）→ Code 默认 0 → 判定未运行 → 控制台说离线。而机器人其实好好
    // 在线，用户连验证码都发不出去，彻底卡死。
    //
    // 代价是没有「订阅即补一帧状态」的便利 —— 前端要等机器人下次推状态。
    // 这个代价可以接受，而且前端本来就靠 `botOnline` 初值 true 显示在线。
    send_json(&mut ws, &Response::ok("ok")).await?;

    forward_events(&mut ws, &slot.bot).await
}

// ════════════════════════════════════════════════════════════════
// /ws/api/sendinfo
// ════════════════════════════════════════════════════════════════

async fn sendinfo(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 sendinfo 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_sendinfo(socket, state).await {
            debug!("sendinfo 连接结束: {e}");
        }
    })
}

async fn handle_sendinfo(mut ws: WebSocket, state: AppState) -> Result<()> {
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<SendInfoRequest>(&mut ws).await? else {
        return Ok(());
    };

    let Some(slot) = state.manager.get(&req.botname) else {
        send_json(&mut ws, &Response::not_found("机器人未在运行")).await?;
        return Ok(());
    };

    let mut failures = Vec::new();
    for cmd in &req.data {
        if let Some(text) = cmd.chat.as_deref() {
            if let Err(e) = slot.bot.say(text) {
                failures.push(format!("{text}: {e}"));
            }
        }
    }

    let resp = if failures.is_empty() {
        Response::ok("已发送")
    } else {
        Response::bad_request(failures.join("; "))
    };
    send_json(&mut ws, &resp).await?;
    Ok(())
}

// ════════════════════════════════════════════════════════════════
// /ws/api/expand
// ════════════════════════════════════════════════════════════════

async fn expand(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 expand 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_expand(socket, state).await {
            debug!("expand 连接结束: {e}");
        }
    })
}

/// 下发扩地任务。
///
/// 与 JS 版一致：这条连接**不保持**。下发完就回一条结果并关闭 —— 进度通过
/// 事件总线（`/ws/api/events`）推给 Go，再由 Go 转给浏览器。
async fn handle_expand(mut ws: WebSocket, state: AppState) -> Result<()> {
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<ExpandRequest>(&mut ws).await? else {
        return Ok(());
    };
    info!(username = %req.username, action = ?req.action, "收到扩地请求");

    let Some(slot) = state.manager.get(&req.username) else {
        send_json(&mut ws, &Response::not_found("机器人未在运行")).await?;
        return Ok(());
    };

    // ── 停止 ──
    if req.action.as_deref() == Some("stop") {
        let was = slot.bot.queue().stop();
        send_json(
            &mut ws,
            &Response::ok(if was {
                "已停止扩地"
            } else {
                "当前没有正在执行的扩地任务"
            })
            .with("stopped", was),
        )
        .await?;
        return Ok(());
    }

    // ── 开始 ──
    if req.chunks.is_empty() {
        send_json(&mut ws, &Response::bad_request("chunks 为空，没有可扩的区块")).await?;
        return Ok(());
    }

    let Some(pc) = player_chunk(&slot.bot) else {
        // 机器人还没进世界就拿不到位置，而排序要靠它选起点。回 409 让前端
        // 稍后重试，比用一个瞎猜的起点排出一份要横穿选区的顺序好。
        send_json(
            &mut ws,
            &Response::conflict("机器人尚未进入世界（拿不到位置），请稍后重试"),
        )
        .await?;
        return Ok(());
    };

    let ordered = crate::claimqueue::order_square(&req.chunks, &req.occupied, Some(pc));
    let count = ordered.len();

    if !slot.bot.queue().submit(ordered, req.occupied.clone()) {
        send_json(&mut ws, &Response::bad_request("队列为空，未启动")).await?;
        return Ok(());
    }

    // 真正开始跑要把任务送进机器人线程 —— 队列的执行需要机器人那一侧的
    // runtime（寻路只能在那个线程上操作 Azalea 的客户端）。
    let (reply, _rx) = tokio::sync::oneshot::channel();
    let _ = slot.commands.send(crate::bot::Command::RunQueue { reply });

    info!(username = %req.username, chunks = req.chunks.len(), ordered = count, "扩地下发");
    send_json(
        &mut ws,
        &Response::ok(format!("已下发 {count} 个区块，开始扩地")).with("count", count),
    )
    .await?;
    Ok(())
}

/// 取机器人当前所在区块。
fn player_chunk(bot: &Arc<crate::bot::Bot>) -> Option<(i32, i32)> {
    let pos = bot.status().pos?;
    Some(((pos.x / 16.0).floor() as i32, (pos.z / 16.0).floor() as i32))
}

// ════════════════════════════════════════════════════════════════
// /ws/api/events
// ════════════════════════════════════════════════════════════════

async fn events(
    ws: WebSocketUpgrade,
    headers: axum::http::HeaderMap,
    uri: axum::http::Uri,
    State(state): State<AppState>,
) -> impl IntoResponse {
    let secret = extract_secret(&headers, &uri);
    ws.on_upgrade(move |socket| async move {
        if !state.authorized(secret.as_deref()) {
            warn!("拒绝未经授权的 events 握手");
            return reject_unauthorized(socket).await;
        }
        if let Err(e) = handle_events(socket, state).await {
            debug!("events 连接结束: {e}");
        }
    })
}

/// 全局事件流。
///
/// Go 用它订阅所有机器人的状态与扩地进度（`bots.go: jsEventsOnce`），落库
/// 并转给浏览器。这是「管理面板能看到全局」的唯一来源。
///
/// 与 `/botlogs` 的区别：`botlogs` 订阅一个机器人、且保持连接给调用方持续
/// 消费；`events` 是全局的，会在启动时把**所有**在跑机器人的当前状态补一遍
/// —— 不补的话，Go 重启之后要把每个机器人都重启一次才能重新知道它们的
/// 状态。
async fn handle_events(mut ws: WebSocket, state: AppState) -> Result<()> {
    send_json(&mut ws, &serde_json::json!({"code": 200, "message": "ready"})).await?;

    let Some(req) = read_json::<EventsRequest>(&mut ws).await? else {
        return Ok(());
    };
    info!(all = req.all, username = ?req.username, "收到事件订阅");

    // 订阅哪些机器人。
    let names: Vec<String> = match (&req.username, req.all) {
        (Some(name), _) => vec![name.clone()],
        (None, true) => state.manager.running(),
        (None, false) => {
            send_json(&mut ws, &Response::bad_request("需要 all:true 或 username")).await?;
            return Ok(());
        }
    };

    // 先把每个机器人的当前状态补一遍。
    let mut subs = Vec::new();
    for name in &names {
        if let Some(slot) = state.manager.get(name) {
            send_json(
                &mut ws,
                &EventFrame {
                    botname: name.clone(),
                    data: vec![status_event(&slot.bot.status())],
                },
            )
            .await?;
            subs.push((name.clone(), slot.bot.subscribe()));
        }
    }

    // 订阅之后要能发现【新启动】的机器人。
    //
    // Go 侧是一条长连接订阅（`jsEventsOnce` 拨通后一直读），而它可能在
    // 任何机器人启动之前就连上来了 —— 实测就是这样：订阅建立时列表为空，
    // 随后启动的机器人一条事件都推不过去，前端永远看不到状态。
    //
    // 所以这个循环每一轮都要重新扫一遍运行列表，把还没订阅的名字补上。
    let mut subscribed: HashSet<String> = subs.iter().map(|(n, _)| n.clone()).collect();
    let mut next_rescan = tokio::time::Instant::now() + RESCAN_INTERVAL;

    if subs.is_empty() {
        info!("事件订阅时没有运行中的机器人，将在有新机器人时自动纳入");
    }

    // 扇出所有订阅。用 select 轮询每个接收端 —— 数量是机器人数（几十），
    // 每轮遍历一遍完全够用，不值得为它建索引。
    loop {
        // 先处理对端消息（断开检测）。
        if let Ok(Some(msg)) = tokio::time::timeout(Duration::from_millis(0), ws.recv()).await {
            match msg {
                Ok(Message::Close(_)) => return Ok(()),
                Ok(_) => {}
                Err(e) => return Err(e.into()),
            }
        }

        // 定期扫一遍运行列表，把新启动的机器人纳入订阅。
        //
        // 用轮询而不是「启动时通知」：通知要维护一份订阅者注册表，而订阅者
        // 的连接可能随时断掉，那份表就得有一套清理逻辑。轮询的代价是几十个
        // 名字的一次遍历，不值得为它增加状态。
        if tokio::time::Instant::now() >= next_rescan {
            next_rescan = tokio::time::Instant::now() + RESCAN_INTERVAL;
            for name in state.manager.running() {
                if subscribed.contains(&name) {
                    continue;
                }
                if let Some(slot) = state.manager.get(&name) {
                    info!(bot = %name, "事件流纳入新启动的机器人");
                    // 先补一帧当前状态，否则订阅者要等它下次说话才知道它
                    // 已经上线。
                    send_json(
                        &mut ws,
                        &EventFrame {
                            botname: name.clone(),
                            data: vec![status_event(&slot.bot.status())],
                        },
                    )
                    .await?;
                    subs.push((name.clone(), slot.bot.subscribe()));
                    subscribed.insert(name);
                }
            }
        }

        let mut sent_any = false;
        for (name, rx) in subs.iter_mut() {
            match rx.try_recv() {
                Ok(data) => {
                    let frame = EventFrame {
                        botname: name.clone(),
                        data: vec![data],
                    };
                    send_json(&mut ws, &frame).await?;
                    sent_any = true;
                }
                Err(broadcast::error::TryRecvError::Lagged(n)) => {
                    debug!(bot = %name, skipped = n, "事件订阅者落后，跳过若干帧");
                }
                Err(_) => {}
            }
        }

        if !sent_any {
            // 没有事件时短暂让出，避免忙等占满一个核。
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }
}

// ════════════════════════════════════════════════════════════════
// 事件转发
// ════════════════════════════════════════════════════════════════

/// 把机器人的事件转发到这条连接，直到对端断开。
///
/// 每条事件包成 `[{botname, data:[...]}]` 形状的一帧，和 JS 版一致。
///
/// 订阅者落后导致的丢帧（`RecvError::Lagged`）不作为错误处理：一条日志不
/// 值得让整个客户端停下，而且 `broadcast` 会自动跳过落后的部分继续送。
async fn forward_events(ws: &mut WebSocket, bot: &Arc<crate::bot::Bot>) -> Result<()> {
    let mut rx = bot.subscribe();
    loop {
        tokio::select! {
            // 对端断开：读端返回 None 或 Close。这个分支是必要的，否则
            // 机器人安静时（没事件）转发任务不会察觉连接已经没了。
            incoming = ws.recv() => {
                match incoming {
                    Some(Ok(Message::Close(_))) | None => return Ok(()),
                    Some(Err(e)) => return Err(e.into()),
                    // ping/pong 之外，客户端不该在订阅期间发东西。收到就忽略，
                    // 不当作错误 —— 有些代理会插入自己的帧。
                    Some(Ok(_)) => continue,
                }
            }
            event = rx.recv() => {
                match event {
                    Ok(data) => {
                        let frame = EventFrame {
                            botname: bot.username.clone(),
                            data: vec![data],
                        };
                        send_json(ws, &frame).await?;
                    }
                    Err(RecvError::Lagged(n)) => {
                        warn!(bot = %bot.username, skipped = n, "订阅者落后，跳过若干事件");
                    }
                    Err(RecvError::Closed) => return Ok(()),
                }
            }
        }
    }
}

/// 一条指令的构造函数，供测试和将来扩展。
pub fn chat_command(text: impl Into<String>) -> Command {
    Command {
        chat: Some(text.into()),
    }
}

/// 把事件帧压成 JSON，用于测试。
pub fn frame_json(botname: &str, data: EventData) -> serde_json::Value {
    serde_json::to_value(EventFrame {
        botname: botname.to_string(),
        data: vec![data],
    })
    .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::log_event;

    #[test]
    fn 指令构造() {
        let c = chat_command("hello");
        assert_eq!(c.chat.as_deref(), Some("hello"));
    }

    #[test]
    fn 帧形状是数组包对象() {
        let v = frame_json("xiaofanbot", log_event("info", "hi"));
        // 前端按 `[{botname, data:[...]}]` 解析。两层都要检查：
        //   - 最外层是数组：前端第一句 `Array.isArray(data)` 靠它，
        //     发成裸对象会掉进兜底分支，把整坨 JSON 打进控制台；
        //   - `data` 是数组而不是对象：后者会让 `for (const item of
        //     msg.data)` 抛错。
        assert!(v.is_array(), "最外层必须是数组，实际: {v}");
        assert!(v[0]["data"].is_array(), "data 必须是数组，实际: {v}");
        assert_eq!(v[0]["data"][0]["chat"], "[消息] hi");
        assert_eq!(v[0]["botname"], "xiaofanbot");
    }

    #[test]
    fn 状态帧带_status_字段() {
        let st = crate::status::Status::default();
        let v = frame_json("bot", status_event(&st));
        // `status` 必须在 data[0] 下，前端取的是 `item.status`。
        assert!(v.is_array(), "最外层必须是数组，实际: {v}");
        assert!(v[0]["data"][0].get("status").is_some(), "实际: {v}");
    }
}
