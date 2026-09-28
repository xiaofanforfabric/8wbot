//! 机器人状态文本解析。
//!
//! 这是 `8wbotforJavaScript/status.js` 的等价实现，拆成独立模块的理由和那边
//! 一样：这一段要处理同一句话的中英文两种形态，还要把插件吐出的多行邦国信息
//! 拼成一个块，是整条状态链路上最容易出错的地方 —— 能脱离网络单独测，就值得
//! 单独放。
//!
//! 语言背景（沿用原注释，因为这是模式串为什么有两套的原因）：mineflayer 默认
//! locale 是 en_US，所以原版翻译消息（`/server` 的回复）机器人看到的是英文；
//! 而 `/u info` 是服务器插件自己写的文本，看到的中文和玩家客户端一致。Azalea
//! 走的是同一个服务端、同一套语言文件，所以两条路径的语言差异照旧存在，两套
//! 模式都要留。

use once_cell::sync::Lazy;
use regex::Regex;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

/// 「--- 邦国信息: 雅典维亚城邦（二世） ---」
static KINGDOM_HEAD_RE: Lazy<Regex> =
    Lazy::new(|| Regex::new(r"^[-—\s]*邦国信息\s*[:：]\s*(.+?)\s*[-—\s]*$").unwrap());

/// 「You are currently connected to main.」
static SERVER_EN_RE: Lazy<Regex> =
    Lazy::new(|| Regex::new(r"You are currently connected to\s+([A-Za-z0-9_-]+)").unwrap());

/// 「您已连接至 main。」
static SERVER_ZH_RE: Lazy<Regex> =
    Lazy::new(|| Regex::new(r"已连接至\s*([A-Za-z0-9_-]+)").unwrap());

/// 「君主: xiaofan」「邦国特产：陶瓦」—— 半角/全角冒号都认。
///
/// 键长上限 20 是为了不把正文里的长句子误判成字段：一行的冒号左边越长，
/// 越可能是一句话而不是一个字段名。
static KV_RE: Lazy<Regex> =
    Lazy::new(|| Regex::new(r"^\s*([^:：]{1,20})\s*[:：]\s*(.*)$").unwrap());

/// 邦国宣言通常是块的最后一行。
static KINGDOM_END_RE: Lazy<Regex> =
    Lazy::new(|| Regex::new(r"邦国宣言|Kingdom\s+Motto").unwrap());

/// 疆土开拓的通知。
///
/// 形如「疆土 (12, -34) [200, -536] 已为你的邦国开拓」。开拓由走路触发，
/// 服务器没有命令可查，所以这条消息是唯一能知道「刚刚开拓了哪一块」的来源。
static CLAIM_RE: Lazy<Regex> = Lazy::new(|| {
    Regex::new(r"疆土\s*\(\s*(-?\d+)\s*,\s*(-?\d+)\s*\)\s*\[\s*(-?\d+)\s*,\s*(-?\d+)\s*\]\s*已为你的邦国开拓")
        .unwrap()
});

