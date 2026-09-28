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
//! ## 环境变量
//!
//! - `MC_ADDRESS`   游戏服务器地址，默认 `bgjq.simpfun.cn:25565`
//! - `CONTROL_ADDR` 本地监听地址，默认 `127.0.0.1:8088`
//!
//! 监听地址默认只绑本机：这个端口能启停机器人、能代替它们说话，不该暴露。

use bgjq_bot::manager::Manager;
use bgjq_bot::server::{self, AppState};
use std::time::Duration;
use tracing::info;
use tracing_subscriber::EnvFilter;

/// 游戏服务器地址。
const DEFAULT_ADDRESS: &str = "bgjq.simpfun.cn:25565";

/// 控制端口。绑 127.0.0.1 而不是 0.0.0.0 —— 见文件头。
const DEFAULT_CONTROL: &str = "127.0.0.1:8088";

fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| EnvFilter::new("info,azalea=warn,bevy=warn")),
        )
        .init();

    let address = std::env::var("MC_ADDRESS").unwrap_or_else(|_| DEFAULT_ADDRESS.to_string());
    let control = std::env::var("CONTROL_ADDR").unwrap_or_else(|_| DEFAULT_CONTROL.to_string());

    // 多线程：控制面只有网络 IO，不碰 Azalea 的 !Send 状态（每个机器人
    // 在自己的线程上，见 manager.rs）。
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("无法建立 tokio runtime");

    rt.block_on(async move {
        if let Err(e) = run(address, control).await {
            tracing::error!("节点退出: {e}");
            std::process::exit(1);
        }
    });
}

async fn run(address: String, control: String) -> anyhow::Result<()> {
    let manager = Manager::new(address.clone());
    let state = AppState::new(manager.clone());

    let listener = tokio::net::TcpListener::bind(&control).await?;
    info!(%address, %control, "机器人节点已启动");

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
