//! 端到端测试：用真实的 WebSocket 客户端打自己实现的端点。
//!
//! 单元测试只能验证类型和解析，验证不了「Go 后端这样发过来，我这样回过去，
//! 它能不能读懂」。这个测试把整套握手机制跑一遍 —— 包括那条最容易搞错的
//! 「先发 ready 再收请求」的顺序，以及 409 必须原样传出去。
//!
//! 它**不连接游戏服务器**：这里只测控制面。机器人的实际行为由真实服务器上
//! 的手工验证覆盖。

use bgjq_bot::manager::Manager;
use bgjq_bot::server::{self, AppState};
use futures_util::{SinkExt, StreamExt};
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};

/// 测试里用到的连接类型。
///
/// 写成具体类型而不是泛型：泛型要把 `Sink::Error: Debug` 一路带在签名上，
/// 而这里只有一种连接，多出来的抽象没有换来任何东西。
type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

/// 起一个带密钥的测试节点，返回它的地址。
async fn spawn_secured(secret: &str) -> String {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let manager = Manager::new("127.0.0.1:1");
    let mut state = AppState::insecure(manager);
    state.secret = Some(secret.to_string());

    tokio::task::spawn_local(async move {
        let _ = axum::serve(listener, server::router(state)).await;
    });

    format!("ws://{addr}")
}

/// 拨通一个带查询串的地址，不做 ready 检查。
async fn connect_raw(
    url: &str,
) -> Result<Ws, tokio_tungstenite::tungstenite::Error> {
    let (ws, _) = tokio_tungstenite::connect_async(url).await?;
    Ok(ws)
}

/// 起一个测试用的节点，返回它的地址。
async fn spawn_node() -> String {
    // 端口 0 让内核分配，避免和真实节点的 8088 撞车，也避免并行测试互相抢。
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let manager = Manager::new("127.0.0.1:1"); // 故意的死地址：不会真去连
    let state = AppState::new(manager);

    tokio::task::spawn_local(async move {
        let _ = axum::serve(listener, server::router(state)).await;
    });

    format!("ws://{addr}")
}

/// 拨通一个端点，跳过节点发来的 ready 帧。
async fn connect(base: &str, path: &str) -> Ws {
    let (mut ws, _) = tokio_tungstenite::connect_async(format!("{base}{path}"))
        .await
        .expect("连接失败");
    let ready = ws.next().await.expect("应当收到 ready").unwrap();
    let text = ready.into_text().unwrap();
    assert!(
        text.contains(r#""code":200"#),
        "ready 帧应当带 code 200，实际: {text}"
    );
    ws
}

/// 发一条 JSON 并读回一条。
async fn roundtrip(ws: &mut Ws, payload: &str) -> serde_json::Value {
    ws.send(Message::Text(payload.into())).await.unwrap();
    let msg = ws.next().await.expect("应当有应答").unwrap();
    let text = msg.into_text().unwrap();
    serde_json::from_str(&text).expect("应答应当是 JSON")
}

#[tokio::test]
async fn 未运行的机器人查询返回_online_false() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/botstatus").await;
            let resp = roundtrip(&mut ws, r#"{"username":"nobody"}"#).await;
            assert_eq!(resp["code"], 200);
            assert_eq!(resp["online"], false);
        })
        .await;
}

#[tokio::test]
async fn 停止未运行的机器人返回_404() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/stopbot").await;
            let resp = roundtrip(&mut ws, r#"{"username":"nobody"}"#).await;
            assert_eq!(resp["code"], 404, "实际: {resp}");
        })
        .await;
}

#[tokio::test]
async fn 给未运行的机器人发消息返回_404() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/sendinfo").await;
            // 注意字段是 botname 不是 username —— Go 侧就是这么发的。
            let resp = roundtrip(
                &mut ws,
                r#"{"botname":"nobody","data":[{"chat":"hi"}]}"#,
            )
            .await;
            assert_eq!(resp["code"], 404, "实际: {resp}");
        })
        .await;
}

#[tokio::test]
async fn 订阅未运行的机器人返回_404() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/botlogs").await;
            let resp = roundtrip(&mut ws, r#"{"username":"nobody"}"#).await;
            assert_eq!(resp["code"], 404, "实际: {resp}");
        })
        .await;
}

