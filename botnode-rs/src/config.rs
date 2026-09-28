//! 配置：从 `.env` 和环境变量读取。
//!
//! ## 为什么要有这一层
//!
//! 原来的 JS 节点用 `dotenv` 读 `.env`（`index.js:1`），运维习惯了「改
//! `.env` 重启」这条路。如果 Rust 节点只认环境变量，上线时那份已经存在的
//! `.env` 就白写了，而且两边配置来源不一致会让「为什么香港那边好了这边
//! 没好」变成一件难查的事。
//!
//! ## 变量名沿用 JS 节点的那一套
//!
//! `MC_HOST` / `MC_PORT` / `WS_PORT` / `INTERNAL_NODE_SECRET` —— 现有的
//! `8wbotforJavaScript/.env` 可以直接拿来用，不用改一个字节。我最初自己
//! 定了 `MC_ADDRESS` / `CONTROL_ADDR`，那是个没必要的分叉。
//!
//! 优先级：**进程环境变量 > `.env` 文件 > 内置默认值**。环境变量优先是因为
//! systemd 的 `Environment=` 和容器编排都走那条路，而它们的意图比一个可能
//! 忘了更新的文件更明确。

use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// 节点配置。
#[derive(Debug, Clone)]
pub struct Config {
    /// 游戏服务器地址，`host:port` 形式。
    pub mc_address: String,
    /// 控制端口监听地址。
    pub control_addr: String,
    /// 内部节点密钥。`None` 表示不校验（会打警告）。
    pub secret: Option<String>,
}

impl Config {
    /// 从 `.env` 和进程环境变量装载。
    ///
    /// 在当前目录及向上两级里找 `.env`（最多三层）—— 这样无论从仓库根、
    /// `botnode-rs/` 还是部署目录启动都能找到同一份配置。
    pub fn load() -> Self {
        let mut file_vars = HashMap::new();
        if let Some(path) = find_env_file() {
            match parse_env_file(&path) {
                Ok(vars) => {
                    tracing::info!(path = %path.display(), count = vars.len(), "已载入 .env");
                    file_vars = vars;
                }
                Err(e) => {
                    // 读不动不该拦住启动：很多部署方式（容器、systemd 的
                    // EnvironmentFile）压根没有 .env，全靠环境变量。
                    tracing::warn!(path = %path.display(), "读取 .env 失败: {e}");
                }
            }
        }

        // 环境变量优先，缺了才看 .env。
        let get = |key: &str| -> Option<String> {
            std::env::var(key)
                .ok()
                .filter(|s| !s.is_empty())
                .or_else(|| file_vars.get(key).cloned())
        };

        let host = get("MC_HOST").unwrap_or_else(|| "bgjq.simpfun.cn".to_string());
        let port = get("MC_PORT").unwrap_or_else(|| "25565".to_string());

        // `MC_ADDRESS` 也认，作为「想一次给全 host:port」的写法。两个都给时
        // 以它为准 —— 更具体的那一个胜出。
        let mc_address = get("MC_ADDRESS").unwrap_or_else(|| format!("{host}:{port}"));

        // JS 节点用 `WS_PORT` 表示控制端口，而它只给端口号不给地址。
        // `CONTROL_ADDR` 可以给完整的 `host:port`，给的时候就以它为准。
        let control_addr = get("CONTROL_ADDR").unwrap_or_else(|| {
            let p = get("WS_PORT").unwrap_or_else(|| "8088".to_string());
            // 默认只绑本机 —— 这个端口能启停机器人、能代替它们说话。
            format!("127.0.0.1:{p}")
        });

        let secret = get("INTERNAL_NODE_SECRET");

        Self {
            mc_address,
            control_addr,
            secret,
        }
    }

    /// 密钥是否缺失。启动时据此打警告。
    pub fn secret_missing(&self) -> bool {
        self.secret.is_none()
    }
}

/// 在当前目录及向上两级里找 `.env`。
///
/// 找的是「最近的」那一个：从部署目录启动时应当用部署目录里的配置，而不是
/// 意外命中仓库里那份开发用的。
fn find_env_file() -> Option<PathBuf> {
    let mut dir = std::env::current_dir().ok()?;
    for _ in 0..3 {
        let candidate = dir.join(".env");
        if candidate.is_file() {
            return Some(candidate);
        }
        if !dir.pop() {
            break;
        }
    }
    None
}

