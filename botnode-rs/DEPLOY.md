# 部署到内地 E5

这份说明对应「香港跑 Go 后端、内地 E5 跑机器人节点」的分工。E5 上只需要
一个二进制文件和两条环境变量。

## 1. 前置：Rust 工具链

**只有要自己编译才需要。** 如果直接用编译好的二进制，跳到第 3 步。

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
rustup toolchain install nightly-2026-08-01 --profile minimal
```

**工具链必须钉在 `nightly-2026-08-01`。** Azalea 依赖的 `simdnbt` 用了
`#![feature(portable_simd)]`，所以必须 nightly；而更新的 nightly 编不过
`azalea-core` 的 `bitset.rs`（`FixedBitSet<N>` 常量推断变了，报
`E0284 type annotations needed`，试过 `2026-09-28` 确认复现）。

## 2. 编译

```bash
git clone https://github.com/xiaofanforfabric/8wbot.git
cd 8wbot/botnode-rs
rustup override set nightly-2026-08-01
cargo build --release
```

产物：`target/release/bgjq-bot`（约 40 MB，已 strip）。

## 3. 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `INTERNAL_NODE_SECRET` | **是** | 与香港 Go 后端的同名变量填**同一个值** |
| `MC_ADDRESS` | 否 | 默认 `bgjq.simpfun.cn:25565` |
| `CONTROL_ADDR` | 否 | 默认 `127.0.0.1:8088` |
| `RUST_LOG` | 否 | 默认 `info,azalea=warn,bevy=warn` |

`INTERNAL_NODE_SECRET` 不填的话节点会照常运行并打警告，但**任何人都能启停
你的机器人** —— 这个端口能代替机器人说话。

## 4. systemd 服务

```ini
# /etc/systemd/system/bgjq-bot.service
[Unit]
Description=8W 邦国崛起机器人节点
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=bgjq
WorkingDirectory=/opt/bgjq-bot
EnvironmentFile=/opt/bgjq-bot/env
ExecStart=/opt/bgjq-bot/bgjq-bot
Restart=always
RestartSec=5

# 收到 SIGTERM 时把机器人一个个停掉再退出。节点自己处理这个信号 ——
# 不停就退出的后果是机器人还挂在服务器上，而服务端的登录缓存按 IP 计，
# 下次启动会被那条僵死的会话顶掉，表现为反复上下线。
KillSignal=SIGTERM
TimeoutStopSec=15

# 加固
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```bash
# /opt/bgjq-bot/env
INTERNAL_NODE_SECRET=和香港那边一致的值
MC_ADDRESS=bgjq.simpfun.cn:25565
CONTROL_ADDR=127.0.0.1:8088
RUST_LOG=info,azalea=warn,bevy=warn
```

```bash
chmod 600 /opt/bgjq-bot/env     # 里面有密钥
systemctl daemon-reload
systemctl enable --now bgjq-bot
journalctl -u bgjq-bot -f
```

## 5. 香港 Go 后端要改的

```bash
JS_NODE_URL=wss://js.xiaofanai.uk      # 已经指向这个域名，不用改
INTERNAL_NODE_SECRET=和 E5 那边一致的值  # 必须两边一样
```

**如果 `CONTROL_ADDR` 保持 `127.0.0.1`（推荐），前面需要一个反向代理把
公网 `wss://js.xiaofanai.uk` 转到本机 8088。** 原来 JS 节点跑在 8889 端口
并被 Cloudflare 挡着，现在换成 8088 的话反代规则要跟着改，或者把
`CONTROL_ADDR` 设成 `127.0.0.1:8889` 复用现有规则。

## 6. 验证

从香港那台机器上（或者任何能访问 E5 的机器）：

```bash
# 密钥对：应回 200
websocat "wss://js.xiaofanai.uk/ws/api/botstatus?secret=$INTERNAL_NODE_SECRET"
# 再发：{"username":"xiaofanbot"}

# 密钥错：应回 {"code":401,...}
websocat "wss://js.xiaofanai.uk/ws/api/botstatus?secret=wrong"
```

预期：第一条连接先收到 `{"code":200,"message":"ready"}`，发请求后收到状态。

## 7. 容量

目标是一台 8H4G 跑 20 个机器人。debug 版实测：空载 17 MB，每个机器人
约 46 MB。release 版会更低。20 个机器人另有 20 个线程（每个机器人一个，
因为 Azalea 的连接 future 不是 `Send`）。

按 debug 数字的悲观上界：`17 + 20 × 46 ≈ 937 MB`，在 4 GB 预算内。

## 8. 排障

| 现象 | 原因 |
|---|---|
| `{"code":401}` | 两边的 `INTERNAL_NODE_SECRET` 不一致 |
| Go 报「节点不可达」 | 反代没通，或 `CONTROL_ADDR` 和反代规则不一致 |
| `{"code":409}` | 该机器人已在运行；Go 会自动杀掉旧实例重试 |
| 机器人反复上下线 | 同一个账号在两个地方连（登录缓存按 IP 计） |
| `set_objective ... Invalid root type 2` | 服务器用了 26.x 的计分板写法，crates.io 上的 azalea 0.16（协议 26.1）解析不了。**只影响计分板，不断连** |
| 机器人掉进虚空 | 天空岛边缘。寻路器拒绝跳超过 3 格的落差，但目标不可达时会放弃该块继续下一块 |
