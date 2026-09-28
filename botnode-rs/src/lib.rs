//! 8W 邦国崛起机器人节点。
//!
//! 这一层替代原来的 `8wbotforJavaScript`：负责连接游戏服务器、驱动机器人
//! 走路与开拓、把状态推给 Go 后端。做成 lib + bin 是因为状态解析和通信契约
//! 都值得脱离网络单独测 —— 原 JS 版把 `status.js` 单独拆出来也是这个理由。

pub mod bot;
pub mod claimqueue;
pub mod config;
pub mod manager;
pub mod protocol;
pub mod server;
pub mod status;
