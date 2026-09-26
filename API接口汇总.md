# 邦国崛起 API 接口汇总

> 来源：https://open.bgjq.top/api-docs.html
> 整理说明：V1 与 V2 是**两套相互独立、互不相通**的接口系统，需分别在「我的密钥」页面申请对应 Key。
> 仅供参考查阅，便于快速定位接口，无需逐个打开网页。

---

## 〇、通用说明

| 项目 | 内容 |
|------|------|
| 服务性质 | **只读查询服务**，不会修改服务器任何数据 |
| 数据格式 | `application/json; charset=utf-8`，支持 CORS 跨域 |
| 鉴权 | 除健康检查外，所有查询接口需携带 **API Key**（请求头二选一） |
| 响应错误 | 统一 `{"error":"错误说明"}`，状态码 `400/401/403/404/429/500` |
| 时间字段 | 输出 ISO 8601；空值输出 null |
| 列表控制 | 列表类接口支持 `?limit=N`（默认 100） |
| 缓存 | 查询响应带缓存，数据可能有最长 1 小时延迟 |

**鉴权方式（请求头二选一）：**
```bash
# 方式一
Authorization: Bearer <你的 KEY>
# 方式二
X-API-Key: <你的 KEY>
```
未携带或 Key 错误返回 `401 {"error":"missing or invalid api key"}`。

---

## 一、基础地址

- **V1 服务地址：** `https://bgjq.simpfun.cn/api/`
- **V2 服务地址：** `https://bgjq.simpfun.cn/api-v2/`（仅支持 GET）

---

# V1 接口（/api/）

## 1. 健康检查（免鉴权）

| 方法 | 接口 | 说明 |
|------|------|------|
| GET | `/api/health` | 返回 `{"status":"ok"}`，连通性测试 |

快速体验：
```bash
curl https://bgjq.simpfun.cn/api/health
```

---

## 2. 玩家

| 方法 | 接口 | 说明 |
|------|------|------|
| GET | `/api/info/players` | 所有玩家列表，含所属邦国（id、名称）、职位、职业、称号、上下线时间等 |
| GET | `/api/info/players/{key}` | 单个玩家详情，`{key}` 可为 **UUID** 或 **游戏名** |
| GET | `/api/info/players/{key}/union` | 玩家所属邦国完整信息；玩家无邦国时返回 404 |

---

## 3. 邦国

| 方法 | 接口 | 说明 |
|------|------|------|
| GET | `/api/info/unions` | 所有邦国列表，含君主、成员数、领土区块数、领土面积、特产、当前生效科技等 |
| GET | `/api/info/unions/{key}` | 单个邦国详情，`{key}` 可为 **数字 id** 或 **邦国名** |
| GET | `/api/info/unions/{key}/members` | 邦国成员列表，含职位（OWNER/ADMIN/MEMBER）、职业等 |
| GET | `/api/info/unions/{key}/territory` | 邦国领土区块明细：区块坐标、忠诚度、占领时间 |
| GET | `/api/info/unions/{key}/tech` | 邦国已激活（未过期）科技：效果、等级、到期时间 |
| GET | `/api/info/unions/{key}/contributions` | 邦国捐献记录，按时间倒序，可用 `?limit=` 控制条数 |

---

## 4. 全服数据

| 方法 | 接口 | 说明 |
|------|------|------|
| GET | `/api/info/territory` | 全服所有领土区块，可用 `?union_id=数字` 过滤 |
| GET | `/api/info/buffs` | 全服当前生效的科技/加成 |
| GET | `/api/info/diplomacy` | 邦国外交关系（grantor → grantee，权限级别） |
| GET | `/api/info/bans` | 封禁记录，可用 `?limit=` 控制条数 |
| GET | `/api/info/logs` | 玩家上下线等日志，可用 `?limit=` 控制条数 |
| GET | `/api/info/announcements` | 公告历史，可用 `?limit=` 控制条数 |

---

## 5. 请求示例

```bash
# 查询所有邦国
curl "https://bgjq.simpfun.cn/api/info/unions" \
  -H "Authorization: Bearer <你的 KEY>"

# 查询单个玩家（按游戏名）
curl "https://bgjq.simpfun.cn/api/info/players/Steve" \
  -H "X-API-Key: <你的 KEY>"

# 查询邦国领土区块
curl "https://bgjq.simpfun.cn/api/info/unions/1/territory" \
  -H "Authorization: Bearer <你的 KEY>"
```

---

# V2 接口（/api-v2/）

> 由 ServerOpenAPI 插件提供（nginx 将插件本地端口转发为本地址），用于查询**服务器实时状态**。
> 数据格式 `application/json`，**仅支持 GET**。

| 方法 | 接口 | 说明 | 接口级限速 |
|------|------|------|-----------|
| GET | `/api-v2/players` | 在线玩家列表 | 10 QPM |
| GET | `/api-v2/players/{name}/equipment` | 玩家装备与手持物品 | 30 QPM |
| GET | `/api-v2/time` | 游戏内时间 | 10 QPM |
| GET | `/api-v2/dragon` | 末影龙是否存活 | 1 QPM |

**限速说明：**
- 同时受 **接口级固定配额**（全局共享，见上各接口 QPM）与 **密钥级配额**（平台发放默认 30 QPM，最高 60）两重限速
- 任一超限返回 `429`（带 `Retry-After` 响应头）

---

# 字段映射说明

| API 字段 | 数据来源 |
|----------|----------|
| 玩家 ↔ 邦国 | `players.union_id` → `unions.id` |
| 领土 | `chunks` 表按 `union_id` 汇总（区块数） |
| 特产 | `unions.special_output_material` |
| 科技 | `union_buffs`（仅未过期） |
| 捐献 | `union_contributions` |
| 外交 | `union_diplomacy`（READ_ONLY / ALL_ACCESS / DENY） |

**领土面积换算：**
- 领土区块数为**权威口径**
- 领土面积 = 区块数 × 256（每个区块 16×16 方块），仅供换算参考
- 区块坐标 x/z 为 Minecraft 区块坐标，换算方块坐标：`blockX = x*16`、`blockZ = z*16`

---

# 限流与注意事项（V1）

- 所有 `/info/*` 查询接口按 **Key × 接口** 进行每分钟限流
- 全服数据类接口（territory / buffs / diplomacy / bans / logs / announcements）**限流较严**
- 超限返回 `429 {"error":"... rate limit exceeded"}`，请稍后重试
- **同一 IP 在 1 小时内密钥错误超过 3 次** 将被临时封锁，请核对 Key 后重试
- 路径参数 `{key}`：玩家支持 UUID 或游戏名；邦国支持数字 id 或邦国名
- 请合理使用接口，避免高频请求；Key 请妥善保管，勿泄露
