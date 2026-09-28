//! 8W 邦国崛起机器人节点。
//!
//! ## runtime 的划分
//!
//! 控制面（WebSocket 服务）跑普通的多线程 runtime —— axum 本来就要求连接
//! 任务能 `Send`。机器人**不在**这个 runtime 上：Azalea 的 `Swarm` 持有
//! `Arc<RwLock<World>>` 而 `World` 不是 `Sync`，它的连接 future 不是 `Send`。
//!
//! 每个机器人因此有自己的线程和自己的 `current_thread` runtime，见
//! `manager.rs` 文件头的说明。控制面用通道跟它们说话。
//!
//! ## 配置
//!
//! 从 `.env` 和进程环境变量读取，变量名与原来的 JS 节点一致，现有那份
//! `.env` 可以直接用：
//!
//! - `MC_HOST` / `MC_PORT`  游戏服务器，默认 `bgjq.simpfun.cn` / `25565`
//! - `WS_PORT`              控制端口，默认 8088（只绑 127.0.0.1）
//! - `INTERNAL_NODE_SECRET` 与 Go 后端共享的密钥，**不填则任何人可控制机器人**
//!
//! 详见 `config.rs`。监听地址默认只绑本机：这个端口能启停机器人、能代替
//! 它们说话，不该暴露。

use bgjq_bot::config::Config;
use bgjq_bot::manager::Manager;
use bgjq_bot::server::{self, AppState};
use std::time::Duration;
use tracing::info;
use tracing_subscriber::EnvFilter;

fn main() {
    // 先读配置再初始化日志 —— 顺序不能反。
    //
    // `.env` 里的 `RUST_LOG` 由 `Config::load` 写回进程环境，而
    // `EnvFilter::try_from_default_env()` 要在那之后才读得到它。顺序反过来
    // 就是「我明明设了 debug 却什么都看不到」。
    //
    // 代价是配置载入那几条日志（「已载入 .env」）看不到 —— 可以接受，
    // 它们在排查配置问题时才有用，而那时改的是别的东西。
    let cfg = Config::load();

    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| EnvFilter::new("info,azalea=warn,bevy=warn")),
        )
        .init();

    if cfg.secret_missing() {
        // 用 error 而不是 warn：这不是「有点问题」，是任何能连到这个端口
        // 的人都可以冒充机器人说话、启停它们。绑在 127.0.0.1 上降低了
        // 风险，但反代通常会把公网流量引过来。
        tracing::error!(
            "INTERNAL_NODE_SECRET 未设置 —— 控制端口不受鉴权保护，任何能连上它的人都能操控机器人"
        );
    }

    info!(
        mc = %cfg.mc_address,
        control = %cfg.control_addr,
        auth = cfg.secret.is_some(),
        "配置就绪"
    );

    // 多线程：控制面只有网络 IO，不碰 Azalea 的 !Send 状态（每个机器人
    // 在自己的线程上，见 manager.rs）。
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("无法建立 tokio runtime");

    rt.block_on(async move {
        if let Err(e) = run(cfg).await {
            tracing::error!("节点退出: {e}");
            std::process::exit(1);
        }
    });
}

async fn run(cfg: Config) -> anyhow::Result<()> {
    let manager = Manager::new(cfg.mc_address.clone());
    let state = AppState::new(manager.clone());

    let listener = tokio::net::TcpListener::bind(&cfg.control_addr).await?;
    info!(mc = %cfg.mc_address, control = %cfg.control_addr, "机器人节点已启动");

    // 收到信号时把机器人一个个停掉再退出。
    //
    // 不停就退出的后果不只是「进程没了」：机器人还挂在服务器上，而服务端的
    // 登录缓存是按 IP 的，下一次启动会被那条僵死的连接顶掉，表现为反复上下线。
    let shutdown_manager = manager.clone();
    let shutdown = async move {
        shutdown_signal().await;
        let n = shutdown_manager.stop_all();
        info!(count = n, "收到退出信号，正在停止所有机器人");
        // 等机器人线程真的退出：连接任务被 drop 时才会发出断开包，不等就
        // 退进程会让服务端等到超时才清会话 —— 而它的登录缓存按 IP 计，
        // 下次启动会被那条僵死的会话顶掉。
        //
        // 放到阻塞线程上等，否则会把控制面的 runtime 卡住。
        let waiter = shutdown_manager.clone();
        let _ = tokio::task::spawn_blocking(move || {
            waiter.join_all(Duration::from_secs(5));
        })
        .await;
    };

    axum::serve(listener, server::router(state))
        .with_graceful_shutdown(shutdown)
        .await?;

    info!("节点已停止");
    Ok(())
}

/// 等到 Ctrl-C 或 SIGTERM。
///
/// SIGTERM 也要处理：用 systemd 或容器管理时，停止信号是 SIGTERM 而不是
/// Ctrl-C，只监听后者会让「停止」变成直接杀进程，机器人来不及下线。
async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c().await.expect("无法监听 Ctrl-C");
    };

    #[cfg(unix)]
    let terminate = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("无法监听 SIGTERM")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {}
        _ = terminate => {}
    }
}