/// 一条聊天消息解析出的全部状态。
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Status {
    /// 位置，未上报时为 None。
    pub pos: Option<Pos>,
    /// 维度：`overworld` / `the_nether` / `the_end`，与 mineflayer 的取值一致，
    /// 这样前端不需要为换实现改一行。
    pub dimension: Option<String>,
    /// 当前所在子服，例如 `main`。
    pub server: Option<String>,
    /// 邦国信息块，解析到之后填充。
    pub kingdom: Option<Kingdom>,
    /// 最近一次开拓的区块。
    pub last_claim: Option<Claim>,
    /// 最后一次更新的时间（RFC3339）。
    pub updated_at: Option<String>,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct Pos {
    pub x: f64,
    pub y: f64,
    pub z: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Kingdom {
    pub title: String,
    pub fields: BTreeMap<String, String>,
    pub raw: Vec<String>,
    pub updated_at: String,
}

/// 一次领地开拓。
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct Claim {
    /// 区块坐标。
    pub chunk_x: i32,
    pub chunk_z: i32,
    /// 方块坐标（区块中心）。
    pub x: i32,
    pub z: i32,
}

/// 返回邦国信息块的标题；不是首行则返回 None。
pub fn parse_kingdom_head(text: &str) -> Option<String> {
    KINGDOM_HEAD_RE
        .captures(text)
        .map(|c| c[1].to_string())
}

/// 返回服务器名；认不出则返回 None。
pub fn parse_server(text: &str) -> Option<String> {
    if let Some(c) = SERVER_EN_RE.captures(text) {
        return Some(c[1].to_string());
    }
    SERVER_ZH_RE.captures(text).map(|c| c[1].to_string())
}

/// 解析「键: 值」行，返回 `(key, value)`；不成字段则返回 None。
pub fn parse_field(text: &str) -> Option<(String, String)> {
    let c = KV_RE.captures(text)?;
    let key = c[1].trim().to_string();
    let value = c[2].trim().to_string();
    if key.is_empty() || value.is_empty() {
        return None;
    }
    Some((key, value))
}

/// 这一行是否标志邦国信息块结束。
pub fn is_kingdom_end(text: &str) -> bool {
    KINGDOM_END_RE.is_match(text)
}

/// 解析疆土开拓通知。
///
/// 方块坐标由 `chunk * 16 + 8` 得到 —— 服务器两个坐标都给，但方块坐标是
/// 区块中心，用区块坐标反而更容易和「当前在哪个区块」对齐。
pub fn parse_claim(text: &str) -> Option<Claim> {
    let c = CLAIM_RE.captures(text)?;
    Some(Claim {
        chunk_x: c[1].parse().ok()?,
        chunk_z: c[2].parse().ok()?,
        x: c[3].parse().ok()?,
        z: c[4].parse().ok()?,
    })
}

/// 把累积到的原始行拼成结构化结果。
///
/// 只有标题、没有正文的残缺块会被判为无效并返回 None —— 服务器偶尔只发
/// 一个标题就断了（掉线、切服），把那种壳当成有效状态会让前端显示一个
/// 全是空的邦国面板。
pub fn parse_kingdom_block(title: &str, raw_lines: &[String]) -> Option<Kingdom> {
    let mut fields = BTreeMap::new();
    for line in raw_lines {
        // 跳过标题行本身：它也是「键: 值」形态，不排除会多出一个名为
        // 「--- 邦国信息」的伪字段。
        if KINGDOM_HEAD_RE.is_match(line) {
            continue;
        }
        if let Some((k, v)) = parse_field(line) {
            fields.insert(k, v);
        }
    }
    if fields.is_empty() && raw_lines.len() < 3 {
        return None;
    }
    Some(Kingdom {
        title: title.to_string(),
        fields,
        raw: raw_lines.to_vec(),
        updated_at: now_rfc3339(),
    })
}

/// 当前时间，RFC3339。与 JS 的 `new Date().toISOString()` 同一个格式，
/// 前端解析不用改。
pub fn now_rfc3339() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let d = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let secs = d.as_secs();
    let millis = d.subsec_millis();
    // 手写而不引入 chrono：这里只需要一个格式固定的时间戳，多一个依赖
    // 不值得。算法取自 civil_from_days。
    let days = (secs / 86_400) as i64;
    let rem = secs % 86_400;
    let (h, mi, s) = (rem / 3600, (rem % 3600) / 60, rem % 60);
    let (y, m, dd) = civil_from_days(days);
    format!("{y:04}-{m:02}-{dd:02}T{h:02}:{mi:02}:{s:02}.{millis:03}Z")
}

/// 把「1970-01-01 起的天数」换算成公历年月日。
///
/// 这是 Howard Hinnant 的 `civil_from_days`，被各标准库广泛采用。
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 解析邦国信息标题() {
        assert_eq!(
            parse_kingdom_head("--- 邦国信息: 雅典维亚城邦（二世） ---"),
            Some("雅典维亚城邦（二世）".to_string())
        );
        assert_eq!(parse_kingdom_head("邦国信息：测试邦"), Some("测试邦".to_string()));
        assert_eq!(parse_kingdom_head("随便一句话"), None);
    }

    #[test]
    fn 解析服务器名_中英文都要认() {
        assert_eq!(
            parse_server("You are currently connected to main."),
            Some("main".to_string())
        );
        assert_eq!(parse_server("您已连接至 main。"), Some("main".to_string()));
        assert_eq!(parse_server("无关的话"), None);
    }

    #[test]
    fn 解析字段_半角全角冒号都认() {
        assert_eq!(
            parse_field("君主: xiaofan"),
            Some(("君主".to_string(), "xiaofan".to_string()))
        );
        assert_eq!(
            parse_field("邦国特产：陶瓦"),
            Some(("邦国特产".to_string(), "陶瓦".to_string()))
        );
        assert_eq!(parse_field("没有冒号"), None);
    }

    #[test]
    fn 键长上限挡住长句子() {
        // 冒号左边超过 20 个字符就不当成字段，否则正文里的句子会污染 fields。
        //
        // 数的是字符而不是字节：Rust 的 regex 在 Unicode 模式下 `{1,20}`
        // 按字符计，这对中文正好和 JS 的按 UTF-16 码元计一致 —— 汉字在
        // 两种计数下都是一个单位。只有 emoji 之类会出现差异，而字段名里
        // 不会出现 emoji。
        let long = "这句话一共超过了二十个字符所以不该被当成字段名: 后面还有内容";
        assert!(
            long.split(':').next().unwrap().chars().count() > 20,
            "测试数据本身要超过上限，否则测不到边界"
        );
        assert_eq!(parse_field(long), None);

        // 刚好 20 字符要能通过，确认边界是「不超过」而不是「小于」。
        let at_limit = "一二三四五六七八九十一二三四五六七八九十: 值";
        assert_eq!(at_limit.split(':').next().unwrap().chars().count(), 20);
        assert!(parse_field(at_limit).is_some(), "20 字应当仍然算字段");
    }

    #[test]
    fn 拼装邦国块() {
        let raw = vec![
            "--- 邦国信息: 测试邦 ---".to_string(),
            "君主: xiaofan".to_string(),
            "邦国特产: 陶瓦".to_string(),
            "邦国宣言: 测试".to_string(),
        ];
        let k = parse_kingdom_block("测试邦", &raw).expect("应当解析成功");
        assert_eq!(k.title, "测试邦");
        assert_eq!(k.fields.get("君主").map(String::as_str), Some("xiaofan"));
        assert_eq!(k.fields.get("邦国特产").map(String::as_str), Some("陶瓦"));
        // 标题行不能变成一个伪字段。
        assert!(!k.fields.keys().any(|k| k.contains("邦国信息")));
    }

    #[test]
    fn 残缺块判为无效() {
        // 只有一个标题、正文没到，是切服/掉线时的中间态。
        let raw = vec!["--- 邦国信息: 测试邦 ---".to_string()];
        assert!(parse_kingdom_block("测试邦", &raw).is_none());
    }

    #[test]
    fn 解析疆土开拓() {
        let c = parse_claim("疆土 (12, -34) [200, -536] 已为你的邦国开拓").expect("应当解析");
        assert_eq!(c.chunk_x, 12);
        assert_eq!(c.chunk_z, -34);
        assert_eq!(c.x, 200);
        assert_eq!(c.z, -536);
        // 带不带空格都要认 —— 服务器格式在不同插件版本里变过。
        assert!(parse_claim("疆土(1,2)[24,40]已为你的邦国开拓").is_some());
        assert!(parse_claim("疆土 (1, 2) 已为你的邦国开拓").is_none());
    }

    #[test]
    fn 时间戳格式() {
        let t = now_rfc3339();
        // 形如 2026-09-28T22:15:33.123Z
        assert_eq!(t.len(), 24, "实际: {t}");
        assert!(t.ends_with('Z'));
        assert_eq!(&t[4..5], "-");
        assert_eq!(&t[10..11], "T");
    }
}