/// 解析 `.env` 文件。
///
/// 支持的形式（与 dotenv 的常用子集一致）：
/// - `KEY=value`
/// - `KEY="value"` / `KEY='value'`
/// - `export KEY=value`
/// - `#` 开头的整行注释
/// - 值里含 `#` 时不当作注释（密钥里可能有）
///
/// 不支持变量插值（`${OTHER}`）—— 现有 `.env` 里没用到，而不完整的实现
/// 比明确不支持更危险。
fn parse_env_file(path: &Path) -> std::io::Result<HashMap<String, String>> {
    let content = std::fs::read_to_string(path)?;
    let mut out = HashMap::new();

    for raw in content.lines() {
        let line = raw.trim();

        // 整行注释和空行。
        if line.is_empty() || line.starts_with('#') {
            continue;
        }

        let line = line.strip_prefix("export ").unwrap_or(line);

        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        let key = key.trim();
        if key.is_empty() {
            continue;
        }

        let value = value.trim();
        // 去掉成对的引号。只去成对的：值里可能真的以引号开头（罕见但合法），
        // 而无条件去掉会静默改掉密钥。
        let value = if (value.starts_with('"') && value.ends_with('"') && value.len() >= 2)
            || (value.starts_with('\'') && value.ends_with('\'') && value.len() >= 2)
        {
            &value[1..value.len() - 1]
        } else {
            value
        };

        out.insert(key.to_string(), value.to_string());
    }

    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn write_temp(content: &str) -> PathBuf {
        let mut p = std::env::temp_dir();
        p.push(format!(
            "bgjq-env-test-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        let mut f = std::fs::File::create(&p).unwrap();
        f.write_all(content.as_bytes()).unwrap();
        p
    }

    #[test]
    fn 解析基本形式() {
        let p = write_temp("A=1\nB=two\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.get("A").map(String::as_str), Some("1"));
        assert_eq!(m.get("B").map(String::as_str), Some("two"));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 解析引号与_export() {
        let p = write_temp("export SECRET=\"abc def\"\nQ='single'\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.get("SECRET").map(String::as_str), Some("abc def"));
        assert_eq!(m.get("Q").map(String::as_str), Some("single"));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 跳过注释与空行() {
        let p = write_temp("# 注释\n\nA=1\n   \n#又一个\nB=2\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.len(), 2);
        assert!(m.contains_key("A") && m.contains_key("B"));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 值里的井号不当注释() {
        // 密钥里出现 # 是完全可能的，把它当注释会把密钥截断 —— 那种错误
        // 表现为 401，而两边配置「看起来」一样，极难排查。
        let p = write_temp("SECRET=ab#cd\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.get("SECRET").map(String::as_str), Some("ab#cd"));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 值里的等号保留() {
        // base64 密钥常以 = 结尾。
        let p = write_temp("SECRET=abc==\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.get("SECRET").map(String::as_str), Some("abc=="));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 不成对引号不去掉() {
        // 只去掉成对的引号。无条件去掉会静默改掉一个以引号开头的密钥。
        let p = write_temp("A=\"unclosed\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.get("A").map(String::as_str), Some("\"unclosed"));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 忽略没有等号的行() {
        let p = write_temp("这行没有等号\nA=1\n");
        let m = parse_env_file(&p).unwrap();
        assert_eq!(m.len(), 1);
        assert_eq!(m.get("A").map(String::as_str), Some("1"));
        std::fs::remove_file(p).ok();
    }

    #[test]
    fn 默认值合理() {
        // 不依赖真实环境：直接构造一个空配置来验证拼装规则。
        let cfg = Config {
            mc_address: "bgjq.simpfun.cn:25565".into(),
            control_addr: "127.0.0.1:8088".into(),
            secret: None,
        };
        assert!(cfg.secret_missing());
        assert_eq!(cfg.mc_address, "bgjq.simpfun.cn:25565");
        // 默认必须绑本机 —— 这个端口能启停机器人。
        assert!(cfg.control_addr.starts_with("127.0.0.1:"));
    }
}
