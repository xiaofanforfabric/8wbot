# bgjq-bot-rs

8W 邦国崛起机器人节点。替代原来的 `8wbotforJavaScript`，用 Rust + Azalea
实现，**提供完全相同的控制接口**，因此 Go 后端不需要改一行。

## 为什么换掉 JS 那层

原来的 Node 版基于 mineflayer，物理不完整，被 GrimAC 持续告警：

| | mineflayer (JS) | understudy (Go) | xiaofanbot (Java) | **Azalea (Rust)** |
|---|---|---|---|---|
| 过 Velocity | ✅ | ✅ | ✅ | ✅ **实测** |
| 垂直物理 | ❌ | ❌ 恒高 | ✅ | ✅ **实测 `vy=-0.0784`** |
| 寻路 | ✅ | ❌ | ✅ | ✅ **实测（含挖穿障碍）** |
| 协议 | 1.21.4 | 1.21.4 | 原生 | **26.2 = 服务器对外版本** |
| GrimAC | ❌ 报 `Simulation 4.13` | ❌ 报 `TickTimer` | ✅ | ✅ **零告警（实测）** |
| 内存/个 | ~100 MB | ~30 MB | ~500 MB | 见下 |
| 20 个在 4 GB 上 | ✅ | ✅ | ❌ ~8 个 | 见下 |

Java 路线是算术上死的：`8H4G ÷ 500 MB ≈ 8`，而需求是 20。xiaofanbot 必须
占一个完整 MC 客户端，无法单独运行。

## 实测记录（2026-09-28，真实服务器）

用 `xiaofanbot` 账号连 `bgjq.simpfun.cn:25565`：

```
EVENT login / EVENT spawn
CHAT <server> 欢迎回来！您的身份已通过缓存自动验证。
START x=-7031.22 y=168.00 z=-9452.73 on_ground=true
  t=    0ms  vy=-0.0784  on_ground=true
  t= 2000ms  y=167.54   on_ground=false  vy=-0.0784   ← 踩空
  t= 3000ms  y=146.72   on_ground=false  vy=-1.5544   ← 加速
  t= 4000ms  y=105.30   on_ground=false  vy=-2.3723
CHAT <server> xiaofanbot fell from a high place
```

**「摔死」是成功的证据，不是失败。** `vy` 逐帧累加（`-0.0784 → -1.5544 →
-2.3723`）说明那是真实的重力积分；服务器主动报出死因，说明服务端和 Azalea
各自独立算出了同一个结果。对照 understudy：它在同一个悬崖上 y 永远不变，
永远不会摔死 —— 那种客户端在服务器看来就是浮空行走。

第二轮用 `start_goto` 走到指定坐标，**途中自己挖开了挡路的方块**（A\* 把
「挖开这一格的代价」算进路径成本，代价取自 `best_tool_in_hotbar_for_block`
和方块硬度），**全程 GrimAC 零告警**。

## 控制接口

端口默认 `127.0.0.1:8088`，实现 Go 后端 `goservers/wsclient.go` 期望的
全部端点。完整契约见 `MIGRATION.md`。

| 端点 | 作用 |
|---|---|
| `/ws/api/startbot` | 启动；连接保持打开用于推日志 |
| `/ws/api/stopbot` | 停止 |
| `/ws/api/botstatus` | 查询状态 |
| `/ws/api/botlogs` | 订阅日志流 |
| `/ws/api/sendinfo` | 下发指令（聊天） |

握手顺序：服务端先发一帧 `{"code":200,...}`，然后等请求。Go 侧拨通后会先
读一条（忽略内容），这一帧就是给它读的，避免它白等满 3 秒。

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `MC_ADDRESS` | `bgjq.simpfun.cn:25565` | 游戏服务器地址 |
| `CONTROL_ADDR` | `127.0.0.1:8088` | 控制端口。**默认只绑本机** |
| `INTERNAL_NODE_SECRET` | 空 | 内部节点密钥。**生产必须设置** |
| `RUST_LOG` | `info,azalea=warn,bevy=warn` | 日志级别 |

### 鉴权

`INTERNAL_NODE_SECRET` 与 Go 后端的同名环境变量一致。Go 侧通过
`?secret=` 附加（见 `wsclient.go: getJSNodeURL`），也接受
`X-Internal-Secret` 头。

**不设置它等于把控制权开放给任何人** —— 这个接口能启停机器人、能代替它们
说话。为空时会打一条警告，不会静默放行。

鉴权失败时的行为与原 JS 节点一致：**升级照常完成，然后发一帧
`{"code":401}` 再关闭**。不改成直接拒绝升级，是因为 Go 侧读的是 WS 帧里的
`code`，看到 HTTP 401 会把「密钥不对」报成「节点不可达」，反而更难排查。

密钥比较用定长时间实现（`constant_time_eq`）：用 `==` 会在第一个不同的字节
处提前返回，攻击者能靠响应时间逐字节试出密钥。

控制端口不应直接暴露到公网。生产上是 Cloudflare 代理 443 →
`js.xiaofanai.uk`。

## 构建

需要 **nightly Rust**：Azalea 依赖的 `simdnbt` 用了 `#![feature(portable_simd)]`。

```bash
rustup toolchain install nightly-2026-08-01 --profile minimal
rustup override set nightly-2026-08-01
cargo build --release
```

**工具链要钉在 `nightly-2026-08-01`。** 更新的 nightly（试过 `2026-09-28`）
编不过 `azalea-core` 的 `bitset.rs`：`FixedBitSet<N>` 的常量推断变了，报
`E0284 type annotations needed`。

## 测试

```bash
cargo test              # 25 个单元测试 + 12 个控制面端到端测试
```

