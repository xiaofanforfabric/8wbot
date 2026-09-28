//! 与 Go 后端的 WebSocket 契约。
//!
//! 这些类型逐字对应 `goservers/wsclient.go` 实际收发的 JSON。字段名、类型和
//! 默认值都是照着那边读出来的，不是设计出来的 —— 两边对不上，机器人就不会
//! 被驱动，而失败会表现为「连上了但没反应」，比报错更难查。
//!
//! 有一处历史遗留的不一致：`sendinfo` 用 `botname`，其他端点用 `username`。
//! 接收时两个都认，发送时仍用各自的原文，这样既能兼容现有 Go 后端，也不必
//! 让它先改。

use serde::{Deserialize, Serialize};

/// `/ws/api/startbot`、`/ws/api/stopbot`、`/ws/api/botstatus`、`/ws/api/botlogs`
/// 的请求体。
#[derive(Debug, Clone, Deserialize)]
pub struct BotRequest {
    /// 启动哪个机器人。
    pub username: String,
}

/// `/ws/api/sendinfo` 的请求体。
///
/// 用 `botname` 而不是 `username` —— Go 侧就是这么发的。
#[derive(Debug, Clone, Deserialize)]
pub struct SendInfoRequest {
    pub botname: String,
    /// 指令列表。当前只有 `chat` 一种。
    pub data: Vec<Command>,
}

/// `sendinfo` 里的一条指令。
#[derive(Debug, Clone, Deserialize)]
pub struct Command {
    /// 要发送到游戏聊天的文本。以 `/` 开头就是命令，由服务端区分。
    pub chat: Option<String>,
}

/// 所有端点的统一应答。
///
/// Go 侧只看 `code` 和 `message`，所以这两个字段必须始终在。其余字段放在
/// `extra` 里，避免为每个端点定义一个类型却只有一处用。
#[derive(Debug, Clone, Serialize)]
pub struct Response {
    pub code: u16,
    pub message: String,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

impl Response {
    pub fn ok(message: impl Into<String>) -> Self {
        Self {
            code: 200,
            message: message.into(),
            extra: serde_json::Map::new(),
        }
    }

    /// `409` 有特殊含义：机器人已在运行。
    ///
    /// Go 的 `startBotReplacingStale` 靠它决定「杀掉旧实例再试一次」，所以这
    /// 个码不能换成别的，也不能用 200 代替 —— 那样 Go 会以为启动成功，然后
    /// 对着一个它没启动的机器人发指令。
    pub fn conflict(message: impl Into<String>) -> Self {
        Self {
            code: 409,
            message: message.into(),
            extra: serde_json::Map::new(),
        }
    }

    pub fn not_found(message: impl Into<String>) -> Self {
        Self {
            code: 404,
            message: message.into(),
            extra: serde_json::Map::new(),
        }
    }

    pub fn bad_request(message: impl Into<String>) -> Self {
        Self {
            code: 400,
            message: message.into(),
            extra: serde_json::Map::new(),
        }
    }

    /// 鉴权失败。
    ///
    /// 用 401 而不是直接拒绝升级：Go 侧读的是 WS 帧里的 `code`，回 HTTP
    /// 状态码它会看不懂。这个码和原 JS 节点一致。
    pub fn unauthorized(message: impl Into<String>) -> Self {
        Self {
            code: 401,
            message: message.into(),
            extra: serde_json::Map::new(),
        }
    }

    pub fn with(mut self, key: &str, value: impl Into<serde_json::Value>) -> Self {
        self.extra.insert(key.to_string(), value.into());
        self
    }
}

/// `/ws/api/expand` 的请求体。
///
/// 与 Go 的 `POST /api/expand` 转发的形状一致（见 `main.go:2045`）：
/// 停止时只带 `action:"stop"`，开始时代 `chunks` 和 `occupied`。
#[derive(Debug, Clone, Deserialize)]
pub struct ExpandRequest {
    pub username: String,
    /// `"stop"` 表示中断当前扩地任务。
    #[serde(default)]
    pub action: Option<String>,
    /// 待扩区块，`[[cx, cz], ...]`。
    #[serde(default)]
    pub chunks: Vec<(i32, i32)>,
    /// 服务器已知已占区块，供排序判断接壤。
    #[serde(default)]
    pub occupied: Vec<(i32, i32)>,
}

/// `/ws/api/events` 的订阅请求。
///
/// Go 发的是 `{"all":true}` —— 全局订阅，收所有机器人的事件。
#[derive(Debug, Clone, Deserialize)]
pub struct EventsRequest {
    #[serde(default)]
    pub all: bool,
    /// 也可以只订阅一个机器人。
    #[serde(default)]
    pub username: Option<String>,
}

/// 推给订阅者的日志/状态帧。
///
/// 形状是 `[{botname, data:[...]}]` —— 外面包一层数组是 JS 版既有的格式，
/// 前端按这个形状解析，所以照搬。
#[derive(Debug, Clone, Serialize)]
pub struct EventFrame {
    pub botname: String,
    pub data: Vec<EventData>,
}

/// 一帧里的一条事件。
///
/// 这一层用 `serde_json::Value` 而不是具体类型，是因为 `data` 里可以放日志行、
/// 状态块、开拓通知等好几种载荷，而它们共用同一个 `botname`/`data` 外壳。
/// 前端按字段名取用，多一个类型只会让每次加一种载荷都要改两处。
pub type EventData = serde_json::Map<String, serde_json::Value>;

/// 构造一条日志事件。
pub fn log_event(level: &str, text: &str) -> EventData {
    let mut m = EventData::new();
    m.insert("chat".into(), format!("{}{}", prefix_for(level), text).into());
    m
}

/// 日志级别到聊天前缀。
///
/// **这个前缀不是装饰，是契约的一部分。** 前端只认 `data[].chat` 这一个
/// 字段（见 `ConsolePage.vue` 的 `if (d.chat) appendLog(d.chat)`），前缀
/// 由节点加好。我一开始发的是 `{level, text, time}` 三个字段，前端一个都
/// 不认 —— 于是每一帧都落到最后的兜底分支
/// `appendLog(JSON.stringify(data))`，把整坨 JSON 打进控制台。用户看到的
/// 就是满屏 `{"botname":"xiaofanbot","data":[{"level":"info",...}]}`。
///
/// 前缀取值与原 JS 版一致（`index.js` 里那几处 broadcast）：
/// `[消息]` `[聊天]` `[系统]`。
fn prefix_for(level: &str) -> &'static str {
    match level {
        // 聊天：机器人自己说的、以及别人说的话。
        "chat" => "[聊天] ",
        // 出站指令（我们发给服务器的）。原来用 "info" 表示，但那样它和
        // 系统消息混在一起，看不出这是「我发出去的」。
        "info" => "[消息] ",
        // 其余（warn/error/debug）都是节点自己的状态通报。
        _ => "[系统] ",
    }
}