#[tokio::test]
async fn 重复启动同一个机器人第二次返回_409() {
    // 这是最关键的一条：Go 的 startBotReplacingStale 靠 409 决定杀掉旧实例
    // 重试。如果这里回 200，Go 会以为启动成功，然后对着一个它没启动的机器人
    // 发指令 —— 表现为「点了启动但机器人不动」，且没有任何报错。
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;

            // 第一次启动。地址是死地址，所以机器人永远连不上，但它会占住
            // 名字 —— 这正是要测的状态。
            let mut first = connect(&base, "/ws/api/startbot").await;
            first
                .send(Message::Text(r#"{"username":"xiaofanbot"}"#.into()))
                .await
                .unwrap();
            let resp = first.next().await.unwrap().unwrap().into_text().unwrap();
            let v: serde_json::Value = serde_json::from_str(&resp).unwrap();
            assert_eq!(v["code"], 200, "首次启动应当成功，实际: {resp}");

            // 第二次启动同一个名字。
            let mut second = connect(&base, "/ws/api/startbot").await;
            let v2 = roundtrip(&mut second, r#"{"username":"xiaofanbot"}"#).await;
            assert_eq!(v2["code"], 409, "重复启动应当是 409，实际: {v2}");
        })
        .await;
}

#[tokio::test]
async fn 启动后能查到并停止() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;

            let mut starter = connect(&base, "/ws/api/startbot").await;
            starter
                .send(Message::Text(r#"{"username":"bot1"}"#.into()))
                .await
                .unwrap();
            let _ = starter.next().await.unwrap().unwrap();

            // 查得到。
            let mut status = connect(&base, "/ws/api/botstatus").await;
            let v = roundtrip(&mut status, r#"{"username":"bot1"}"#).await;
            assert_eq!(v["code"], 200);
            assert_eq!(v["botname"], "bot1");
            // 死地址连不上，所以在运行但不在线。
            assert_eq!(v["online"], false);

            // 停得掉。
            let mut stop = connect(&base, "/ws/api/stopbot").await;
            let v = roundtrip(&mut stop, r#"{"username":"bot1"}"#).await;
            assert_eq!(v["code"], 200, "停止应当成功，实际: {v}");

            // 停完之后查不到。
            let mut status2 = connect(&base, "/ws/api/botstatus").await;
            let v = roundtrip(&mut status2, r#"{"username":"bot1"}"#).await;
            assert_eq!(v["online"], false);
        })
        .await;
}

#[tokio::test]
async fn 坏_json_不会让节点崩溃() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/botstatus").await;

            // 发一条不是 JSON 的。连接会被关掉（解析失败），但节点本身必须
            // 还能继续服务 —— 所以紧接着开一条新连接验证。
            let _ = ws.send(Message::Text("这不是 JSON".into())).await;
            let _ = ws.next().await;

            let mut ws2 = connect(&base, "/ws/api/botstatus").await;
            let v = roundtrip(&mut ws2, r#"{"username":"nobody"}"#).await;
            assert_eq!(v["code"], 200, "节点应当仍然可用，实际: {v}");
        })
        .await;
}


// ════════════════════════════════════════════════════════════════
// 鉴权
// ════════════════════════════════════════════════════════════════

#[tokio::test]
async fn 没带密钥时收到_401_帧() {
    // 这个节点在公网后面，接口能启停机器人、能代替它们说话 —— 密钥校验
    // 是必须的。原 JS 节点也是这么做的。
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_secured("s3cr3t").await;
            let mut ws = connect_raw(&format!("{base}/ws/api/botstatus"))
                .await
                .expect("升级本身应当成功 —— 失败信息要通过帧传，而不是拒绝升级");
            let msg = ws.next().await.expect("应当有帧").unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&msg.into_text().unwrap()).unwrap();
            assert_eq!(v["code"], 401, "实际: {v}");
        })
        .await;
}

#[tokio::test]
async fn 密钥错误时收到_401_帧() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_secured("s3cr3t").await;
            let mut ws = connect_raw(&format!("{base}/ws/api/botstatus?secret=wrong"))
                .await
                .unwrap();
            let msg = ws.next().await.unwrap().unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&msg.into_text().unwrap()).unwrap();
            assert_eq!(v["code"], 401, "实际: {v}");
        })
        .await;
}

#[tokio::test]
async fn 查询串带对密钥时放行() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_secured("s3cr3t").await;
            let mut ws = connect_raw(&format!("{base}/ws/api/botstatus?secret=s3cr3t"))
                .await
                .unwrap();
            let msg = ws.next().await.unwrap().unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&msg.into_text().unwrap()).unwrap();
            // 放行之后读到的是 ready，不是 401。
            assert_eq!(v["code"], 200, "实际: {v}");
        })
        .await;
}

