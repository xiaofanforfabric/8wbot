#!/usr/bin/env node
/**
 * 聊天通道诊断脚本
 *
 * 用途：定位「机器人能进服，但一发消息就被踢」的原因。
 *
 * 背景：本地用 Paper / Paper+强制签名 / Paper+Velocity代理 / 原版 四种配置
 * 测试过，客户端都正常。但在 enforcesSecureChat=true 的服务端上会出现
 * 「能进服 + 每一条聊天都被拒 + 命令正常」的现象，与线上症状一致。
 * 本脚本用来确认线上服务器到底属于哪种情况。
 *
 * 用法（在 JS 节点目录下执行）：
 *   node diagnose-chat.js
 *   node diagnose-chat.js --host bgjq.simpfun.cn --port 25565 --version 1.21.4 --name 诊断机器人
 *
 * 只做三件事：连上去 → 发一条普通聊天 → 发一条命令。不做任何其他操作。
 * 全程只打印信息，不写文件、不改配置。
 */

const path = require('path')
try { require('dotenv').config({ path: path.join(__dirname, '.env') }) } catch (_) {}

let mineflayer
try {
  mineflayer = require('mineflayer')
} catch (e) {
  console.error('❌ 找不到 mineflayer，请在 8wbotforJavaScript 目录下执行')
  process.exit(1)
}

// ── 参数 ────────────────────────────────────────────────
function arg (name, fallback) {
  const i = process.argv.indexOf('--' + name)
  return i !== -1 && process.argv[i + 1] ? process.argv[i + 1] : fallback
}

const HOST = arg('host', process.env.MC_HOST || 'bgjq.simpfun.cn')
const PORT = parseInt(arg('port', process.env.MC_PORT || '25565'), 10)
const VERSION = arg('version', process.env.MC_VERSION || '1.21.4')
const NAME = arg('name', 'chatdiag' + Math.floor(Math.random() * 9000 + 1000))
const CHAT_TEXT = 'diagnose chat test'

// 把任意形状的 NBT/聊天组件转成可读文本（主要给踢出原因用）
function flatten (c, depth) {
  depth = depth || 0
  if (depth > 12) return ''
  if (c === null || c === undefined) return ''
  if (typeof c === 'string') return c
  if (typeof c === 'number' || typeof c === 'boolean') return String(c)
  if (Buffer.isBuffer(c)) return c.toString('utf8')
  if (Array.isArray(c)) return c.map(x => flatten(x, depth + 1)).join('')
  if (typeof c === 'object') {
    if (c.type !== undefined && c.value !== undefined) return flatten(c.value, depth + 1)
    if (c.value !== undefined && c.text === undefined && c.translate === undefined) {
      return flatten(c.value, depth + 1)
    }
    if (c.text !== undefined) return flatten(c.text, depth + 1) + flatten(c.extra, depth + 1)
    let s = ''
    if (c.translate !== undefined) {
      s = String(c.translate)
      const args = Array.isArray(c.with) ? c.with : []
      args.forEach((a, i) => { s = s.replace('%' + (i + 1) + '$s', flatten(a, depth + 1)) })
      args.forEach(a => { s = s.replace('%s', flatten(a, depth + 1)) })
    }
    if (c.extra !== undefined) s += flatten(c.extra, depth + 1)
    return s
  }
  return String(c)
}

// 收到的消息优先用 mineflayer 解析好的 ChatMessage 渲染，失败再退回 flatten
function render (msg) {
  try {
    if (msg && typeof msg.toString === 'function') {
      const s = msg.toString()
      if (s && s.trim()) return s.trim()
    }
  } catch (_) {}
  return flatten(msg).trim()
}

const line = () => console.log('─'.repeat(60))

console.log('')
line()
console.log('  聊天通道诊断')
line()
console.log('  服务器 :', `${HOST}:${PORT}`)
console.log('  版本   :', VERSION)
console.log('  用户名 :', NAME)
console.log('  依赖   : mineflayer', require('mineflayer/package.json').version,
  '| minecraft-protocol', require('minecraft-protocol/package.json').version)
line()
console.log('')

const state = {
  secureChat: null,
  spawned: false,
  connectError: null,
  chatEcho: null,
  cmdReplies: [],
  kickReason: null,
  sentChat: false,
  sentCmd: false,
  resourcePack: null,
  packAnswered: false
}

const bot = mineflayer.createBot({
  host: HOST,
  port: PORT,
  username: NAME,
  version: VERSION,
  auth: 'offline'
})

// ① 登录包里的 enforcesSecureChat —— 这是最关键的一行
bot._client.on('login', (packet) => {
  if (packet && packet.enforcesSecureChat !== undefined) {
    state.secureChat = packet.enforcesSecureChat
  }
})

// ② 资源包：服务器会吊着 configuration 阶段直到客户端应答。
//    mineflayer 只抛事件、不会自动应答，不处理就会永远卡在配置阶段进不了游戏。
bot.on('resourcePack', (url, hash) => {
  state.resourcePack = String(url || '')
  console.log('  📦 收到资源包（阶段:', bot._client.state + '）:', state.resourcePack)
  const action = (process.env.RESOURCE_PACK_ACTION || 'accept').toLowerCase()
  try {
    if (action === 'deny') { bot.denyResourcePack() } else { bot.acceptResourcePack() }
    state.packAnswered = true
    console.log('  📦 已应答资源包（', action, '）')
  } catch (e) {
    console.log('  📦 应答失败:', e.message)
  }
})