/// 构造一条状态事件。
pub fn status_event(status: &crate::status::Status) -> EventData {
    let mut m = EventData::new();
    m.insert("status".into(), serde_json::to_value(status).unwrap_or_default());
    m
}

/// 构造一条开拓事件。
pub fn claim_event(claim: &crate::status::Claim) -> EventData {
    let mut m = EventData::new();
    m.insert("claim".into(), serde_json::to_value(claim).unwrap_or_default());
    m
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 解析_startbot_请求() {
        let r: BotRequest = serde_json::from_str(r#"{"username":"xiaofanbot"}"#).unwrap();
        assert_eq!(r.username, "xiaofanbot");
    }

    #[test]
    fn 解析_sendinfo_请求() {
        // 这条是 Go 侧的实际形状，字段名照抄。
        let raw = r#"{"botname":"xiaofanbot","data":[{"chat":"hello"}]}"#;
        let r: SendInfoRequest = serde_json::from_str(raw).unwrap();
        assert_eq!(r.botname, "xiaofanbot");
        assert_eq!(r.data.len(), 1);
        assert_eq!(r.data[0].chat.as_deref(), Some("hello"));
    }

    #[test]
    fn 应答的_code_是数字而不是字符串() {
        // Go 侧做的是 `startResp["code"].(float64)`，发字符串会让类型断言
        // 静默失败并得到 0，于是每个成功都被当成失败。
        let json = serde_json::to_string(&Response::ok("started")).unwrap();
        assert!(json.contains(r#""code":200"#), "实际: {json}");
    }

    #[test]
    fn conflict_是_409() {
        // 这个码决定 Go 会不会杀掉旧实例后重试。
        let json = serde_json::to_string(&Response::conflict("已经在跑")).unwrap();
        assert!(json.contains(r#""code":409"#), "实际: {json}");
    }

    #[test]
    fn 额外字段铺平到顶层() {
        let r = Response::ok("done").with("server", "main");
        let json = serde_json::to_string(&r).unwrap();
        assert!(json.contains(r#""server":"main""#), "实际: {json}");
    }

    #[test]
    fn 事件帧形状() {
        let f = EventFrame {
            botname: "xiaofanbot".into(),
            data: vec![log_event("info", "hello")],
        };
        let json = serde_json::to_value(&f).unwrap();
        // 外层是对象，data 是数组 —— 前端按这个形状取。
        assert_eq!(json["botname"], "xiaofanbot");
        assert!(json["data"].is_array());
        assert_eq!(json["data"][0]["chat"], "[消息] hello");
    }

    /// 日志必须发 `chat` 字段，不能发 `{level, text, time}`。
    ///
    /// 这是我犯过的一个错误：发成 `{level, text}` 之后，前端一个字段都不
    /// 认（它只读 `d.chat`），每一帧都落到兜底分支
    /// `appendLog(JSON.stringify(data))` —— 用户看到的控制台里是满屏的
    /// `{"botname":"xiaofanbot","data":[{"level":"info",...}]}`。
    ///
    /// 所以这条测试锁死字段名和前缀，避免再退回去。
    #[test]
    fn 日志只发_chat_字段且带前缀() {
        for (level, want_prefix) in
            [("chat", "[聊天] "), ("info", "[消息] "), ("warn", "[系统] "), ("error", "[系统] ")]
        {
            let ev = log_event(level, "内容");
            assert!(
                ev.contains_key("chat"),
                "级别 {level} 应当发 chat 字段，实际: {ev:?}"
            );
            // 不能有前端不认的字段 —— 多发的字段不会报错，只会静默失效，
            // 而「静默失效」正是这个 bug 最难查的地方。
            for bad in ["level", "text", "time"] {
                assert!(
                    !ev.contains_key(bad),
                    "不该再发 {bad} 字段（前端不认），级别 {level}"
                );
            }
            assert_eq!(
                ev["chat"], format!("{want_prefix}内容"),
                "级别 {level} 的前缀不对"
            );
        }
    }
}