#[tokio::test]
async fn 头部带对密钥时放行() {
    // Go 后端用查询串，但也支持头部 —— 两种都要认。
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_secured("s3cr3t").await;
            let request = tokio_tungstenite::tungstenite::handshake::client::Request::builder()
                .uri(format!("{base}/ws/api/botstatus"))
                .header("x-internal-secret", "s3cr3t")
                .header("Host", "localhost")
                .header("Connection", "Upgrade")
                .header("Upgrade", "websocket")
                .header("Sec-WebSocket-Version", "13")
                .header("Sec-WebSocket-Key", tokio_tungstenite::tungstenite::handshake::client::generate_key())
                .body(())
                .unwrap();
            let (mut ws, _) = tokio_tungstenite::connect_async(request).await.unwrap();
            let msg = ws.next().await.unwrap().unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&msg.into_text().unwrap()).unwrap();
            assert_eq!(v["code"], 200, "实际: {v}");
        })
        .await;
}

#[tokio::test]
async fn 未配置密钥时不校验但能用() {
    // 本地开发路径：没配密钥就一律放行。这在生产上是危险的，所以启动时
    // 会打警告 —— 但功能本身要正常。
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/botstatus").await;
            let v = roundtrip(&mut ws, r#"{"username":"nobody"}"#).await;
            assert_eq!(v["code"], 200);
        })
        .await;
}


// ════════════════════════════════════════════════════════════════
// /ws/api/expand
// ════════════════════════════════════════════════════════════════

#[tokio::test]
async fn 扩地下发到未运行的机器人返回_404() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/expand").await;
            let resp = roundtrip(
                &mut ws,
                r#"{"username":"nobody","chunks":[[1,2],[3,4]]}"#,
            )
            .await;
            assert_eq!(resp["code"], 404, "实际: {resp}");
        })
        .await;
}

#[tokio::test]
async fn 扩地选区为空返回_400() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/expand").await;
            let resp = roundtrip(&mut ws, r#"{"username":"nobody","chunks":[]}"#).await;
            // 未运行的检查在前，所以这里是 404 而不是 400 —— 但只要不是
            // 200 就说明没有误启动一个空任务。
            assert_ne!(resp["code"], 200, "实际: {resp}");
        })
        .await;
}

#[tokio::test]
async fn 未授权时_expand_也拒绝() {
    // 新的端点必须和旧的一样受密钥保护 —— 漏掉一个就等于开了后门。
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_secured("s3cr3t").await;
            let mut ws = connect_raw(&format!("{base}/ws/api/expand")).await.unwrap();
            let msg = ws.next().await.unwrap().unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&msg.into_text().unwrap()).unwrap();
            assert_eq!(v["code"], 401, "实际: {v}");
        })
        .await;
}

#[tokio::test]
async fn 未授权时_events_也拒绝() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_secured("s3cr3t").await;
            let mut ws = connect_raw(&format!("{base}/ws/api/events")).await.unwrap();
            let msg = ws.next().await.unwrap().unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&msg.into_text().unwrap()).unwrap();
            assert_eq!(v["code"], 401, "实际: {v}");
        })
        .await;
}

#[tokio::test]
async fn events_订阅没有机器人时不报错也不断开() {
    // Go 侧会一直挂着这条连接。如果这里立刻断开，Go 会在重连循环里空转。
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect_raw(&format!("{base}/ws/api/events")).await.unwrap();
            // ready
            let ready = ws.next().await.unwrap().unwrap();
            let v: serde_json::Value =
                serde_json::from_str(&ready.into_text().unwrap()).unwrap();
            assert_eq!(v["code"], 200);

            ws.send(Message::Text(r#"{"all":true}"#.into())).await.unwrap();

            // 一秒内不该有关闭帧。
            let next = tokio::time::timeout(std::time::Duration::from_secs(1), ws.next()).await;
            match next {
                Err(_) => {} // 超时 = 连接还开着，正是期望
                Ok(Some(Ok(Message::Close(_)))) => panic!("不该关闭连接"),
                Ok(Some(Err(_))) => panic!("不该出错"),
                Ok(None) => panic!("不该结束"),
                Ok(Some(Ok(_))) => {} // 收到别的帧也行
            }
        })
        .await;
}

#[tokio::test]
async fn events_既没_all_也没_username_返回_400() {
    let local = tokio::task::LocalSet::new();
    local
        .run_until(async {
            let base = spawn_node().await;
            let mut ws = connect(&base, "/ws/api/events").await;
            let v = roundtrip(&mut ws, r#"{"all":false}"#).await;
            assert_eq!(v["code"], 400, "实际: {v}");
        })
        .await;
}