bot.on('message', (msg) => {
  const t = render(msg)
  if (!t) return
  console.log('  💬 收到:', t.slice(0, 120))

  // 服务器广播出来的自己的聊天（形如 <名字> 内容）
  if (state.sentChat && t.includes(CHAT_TEXT)) state.chatEcho = t
  if (state.sentCmd) state.cmdReplies.push(t)
})

bot.once('spawn', () => {
  state.spawned = true
  console.log('  ✅ 已 spawn（能正常进服）')
  console.log('')

  // ② 发一条普通聊天
  setTimeout(() => {
    state.sentChat = true
    console.log('  → 发送普通聊天:', JSON.stringify(CHAT_TEXT))
    try { bot.chat(CHAT_TEXT) } catch (e) { console.log('     ❌ bot.chat 抛异常:', e.message) }
    console.log('')
  }, 2500)

  // ③ 发一条命令（原版会返回帮助菜单，说明命令通道是通的）
  setTimeout(() => {
    state.sentCmd = true
    console.log('  → 发送命令: "/help"')
    try { bot.chat('/help') } catch (e) { console.log('     ❌ bot.chat 抛异常:', e.message) }
    console.log('')
  }, 6500)

  setTimeout(finish, 13000)
})

bot.on('kicked', (reason) => {
  state.kickReason = flatten(reason) || String(reason)
  console.log('')
  console.log('  ⛔ 被服务器踢出！原因:', JSON.stringify(state.kickReason))
})

bot.on('error', (e) => {
  if (!state.connectError) state.connectError = e.message
  console.log('  ❌ 连接错误:', e.message)
})

bot.on('end', (reason) => {
  if (!state.kickReason && reason && String(reason) !== 'disconnect.quitting' && String(reason) !== 'socketClosed') {
    state.kickReason = flatten(reason) || String(reason)
  }
  setTimeout(finish, 300)
})

let finished = false
function finish () {
  if (finished) return
  finished = true

  const chatOk = !!state.chatEcho
  const cmdOk = state.cmdReplies.length > 0

  console.log('')
  line()
  console.log('  诊断结果')
  line()
  console.log('  1. 是否进服成功      :', state.spawned
    ? '✅ 是'
    : (state.connectError ? `❌ 否（${state.connectError}）` : '❌ 否'))
  console.log('  2. enforcesSecureChat:', state.secureChat === null
    ? '（服务端未提供该字段）'
    : (state.secureChat ? '⚠️ true —— 服务端强制要求正版聊天签名' : '✅ false'))
  if (state.resourcePack) {
    console.log('  2b. 资源包           :', state.packAnswered
      ? `✅ 已应答（${state.resourcePack.slice(0, 60)}）`
      : `⚠️ 收到但未应答（${state.resourcePack.slice(0, 60)}）`)
  }
  console.log('  3. 普通聊天是否送达  :', chatOk
    ? `✅ 是（服务器广播了 ${JSON.stringify(state.chatEcho)}）`
    : (state.kickReason && state.kickReason !== 'socketClosed'
        ? '❌ 否（被服务器拒绝/踢出）'
        : '❌ 否（服务器没有广播我们的消息）'))
  console.log('  4. 命令通道是否正常  :', cmdOk
    ? `✅ 是（收到 ${state.cmdReplies.length} 条 /help 回复）`
    : '❌ 否（没有收到命令回复）')
  if (state.kickReason) {
    console.log('  5. 踢出原因          :', JSON.stringify(state.kickReason))
  }

  console.log('')
  line()
  console.log('  如何解读')
  line()

  if (!state.spawned) {
    if (state.resourcePack && !state.packAnswered) {
      console.log('  ⚠️ 收到了资源包但没有应答，机器人卡在 configuration 阶段！')
      console.log('     → 服务器会一直吊着该阶段直到客户端应答资源包。')
      console.log('     → 这正是「机器人进不去游戏 / 一发消息就异常」的典型原因。')
      console.log('     → 修复：在 resourcePack 事件里调用 bot.acceptResourcePack()。')
    } else {
      console.log('  ⚠️ 根本没进服 —— 先确认地址/端口/版本，或账号是否已被别处占用。')
    }
  } else if (state.secureChat === true) {
    console.log('  ⚠️ 服务端 enforcesSecureChat=true，聊天被拒但命令正常：')
    console.log('     → 离线（非正版）账号没有 Mojang 签名密钥，无法在这种服务器上聊天。')
    console.log('     → 这是服务端配置问题，客户端无解。')
    console.log('     → 解决：server.properties 设 enforce-secure-profile=false，')
    console.log('        并检查代理（Velocity/BungeeCord）的对应配置。')
  } else if (!chatOk && cmdOk) {
    console.log('  ⚠️ 命令正常但聊天被拒 —— 服务端（多半是插件）单独拦截了聊天。')
    console.log('     → 需要服务器控制台的报错堆栈才能继续定位。')
  } else if (chatOk) {
    console.log('  ✅ 聊天通道正常 —— 说明当前代码在这台服务器上可用。')
    console.log('     若仍偶发被踢，请把当时的完整控制台日志发出来。')
  } else {
    console.log('  ⚠️ 聊天和命令都失败：')
    console.log('     → 可能没登录（需要 /login）、被插件限制，或账号被占用。')
  }
  console.log('')

  try { bot.quit() } catch (_) {}
  setTimeout(() => process.exit(0), 300)
}

setTimeout(finish, 45000)