带外到外的是 `tests/control_api.rs`：它起一个真实节点，用真实 WebSocket
客户端跑完整握手，覆盖「重复启动必须回 409」这条 Go 侧依赖的关键行为。
它**不连游戏服务器** —— 机器人的实际行为由真实服务器上的手工验证覆盖。

## 架构要点

### 每个机器人一个线程

Azalea 的 `Swarm` 持有 `Arc<RwLock<World>>`，`World` 不是 `Sync`，所以建立
连接的 future 不是 `Send`。它既不能 `tokio::spawn` 到多线程 runtime，也不能
`spawn_local` 到别处 —— `axum::serve` 对每条连接用 `tokio::spawn`
（`axum/src/serve/mod.rs:276`），连接任务跑在 `LocalSet` 之外。

所以：每个机器人一个专属线程 + `current_thread` runtime，控制面用 channel
跟它说话。控制面本身跑普通多线程 runtime。

### 事件只注册一次

早期 JS 实现里，每来一个 `/botlogs` 连接就往 bot 上重挂一遍监听器且 close
时不摘除，于是开 N 个控制台就挂 5N 个监听器，同一条聊天转发 N 次，内存只涨
不降。这里改成订阅广播通道，订阅者退出时接收端自然 drop。

### 不轮询重设目标

原 JS 注释记过这个坑：反复重设计划让移动包节奏剧烈抖动，在反作弊眼里比正常
走路可疑得多。Azalea 的 `start_goto` 自带重规划，直接调用即可。

### 不 abort 连接任务

停止是「置位停止标志 → 关指令通道 → 等线程退出」。硬 abort 会让服务端等到
超时才清会话，而它的登录缓存按 IP 计，下次启动会被那条僵死的会话顶掉。

## 目录

```
src/
  main.rs      入口、runtime 划分、信号处理
  manager.rs   机器人管理（启动/停止/世代计数）
  bot.rs       单个机器人：Azalea 接入、聊天处理、寻路
  server.rs    WebSocket 端点
  protocol.rs  与 Go 后端的报文契约
  status.rs    服务端文本解析（/u info、/server、疆土开拓）
tests/
  control_api.rs  控制面端到端测试
```

## 实测：控制面与内存

用最小 WebSocket 客户端跑完整流程，全部通过：

| 操作 | 结果 |
|---|---|
| `startbot` | `200` + 实时推事件（登录、中文聊天、进服广播） |
| `startbot` 重复 | `409` ← Go 的 `startBotReplacingStale` 依赖这个 |
| `botstatus` | `online=true`、位置实时更新、维度 `overworld` |
| `botlogs` | 首帧带完整状态 |
| `sendinfo` | `200`，聊天真的发到服务器 |
| `stopbot` | `200`，之后 `online=false` |
| 停止后重启 | `200`，能正常重连 |
| 错误密钥 | `401`（帧内） |
| 无密钥 | `401`（帧内） |
| `X-Internal-Secret` 头 | `200`，放行 |

聊天真的到达服务器（服务器日志）：
```
<8w社区> [雅典维亚城邦（二世）] [xiaofan机器人] xiaofanbot: Rust 节点：带密钥鉴权的正式测试
```

**内存（debug 版实测）**：空载 17 MB，1 个机器人 63 MB。

**连续启停 5 轮**：63 → 68 MB，线程数稳定在 7-8。每轮约 +1 MB 且不加速，
线程数不增长 —— 这是 glibc malloc 保留空闲页的行为，不是泄漏。但仍需关注
20 个机器人频繁重启时的累积。

> release 版的内存数字待补。上面的 debug 数字是上界：未优化、带调试符号。

## 踩过的坑（都已在代码里修掉，留档以免重犯）

### `spawn_local` 在 axum handler 里会 panic

最初的写法是「整个节点跑一个 `LocalSet`，机器人用 `spawn_local` 启动」。
单元测试立刻炸了：`axum::serve` 对每条连接用 `tokio::spawn`
（`axum/src/serve/mod.rs:276`），连接任务跑在 `LocalSet` 之外。改为每个
机器人一个线程 + 自己的 `current_thread` runtime。

### `packet-event` 默认开启，会淹没事件通道

Azalea 默认把**每一个**收到的包都变成 `Event::Packet`。实测后果：

```
WARN azalea_client::client: GameTick is more than 10 ticks behind
WARN azalea::swarm: The client's Event channel has more than 1,000 items!
```

事件通道爆掉之后，控制面的请求会卡住不返。关掉这个特性后恢复正常。这个
节点只要聊天、tick、连接状态，不需要每个包。

### `GameTick` 落后会打乱移动包节奏

上面的 `GameTick is more than 10 ticks behind, skipping ticks so we don't
have to burst too much` 是 GrimAC 眼里的危险信号 —— 它意味着移动包的间隔
不均。关掉 `packet-event` 后这条不再出现。

### `set_objective` 解析失败

```
ERROR azalea_client::plugins::connection:
  Error reading packet set_objective (id 106): Invalid root type 2
```

服务器用了 `set_objective` 的某种写法（大概是 26.x 新增的 root type），
crates.io 上的 `azalea 0.16`（协议 26.1）解析不了。**只影响计分板，不导致
断连**，机器人照常走路说话。要修的话得换 git `main`。

## 已知问题

- 用的是 crates.io 的 `azalea 0.16`（协议 26.1），而服务器对外是 26.2。
  实测能进、能玩、零告警；如果要原生 26.2，把依赖换成 git `main`。
- `online-mode` 特性被关掉（见 `Cargo.toml` 注释）。这只影响微软正版账号
  的会话服务 —— 本节点用离线账号（`Account::offline`），真正的鉴权由服务端
  的 SimpPass 做。**如果将来要用正版账号，必须把这个特性开回来。**
