/**
 * 机器人掉线诊断脚本 —— 独立文件，不修改 index.js 的任何逻辑
 *
 * 用法（在 8wbotforJavaScript 目录下执行，以便找到 node_modules）：
 *     node diagnose-bot.js <机器人名字>
 * 例：
 *     node diagnose-bot.js xiaofanbot
 *
 * 它会做三件事：
 *   1. 用底层协议直连，检测服务器是否为「正版验证」(online-mode)
 *      —— 正版验证的服务器会拒绝非正版账号，这是「连上就被踢」最常见的原因
 *   2. 用 mineflayer 完整走一遍登录，把 kicked / error / end / spawn 全部打出来
 *   3. 解析服务器返回的原始踢出文本（含颜色代码），原样展示
 *
 * 脚本会在拿到结论后自动退出，不会常驻，也不会自动重连。
 */

const mineflayer = require('mineflayer')
const mc = require('minecraft-protocol')

const HOST = process.env.MC_HOST || 'bgjq.simpfun.cn'
const PORT = parseInt(process.env.MC_PORT) || 25565
const VERSION = process.env.MC_VERSION || '1.21.4'
const NAME = process.argv[2] || 'xiaofanbot'

const t0 = Date.now()
const ts = () => `[+${((Date.now() - t0) / 1000).toFixed(1)}s]`

// ── 把 MC 的聊天组件转成纯文本 ──
function flatten (msg) {
  if (msg == null) return ''
  if (typeof msg === 'string') return msg
  if (Array.isArray(msg)) return msg.map(flatten).join('')
  let out = ''
  if (msg.text) out += msg.text
  if (msg.extra) out += flatten(msg.extra)
  if (msg.translate) out += msg.translate
  if (msg.with) out += flatten(msg.with)
  return out
}

console.log('═'.repeat(64))
console.log(`诊断目标: ${NAME} @ ${HOST}:${PORT}  版本 ${VERSION}`)
console.log('═'.repeat(64))

// ══════════════════════════════════════════════════════════
// 第一步：底层协议嗅探 —— 判断正版验证 / 协议号 / 是否被踢
// ══════════════════════════════════════════════════════════
console.log('\n【第一步】底层协议嗅探（判断正版验证与踢出原因）')

const client = mc.createClient({
  host: HOST,
  port: PORT,
  username: NAME,
  version: VERSION,
  auth: 'offline'
})

let sawEncryption = false
let loginStage = 'connecting'

client.on('connect', () => {
  loginStage = 'tcp 已连接'
  console.log(`  ${ts()} TCP 连接成功`)
})

// 收到加密请求 = 服务器开启正版验证，非正版账号必然被拒
client.on('encryption_request', () => {
  sawEncryption = true
  console.log(`  ${ts()} ⚠️ 服务器要求加密（encryption_request）→ 这是【正版验证】服务器`)
})

client.on('login', () => {
  loginStage = '登录成功'
  console.log(`  ${ts()} ✅ 登录成功（通过了正版验证/或为离线模式）`)
})

// 服务器踢人：这里是真正的踢出原因
client.on('kick_disconnect', (packet) => {
  const reason = flatten(packet && packet.reason)
  console.log(`  ${ts()} ⛔ 被服务器踢出 [kick_disconnect]`)
  console.log(`        原因原文: ${JSON.stringify(reason)}`)
})

client.on('disconnect', (packet) => {
  const reason = flatten(packet && packet.reason)
  console.log(`  ${ts()} ⛔ 被服务器断开 [disconnect]`)
  console.log(`        原因原文: ${JSON.stringify(reason)}`)
})

client.on('error', (err) => {
  console.log(`  ${ts()} ❌ 协议层错误: ${err && err.message}`)
})

client.on('end', (reason) => {
  console.log(`  ${ts()} 🔌 协议层连接结束, reason = ${JSON.stringify(reason)}`)
  console.log(`        (注意: 这个 reason 通常是 socketClosed，不含踢出原因)`)
  console.log(`        是否正版验证: ${sawEncryption ? '是（非正版账号会被拒）' : '否（离线模式）'}`)
  console.log(`        登录阶段: ${loginStage}`)
  startMineflayer()
})

// ══════════════════════════════════════════════════════════
// 第二步：mineflayer 完整走一遍，看应用层的表现
// ══════════════════════════════════════════════════════════
function startMineflayer () {
  console.log('\n【第二步】mineflayer 应用层行为\n')

  const bot = mineflayer.createBot({
    host: HOST,
    port: PORT,
    username: NAME,
    version: VERSION,
    auth: 'offline'
  })

  let spawned = false

  bot.once('spawn', () => {
    spawned = true
    console.log(`  ${ts()} ✅ spawned —— 成功进入游戏！`)
    console.log(`        位置: ${JSON.stringify(bot.entity && bot.entity.position)}`)
    console.log(`        游戏版本: ${bot.version}`)
  })

  // 服务器踢出时的真实原因
  bot.on('kicked', (reason, loggedIn) => {
    console.log(`  ${ts()} ⛔ kicked（真实踢出原因）loggedIn=${loggedIn}`)
    console.log(`        原因原文: ${JSON.stringify(flatten(reason))}`)
  })

  bot.on('error', (err) => {
    console.log(`  ${ts()} ❌ error: ${err && err.message}`)
    console.log(`        code=${err && err.code}`)
  })

  bot.on('end', (reason) => {
    console.log(`  ${ts()} 🔌 end, reason = ${JSON.stringify(reason)}`)
    if (!spawned) {
      console.log('\n  ⚠️ 结论: 机器人在 spawn 之前就断开了 —— 说明卡在登录阶段，')
      console.log('     常见原因: 正版验证拒绝 / 名字非法 / 白名单 / IP 限制 / 需要 /login')
    }
    report()
  })

  // 把服务器发来的所有聊天/系统消息打出来，登录提示就藏在这里
  bot.on('message', (jsonMsg) => {
    const text = jsonMsg.toString()
    if (text && text.trim()) console.log(`  ${ts()} 💬 [服务器消息] ${text}`)
  })

  bot.on('chat', (who, msg) => {
    console.log(`  ${ts()} 💬 [聊天] ${who}: ${msg}`)
  })

  // 40 秒还没结论就收工
  setTimeout(() => {
    console.log(`\n  ${ts()} ⏱ 观察结束（40 秒）`)
    if (spawned) console.log('  ✅ 机器人在线且稳定，未掉线')
    try { bot.quit() } catch (_) {}
    report()
  }, 40000)
}

let reported = false
function report () {
  if (reported) return
  reported = true
  console.log('\n' + '═'.repeat(64))
  console.log('诊断结束。请把上面完整输出发给管理员。')
  console.log('═'.repeat(64))
  setTimeout(() => process.exit(0), 500)
}

// 兜底退出
setTimeout(() => { console.log('\n⏱ 总超时，退出'); process.exit(0) }, 90000)
