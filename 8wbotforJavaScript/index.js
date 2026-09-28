require('dotenv').config();

// launcher.js — 启动器，通过 WS /ws/api/startbot 启动机器人并实时推送日志
const mineflayer = require('mineflayer');
// 扩地寻路。这里用 mineflayer-pathfinder：Node 侧没有真 Baritone，
// 而网页操控扩地是「逐块走近距离」，这个插件够用。
const { pathfinder, goals } = require('mineflayer-pathfinder');
const WebSocket = require('ws');
const url = require('url');
const fs = require('fs');
const path = require('path');
const ST = require('./status');
const { ClaimQueue, orderSquare, parseClaimNotify, CHUNK_TIMEOUT_SEC, REPATH_AFTER_SEC } =
  require('./claimqueue');

// ════════════════════════════════════════════════════════════════
// 上次退出原因记录
//
// 背景: mineflayer 的 end 事件 reason 来自底层 socket，永远是
// 'socketClosed'，真正的原因（被踢/抢占登录/掉线）只在 kicked 事件里。
// 这里把真实原因记录下来并落盘，供后台与前端查询。
// ════════════════════════════════════════════════════════════════
const EXIT_LOG_FILE = path.join(__dirname, 'bot-exit-reasons.json');

let exitReasons = {};
try {
  exitReasons = JSON.parse(fs.readFileSync(EXIT_LOG_FILE, 'utf8')) || {};
} catch (_) {
  exitReasons = {};
}

// 把聊天组件压平成纯文本。
// MC 1.20.3+ 的踢出原因是 NBT 格式（prismarine-nbt），形如:
//   { type:'compound', value:{ text:{ type:'string', value:'§c原因...' } } }
// 旧版本则是 JSON 聊天组件: { text:'...', extra:[...] }
// 这里把三种形态（纯字符串 / JSON 组件 / NBT）统一处理。
function flattenChat(msg) {
  if (msg == null) return '';
  if (typeof msg === 'string') return msg;
  if (typeof msg === 'number' || typeof msg === 'boolean') return String(msg);
  if (Buffer.isBuffer(msg)) return msg.toString('utf8');
  if (Array.isArray(msg)) return msg.map(flattenChat).join('');

  if (typeof msg === 'object') {
    // NBT 包装节点: { type:'compound'|'string'|'list'|..., value:... }
    if ('type' in msg && 'value' in msg) {
      return flattenChat(msg.value);
    }
    let out = '';
    if (msg.text !== undefined) out += flattenChat(msg.text);
    if (msg.translate !== undefined) out += flattenChat(msg.translate);
    if (msg.extra !== undefined) out += flattenChat(msg.extra);
    // 仅在没有任何可读文本时才退回 with，避免重复拼接
    if (!out && msg.with !== undefined) out += flattenChat(msg.with);
    return out;
  }
  return '';
}

// 去掉 Minecraft 颜色/格式代码（§c、§l 等），便于阅读与入库
function stripMinecraftColors(text) {
  return String(text || '').replace(/\u00a7[0-9a-fk-orx]/gi, '').trim();
}

/**
 * 发送前净化聊天/命令文本。
 *
 * 原版 SharedConstants.isAllowedChatCharacter 只接受
 *   c !== '\u00a7' && c >= ' ' && c !== '\u007f'
 * 也就是说 § 分节符、所有控制字符、DEL 都是非法字符。服务器一旦收到就会
 * 直接踢人（multiplayer.disconnect.illegal_characters），表现为
 * 「在控制台发一条消息，机器人立刻掉线」。
 *
 * 这里在写给服务器之前剔除非法字符，避免整只机器人被踢下线。
 * 换行符保留：mineflayer 会按 '\n' 拆成多条消息发送，不会进到包体里。
 *
 * @param {*} text 原始文本
 * @returns {{text: string, removed: number}} 净化后文本 + 被剔除的字符数
 */
function sanitizeOutgoingChat(text) {
  const raw = String(text == null ? '' : text).replace(/\r\n?/g, '\n');
  const cleaned = raw
    .replace(/\u00a7[0-9a-fk-orx]/gi, '')                        // 颜色/格式代码整体去掉（§c -> 空）
    .replace(/[\u0000-\u0009\u000b-\u001f\u007f\u00a7]/g, '');   // 再清掉残留的非法字符
  return { text: cleaned, removed: raw.length - cleaned.length };
}

/**
 * 记录某个机器人的退出原因
 * @param {string} username
 * @param {string} reason  原始原因文本
 * @param {string} type    kicked(被服务器踢) | error(连接错误) | stopped(主动下线) | end(连接结束)
 */
function recordExitReason(username, reason, type) {
  const text = stripMinecraftColors(flattenChat(reason)) || '未知原因';
  exitReasons[username] = {
    reason: text,
    type: type || 'unknown',
    time: new Date().toISOString()
  };
  try {
    fs.writeFileSync(EXIT_LOG_FILE, JSON.stringify(exitReasons, null, 2));
  } catch (e) {
    console.warn(`[${username}] 退出原因落盘失败:`, e && e.message);
  }
  console.log(`[${username}] 记录退出原因 [${type}]: ${text}`);
}

function getExitReason(username) {
  return exitReasons[username] || null;
}

const INTERNAL_NODE_SECRET = process.env.INTERNAL_NODE_SECRET || '';
if (!INTERNAL_NODE_SECRET) {
  console.warn('⚠️ 警告: INTERNAL_NODE_SECRET 未配置，节点鉴权处于未保护状态！');
} else {
  console.log('🔒 内部通信安全鉴权已启用 (INTERNAL_NODE_SECRET 已生效)');
}

// 目标游戏服务器（可通过 .env 覆盖，便于换服或本地测试）
const SERVER_HOST = process.env.MC_HOST || 'bgjq.simpfun.cn';
const SERVER_PORT = parseInt(process.env.MC_PORT) || 25565;
const SERVER_VERSION = process.env.MC_VERSION || '1.21.4';

const bots = {}; // username -> bot instance

/** username -> ClaimQueue。网页操控扩地，每个机器人一条独立队列。 */
const claimQueues = new Map();

/** 玩家当前所在区块。JS 的 >> 对负数会先截断，必须先 floor 才是 Java 的 getBlockPos()>>4 语义 */
function playerChunkOf(bot) {
  if (!bot || !bot.entity || !bot.entity.position) return null;
  const p = bot.entity.position;
  return [Math.floor(p.x) >> 4, Math.floor(p.z) >> 4];
}

/** 扩地进度/结果推给订阅者 */
function broadcastExpand(username, ev) {
  broadcast(username, [{ botname: username, data: [{ expand: ev }] }]);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * 走到指定区块的中心。移植自参考实现的 moveToChunkWithBaritone：
 * 设目标 → 每秒查一次是否已在该区块 → 寻路断了就重设目标 → 超时或中断返回 false。
 */
async function walkToChunk(bot, username, cx, cz) {
  // 区块中心方块坐标
  const bx = cx * 16 + 8;
  const bz = cz * 16 + 8;
  const q = claimQueues.get(username);

  if (!bot.pathfinder) {
    console.error(`[${username}] pathfinder 未加载，无法前往 (${cx},${cz})`);
    return false;
  }

  try {
    bot.pathfinder.setGoal(new goals.GoalXZ(bx, bz));
  } catch (e) {
    console.error(`[${username}] setGoal 失败: ${e.message}`);
    return false;
  }

  const deadline = Date.now() + CHUNK_TIMEOUT_SEC * 1000;
  let waited = 0;

  while (Date.now() < deadline) {
    await sleep(1000);
    waited++;
    if (!bot.entity) return false;
    if (q && !q.running) return false; // 被中断

    const pc = playerChunkOf(bot);
    if (pc && pc[0] === cx && pc[1] === cz) {
      try { bot.pathfinder.setGoal(null); } catch (_) {}
      return true;
    }

    // 寻路中断（卡住/被取消）就重设目标。刚起步的前几秒不重设，
    // 否则 setGoal 还没生效就被误判为中断。
    if (waited > REPATH_AFTER_SEC) {
      let moving = false;
      try { moving = bot.pathfinder.isMoving(); } catch (_) {}
      if (!moving) {
        console.log(`[${username}] 前往 (${cx},${cz}) 寻路中断，重设目标`);
        try { bot.pathfinder.setGoal(new goals.GoalXZ(bx, bz)); } catch (_) {}
      }
    }
  }

  try { bot.pathfinder.setGoal(null); } catch (_) {}
  console.warn(`[${username}] 前往 (${cx},${cz}) 超时（${CHUNK_TIMEOUT_SEC}s）`);
  return false;
}

// ════════════════════════════════════════════════════════════════
// 事件总线
//
// 以前每个 /ws/api/botlogs 连接都会往 bot 上重新挂一遍 bot.on(...)，
// 而且 close 时不摘除 —— 那段注释写着 "Remove listeners by re-assigning
// noop"，下面其实是空的。后果：开 N 次控制台就挂 5N 个监听器，同一条
// 聊天被转发 N 次，Node 还会报 MaxListenersExceededWarning，内存只涨不降。
//
// 现在改成：bot 的事件在 startBot 里只注册一次，统一投到总线，再由总线
// 扇出给所有订阅者。顺带解决另一个问题 —— 原来事件只发给「启动它的那条
// 连接」，所以自动重连巡护拉起的机器人不属于任何连接，谁也不知道它在干嘛。
// ════════════════════════════════════════════════════════════════
const subscribers = new Map(); // username -> Set<ws>
// 订阅了 {all:true} 的全局连接，新机器人启动时需要主动纳入
const eventWatchers = new Set();

function subscribeBot(username, ws) {
  let set = subscribers.get(username);
  if (!set) { set = new Set(); subscribers.set(username, set); }
  set.add(ws);
}

// 连接关闭时按连接整体摘除，避免残留
function unsubscribeWs(ws) {
  for (const [name, set] of subscribers) {
    set.delete(ws);
    if (set.size === 0) subscribers.delete(name);
  }
}

function broadcast(username, payload) {
  const set = subscribers.get(username);
  if (!set || set.size === 0) return;
  const msg = JSON.stringify(payload);
  for (const ws of set) {
    if (ws.readyState === 1) {
      try { ws.send(msg); } catch (_) {}
    }
  }
}

// 通知订阅了 {all:true} 的全局连接「有新机器人起来了」。
// 否则常驻订阅者只能看到它订阅那一刻已经存在的机器人。
function notifyBotStarted(username) {
  for (const ws of eventWatchers) {
    if (ws.readyState === 1 && ws._watchAll && typeof ws._onBotStarted === 'function') {
      try { ws._onBotStarted(username); } catch (_) {}
    }
  }
}

// ════════════════════════════════════════════════════════════════
// 状态采集：位置 / 维度 / 所在服务器 / 邦国信息
//
// 位置和维度直接从 mineflayer 读，几乎零开销；
// 所在服务器靠发 /server 问，输出是原版翻译消息 —— 机器人 locale 是
// en_US（mineflayer 默认），所以收到的是英文 "You are currently
// connected to main."，但中文客户端会看到中文，这里两种都匹配。
// 邦国信息靠发 /u info，那 14 行是插件自己吐的中文原文，机器人收到的
// 也是中文；为了不赌语言，解析按「键: 值」结构走，不硬编码字段名。
// ════════════════════════════════════════════════════════════════
const STATUS_POS_MS = parseInt(process.env.STATUS_POS_MS) || 5000;
const STATUS_SERVER_MS = parseInt(process.env.STATUS_SERVER_MS) || 60000;
const STATUS_KINGDOM_MS = parseInt(process.env.STATUS_KINGDOM_MS) || 600000;
const KINGDOM_COLLECT_MS = 4000; // 发完 /u info 后收多少毫秒的回复

const emptyStatus = ST.emptyStatus;

function pushStatus(username, patch) {
  const bot = bots[username];
  if (!bot) return;
  if (!bot._status) bot._status = emptyStatus();
  Object.assign(bot._status, patch, { updatedAt: new Date().toISOString() });
  broadcast(username, [{ botname: username, data: [{ status: bot._status }] }]);
}

// 处理一条进来的聊天消息，抽取状态信息。
// 返回 true 表示这条消息属于邦国信息块。
function handleStatusMessage(username, bot, text) {
  if (!bot._status) bot._status = emptyStatus();

  // ── /server 的回复 ──
  const server = ST.parseServer(text);
  if (server) {
    bot._currentServer = server;
    pushStatus(username, { server: server });
    if (bot._serverCheckResolve) {
      bot._serverCheckResolve(server);
      bot._serverCheckResolve = null;
    }
  }

  // ── /u info 的回复：累积整个块再解析 ──
  const title = ST.parseKingdomHead(text);
  if (title !== null) {
    bot._kingdomBuf = { title: title, raw: [text] };
    if (bot._kingdomTimer) clearTimeout(bot._kingdomTimer);
    bot._kingdomTimer = setTimeout(() => finishKingdom(username), KINGDOM_COLLECT_MS);
    return true;
  }

  if (bot._kingdomBuf) {
    bot._kingdomBuf.raw.push(text);
    if (ST.isKingdomEnd(text)) {
      if (bot._kingdomTimer) clearTimeout(bot._kingdomTimer);
      finishKingdom(username);
    }
    return true;
  }

  return false;
}

function finishKingdom(username) {
  const bot = bots[username];
  if (!bot || !bot._kingdomBuf) return;
  const buf = bot._kingdomBuf;
  bot._kingdomBuf = null;
  if (bot._kingdomTimer) { clearTimeout(bot._kingdomTimer); bot._kingdomTimer = null; }

  const parsed = ST.parseKingdomBlock(buf.title, buf.raw);
  if (!parsed) return; // 残缺块，丢弃

  parsed.updatedAt = new Date().toISOString();
  pushStatus(username, { kingdom: parsed });
  console.log(`[${username}] 邦国信息已更新: ${parsed.title}（${Object.keys(parsed.fields).length} 个字段）`);
}

// 为某个机器人启动周期性采集。重复调用是幂等的。
function startStatusCollector(username) {
  const bot = bots[username];
  if (!bot || bot._statusTimer) return;
  if (!bot._status) bot._status = emptyStatus();

  const tick = () => {
    const b = bots[username];
    if (!b) return stopStatusCollector(username);
    try {
      const p = b.entity && b.entity.position;
      const dim = (b.game && b.game.dimension) || null;
      const pos = p ? { x: Math.floor(p.x), y: Math.floor(p.y), z: Math.floor(p.z) } : null;
      const prev = b._status || {};
      if (JSON.stringify(prev.pos) !== JSON.stringify(pos) || prev.dimension !== dim) {
        pushStatus(username, { pos: pos, dimension: dim });
      }
    } catch (_) {}
  };

  const askServer = () => {
    const b = bots[username];
    if (!b) return;
    try { b.chat('/server'); } catch (_) {}
  };

  const askKingdom = () => {
    const b = bots[username];
    if (!b) return;
    try { b.chat('/u info'); } catch (_) {}
  };

  tick();
  bot._statusTimer = {
    pos: setInterval(tick, STATUS_POS_MS),
    server: setInterval(askServer, STATUS_SERVER_MS),
    kingdom: setInterval(askKingdom, STATUS_KINGDOM_MS),
  };
  // 进游戏后先各问一次，别让卡片空等一个周期
  setTimeout(() => { askServer(); }, 3000);
  setTimeout(() => { askKingdom(); }, 8000);
}

function stopStatusCollector(username) {
  const bot = bots[username];
  if (!bot || !bot._statusTimer) return;
  const t = bot._statusTimer;
  clearInterval(t.pos);
  clearInterval(t.server);
  clearInterval(t.kingdom);
  bot._statusTimer = null;
  if (bot._kingdomTimer) { clearTimeout(bot._kingdomTimer); bot._kingdomTimer = null; }
}

let titleSelectedIndex = 0;
const titleControls = ['Start', 'Options', 'Quit'];

function showTitleScreen() {
  console.log('标题屏幕，使用 Tab 切换选项，按 Enter 确认');
  console.log('可用控件：' + titleControls.join(' | '));
  try {
    process.stdin.setRawMode(true);
    process.stdin.resume();
    process.stdin.on('data', (key) => {
      const k = key.toString();
      if (k === '\u0003') { process.exit(); }
      if (k === '\t') {
        titleSelectedIndex = (titleSelectedIndex + 1) % titleControls.length;
        console.log('Selected:', titleControls[titleSelectedIndex]);
      } else if (k === '\r') {
        const sel = titleControls[titleSelectedIndex];
        console.log('Confirmed:', sel);
        if (sel === 'Start') startBot('wans7891');
        else if (sel === 'Quit') process.exit();
      }
    });
  } catch (e) {
    console.error('Title screen error:', e && e.message);
  }
}

// Start a single bot and return it
function startBot(username) {
  if (bots[username]) return null; // already exists
  console.log(`Starting bot: ${username} -> ${SERVER_HOST}:${SERVER_PORT}`);
  const bot = mineflayer.createBot({
    host: SERVER_HOST,
    port: SERVER_PORT,
    username: username,
    version: SERVER_VERSION
  });
  bots[username] = bot;
  notifyBotStarted(username);

  // ── 扩地：加载寻路插件并建立队列 ──
  try {
    bot.loadPlugin(pathfinder);
  } catch (e) {
    console.warn(`[${username}] 寻路插件加载失败，扩地将不可用: ${e.message}`);
  }
  claimQueues.set(
    username,
    new ClaimQueue(bot, {
      walkToChunk: (cx, cz) => walkToChunk(bot, username, cx, cz),
      playerChunk: () => playerChunkOf(bot),
      isUsable: () => !!bots[username] && !!bot.entity,
      onEvent: (ev) => broadcastExpand(username, ev),
    })
  );

  // ── 资源包应答（必须处理，否则机器人根本进不了游戏）──
  // MC 1.20.3 起服务器会在 configuration 阶段推送资源包，并且「一直吊着该阶段
  // 直到客户端给出应答」。mineflayer 只抛出 resourcePack 事件、不会自动应答，
  // 所以不处理就永远卡在配置阶段：spawn 不触发、进不了游戏，此时发消息必然
  // 触发服务器异常（有时报成 An internal error occurred in your connection.）。
  // Velocity 每次切换服务器都会重新进入 configuration 阶段，本服务器正是
  // Velocity + SimpPass 多后端结构，因此影响被放大。
  bot.on('resourcePack', (url, hash) => {
    const action = (process.env.RESOURCE_PACK_ACTION || 'accept').toLowerCase();
    try {
      if (action === 'deny') {
        bot.denyResourcePack();
        console.log(`[${username}] 已拒绝资源包（RESOURCE_PACK_ACTION=deny）: ${url}`);
      } else {
        // 只回复应答包，不会真的下载资源包，无额外流量开销
        bot.acceptResourcePack();
        console.log(`[${username}] 已应答资源包: ${url}`);
      }
    } catch (e) {
      console.warn(`[${username}] 资源包应答失败: ${e.message}`);
    }
  });

  // ── 退出原因记录（挂在 startBot 里，任何通道连接都能生效）──
  // _exitRecorded 保证原因只记录一次，且优先保留更具体的原因：
  //   kicked（被服务器踢，含抢占登录）> error（连接错误）> end（socketClosed）
  bot._exitRecorded = false;

  bot.on('kicked', (reason, loggedIn) => {
    const text = flattenChat(reason);
    recordExitReason(username, text, 'kicked');
    bot._exitRecorded = true;
    // 抢占登录的典型提示，单独标注便于识别
    if (/another location|already logged|重复登录|已在其他地方/i.test(text)) {
      console.warn(`[${username}] ⚠️ 疑似被其他位置的同账号登录顶下线`);
    }
  });

  bot.on('error', (err) => {
    if (!bot._exitRecorded) {
      recordExitReason(username, (err && err.message) || '未知连接错误', 'error');
      bot._exitRecorded = true;
    }
  });

  bot.on('end', (reason) => {
    if (!bot._exitRecorded) {
      // 说明没收到 kicked/error，reason 通常就是无意义的 socketClosed
      recordExitReason(username, reason || 'socketClosed', 'end');
      bot._exitRecorded = true;
    }
    // 必须先清采集器（它要靠 bots[name] 找定时器），再摘掉实例。
    // 这个 delete 不能省：少了它，机器人掉线后 startbot 会一直回 409
    // 「已有实例在运行」，用户就再也拉不起来了。
    stopStatusCollector(username);
    delete bots[username];

    const why = reason || '未知';
    broadcast(username, [{ botname: username, data: [{ chat: `[系统] ${username} 已断开连接: ${why}` }] }]);
    broadcast(username, [{ botname: username, data: [{ bot_offline: true, reason: why }] }]);
  });

  // ── 单一 message 处理器：/server 检测 + 邦国信息累积 + 广播 ──
  // 以前这里只做 /server 匹配，而聊天广播挂在各个连接分支里（每来一条
  // 连接就挂一份，且从不摘除）。现在统一到这里，只挂一次。
  bot._currentServer = null;
  bot._serverCheckResolve = null;
  bot._status = emptyStatus();

  bot.on('message', (jsonMsg) => {
    const text = jsonMsg.toString();
    try { handleStatusMessage(username, bot, text); } catch (e) {
      console.warn(`[${username}] 状态解析异常: ${e.message}`);
    }
    // 服务器开拓成功通知：疆土 (cx, cz) [bx, bz] 已为你的邦国开拓！
    // 扩地队列靠它确认某区块真的开拓成功了。
    try {
      const notify = parseClaimNotify(text);
      if (notify) {
        const q = claimQueues.get(username);
        if (q) q.markClaimed(notify.cx, notify.cz);
      }
    } catch (e) {
      console.warn(`[${username}] 开拓通知解析异常: ${e.message}`);
    }
    broadcast(username, [{ botname: username, data: [{ chat: `[消息] ${text}` }] }]);
  });

  bot.on('chat', (who, message) => {
    broadcast(username, [{ botname: username, data: [{ chat: `[聊天] ${who}: ${message}` }] }]);
  });

  // ── 地图数据（含二维码大图）──
  // 同样从各连接分支收归到这里，避免重复注册
  const emitMap = (b64, width) => {
    sendMapIfNotMain(username, b64, width, (b, w2) => {
      broadcast(username, [{ botname: username, data: [{ mapdata: b, width: w2 }] }]);
    });
  };
  bot.on('map', (map) => {
    if (map && map.data && map.data.length > 0) {
      emitMap(map.data.toString('base64'), map.data.length > 20000 ? 256 : 128);
    }
  });
  bot._client.on('map', (data) => {
    if (data && data.data && data.columns > 0 && data.rows > 0) {
      const buf = Buffer.isBuffer(data.data) ? data.data : Buffer.from(data.data);
      if (buf.length > 0) emitMap(buf.toString('base64'), data.columns);
    }
  });

  // ── 状态采集（位置/维度/服务器/邦国）──
  startStatusCollector(username);

  return bot;
}

// Check which server the bot is on, then call sendFn(b64, width).
// Large maps (>=128x128) are always sent (QR code).
// Small maps are only sent if bot is NOT on "main" server.
function sendMapIfNotMain(username, b64, width, sendFn) {
  const bot = bots[username];
  if (!bot) { sendFn(b64, width); return; }

  // Large map = QR code, always send regardless of server
  const raw = Buffer.from(b64, 'base64');
  const height = width > 0 ? Math.floor(raw.length / width) : 0;
  if (width >= 128 && height >= 128) {
    console.log(`[${username}] large map (${width}x${height}), sending unconditionally`);
    sendFn(b64, width);
    return;
  }

  // Small maps: check server first — only send if NOT on main
  if (bot._serverCheckResolve) {
    console.log(`[${username}] server check in progress, skipping duplicate map data`);
    return;
  }

  const timeout = setTimeout(() => {
    if (bot._serverCheckResolve) {
      bot._serverCheckResolve('timeout');
      bot._serverCheckResolve = null;
    }
  }, 5000);

  bot._serverCheckResolve = (server) => {
    clearTimeout(timeout);
    if (server === 'main') {
      console.log(`[${username}] on main server, discarding small map data`);
    } else {
      console.log(`[${username}] on ${server} server, sending small map data`);
      sendFn(b64, width);
    }
  };

  bot.chat('/server');
}

function stopBot(username) {
  const bot = bots[username];
  if (!bot) return;
  // 主动下线：直接写明原因，避免被 end 的 socketClosed 覆盖
  recordExitReason(username, '用户主动下线', 'stopped');
  bot._exitRecorded = true;
  // 必须先于 delete bots[username] —— 采集器要靠 bots[name] 找到定时器
  stopStatusCollector(username);
  try { bot.quit(); } catch (e) { /* ignore */ }
  delete bots[username];
  console.log(`Bot stopped: ${username}`);
}

// WebSocket server — routing by path
const WS_PORT = parseInt(process.env.WS_PORT) || 8889;
try {
  const wss = new WebSocket.Server({ port: WS_PORT });
  // Heartbeat: ping every 30s, terminate if no pong
  function heartbeat() { this.isAlive = true; }
  const heartbeatTimer = setInterval(() => {
    wss.clients.forEach((ws) => {
      if (ws.isAlive === false) {
        console.log('WS client heartbeat timeout, terminating');
        return ws.terminate();
      }
      ws.isAlive = false;
      ws.ping();
    });
  }, 30000);
  wss.on('close', () => clearInterval(heartbeatTimer));

  wss.on('connection', (ws, req) => {
    ws.isAlive = true;
    ws.on('pong', heartbeat);

    const parsedUrl = url.parse(req.url || '/', true);
    const pathname = parsedUrl.pathname || '/';
    const clientSecret = parsedUrl.query.secret || req.headers['x-internal-secret'] || '';

    // ─── 安全鉴权检查 ───
    if (INTERNAL_NODE_SECRET) {
      if (!clientSecret || clientSecret !== INTERNAL_NODE_SECRET) {
        console.warn(`⛔ [Auth] 拒绝未经授权的 WS 握手: ${pathname} 来自 ${req.socket.remoteAddress}`);
        ws.send(JSON.stringify({ code: 401, message: 'Unauthorized: invalid or missing internal secret' }));
        ws.close(4001, 'Unauthorized');
        return;
      }
    }

    console.log(`✅ [Auth] 节点握手鉴权通过: ${pathname} 来自 ${req.socket.remoteAddress}`);
    const path = pathname;

    // ─── /ws/api/startbot ───
    if (path === '/ws/api/startbot') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to 8wbot startbot API' }));
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) {
          ws.send(JSON.stringify({ code: 400, message: 'invalid JSON' })); return;
        }
        const username = msg.username;
        if (!username) { ws.send(JSON.stringify({ code: 400, message: 'username required' })); return; }

        if (bots[username]) {
          ws.send(JSON.stringify({ code: 409, message: '机器人已启动并已连接到服务器，请勿重复启动！' }));
          return;
        }

        const bot = startBot(username);
        if (!bot) { ws.send(JSON.stringify({ code: 500, message: '创建机器人实例失败' })); return; }

        ws.send(JSON.stringify({ code: 200, message: '机器人已成功启动并连接到服务器' }));

        // 事件全部由 startBot 里的总线统一注册，这里只订阅，不再逐个 bot.on
        subscribeBot(username, ws);
        ws.on('close', () => unsubscribeWs(ws));

        bot.once('spawn', () => {
          console.log(`[${username}] spawned`);
          broadcast(username, [{ botname: username, data: [{ chat: `[系统] ${username} 已进入游戏` }] }]);
          // Select first hotbar slot (map is there)
          try {
            bot.setQuickBarSlot(0);
            console.log(`[${username}] selected hotbar slot 0`);
          } catch (e) {
            console.log(`[${username}] setQuickBarSlot error:`, e.message);
          }
        });
      });
    }

    // ─── /ws/api/sendinfo ───
    else if (path === '/ws/api/sendinfo') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to 8wbot sendinfo API' }));
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) {
          ws.send(JSON.stringify({ code: 400, message: 'invalid JSON' })); return;
        }
        const botname = msg.botname;
        const dataArr = msg.data;
        if (!botname || !dataArr || !Array.isArray(dataArr) || dataArr.length === 0) {
          ws.send(JSON.stringify({ code: 400, message: 'botname and data array required' }));
          return;
        }

        const bot = bots[botname];
        if (!bot) {
          ws.send(JSON.stringify({ code: 404, message: '机器人没有运行，请先启动' }));
          return;
        }

        try {
          let removedTotal = 0;
          let sent = 0;
          for (const item of dataArr) {
            let payload = '';
            if (item.chat) {
              const r = sanitizeOutgoingChat(item.chat);
              removedTotal += r.removed;
              payload = r.text;
            } else if (item.command) {
              const r = sanitizeOutgoingChat(item.command);
              removedTotal += r.removed;
              payload = r.text ? '/' + r.text : '';
            }
            if (!payload) continue;   // 净化后为空则不发，避免发出空包
            bot.chat(payload);
            sent++;
          }

          if (sent === 0) {
            ws.send(JSON.stringify({
              code: 400,
              message: removedTotal > 0
                ? '消息只包含服务器禁止的字符（§ 等），已拦截未发送'
                : '消息为空，未发送',
            }));
            return;
          }

          ws.send(JSON.stringify({
            code: 200,
            message: removedTotal > 0
              ? `成功执行（已自动过滤 ${removedTotal} 个服务器禁止字符，如 § ，否则机器人会被踢下线）`
              : '成功执行',
            filtered: removedTotal,
          }));
        } catch (e) {
          ws.send(JSON.stringify({ code: 401, message: `执行命令发生错误：${e && e.message || '未知错误'}` }));
        }
      });
    }

    // ─── /ws/api/stopbot ───
    else if (path === '/ws/api/stopbot') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to 8wbot stopbot API' }));
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) {
          ws.send(JSON.stringify({ code: 400, message: 'invalid JSON' })); return;
        }
        const username = msg.botname || msg.username;
        if (!username) { ws.send(JSON.stringify({ code: 400, message: 'botname or username required' })); return; }
        const bot = bots[username];
        if (!bot) {
          ws.send(JSON.stringify({ code: 404, message: '机器人没有运行' }));
          return;
        }
        stopBot(username);
        ws.send(JSON.stringify({ code: 200, message: '机器人已断开' }));
      });
    }

    // ─── /ws/api/botstatus ───
    else if (path === '/ws/api/botstatus') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to botstatus API' }));
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) { ws.send(JSON.stringify({ code: 400 })); return; }
        const username = msg.username;
        if (!username) { ws.send(JSON.stringify({ code: 400, message: 'username required' })); return; }
        const bot = bots[username];
        const exit = getExitReason(username);
        const resp = {
          code: 200,
          online: !!bot,
          // 上次退出原因（供后台落库与前端展示）
          last_exit_reason: exit ? exit.reason : '',
          last_exit_type: exit ? exit.type : '',
          last_exit_time: exit ? exit.time : '',
          // 实时状态缓存：位置/维度/所在服务器/邦国信息
          status: bot && bot._status ? bot._status : null
        };
        ws.send(JSON.stringify(resp));
      });
    }

    // ─── /ws/api/botlogs ───
    // 只做订阅。以前这里每来一条连接就 bot.on(...) 挂 5 个监听器且从不
    // 摘除，开 N 次控制台就泄漏 5N 个。事件现在由 startBot 统一注册并投到
    // 总线，这里订阅即可，连接关闭时按连接整体摘除。
    else if (path === '/ws/api/botlogs') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to botlogs API' }));
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) { ws.send(JSON.stringify({ code: 400 })); return; }
        const username = msg.username || msg.botname;
        if (!username) { ws.send(JSON.stringify({ code: 400, message: 'username required' })); return; }
        const bot = bots[username];
        if (!bot) {
          ws.send(JSON.stringify({ code: 404, message: 'bot not running' }));
          return;
        }
        subscribeBot(username, ws);
        ws.on('close', () => unsubscribeWs(ws));
        ws.send(JSON.stringify({ code: 200, message: 'monitoring started' }));
        // 立刻回一份当前状态，前端不用等下一个采集周期
        if (bot._status) {
          try { ws.send(JSON.stringify([{ botname: username, data: [{ status: bot._status }] }])); } catch (_) {}
        }
      });
    }

    // ─── /ws/api/events ───
    // 全局订阅：一条连接收所有机器人的事件（含 status）。
    // Go 服务端用它维持一条常驻订阅，再按归属扇出给各浏览器，
    // 这样「巡护拉起的机器人」和「没开控制台的用户」都能收到数据。
    else if (path === '/ws/api/events') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to events API' }));
      const watch = [];
      const attach = (username) => {
        if (watch.includes(username)) return;
        subscribeBot(username, ws);
        watch.push(username);
      };
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) { ws.send(JSON.stringify({ code: 400 })); return; }
        // { usernames:[...] } 订阅指定机器人；{ all:true } 订阅当前所有 + 后续新增
        if (Array.isArray(msg.usernames)) {
          msg.usernames.forEach(attach);
        }
        if (msg.all) {
          ws._watchAll = true;
          Object.keys(bots).forEach(attach);
        }
        ws.send(JSON.stringify({ code: 200, message: 'subscribed', watching: watch.slice() }));
      });
      // 订阅全部时，新启动的机器人也要自动纳入
      ws._onBotStarted = (username) => { if (ws._watchAll) attach(username); };
      eventWatchers.add(ws);
      ws.on('close', () => { unsubscribeWs(ws); eventWatchers.delete(ws); });
    }

    // ─── /ws/api/expand ───
    // 网页操控扩地。收 {username, chunks:[[cx,cz],...], occupied:[[cx,cz],...]} 开始，
    // 或 {username, action:'stop'} 停止。进度通过订阅推给本连接。
    // occupied 是服务器已知的已占区块，用于「接壤优先」排序。
    else if (path === '/ws/api/expand') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to 8wbot expand API' }));
      // close 只挂一次，否则每条消息都挂一个新监听器
      if (!ws._expandCloseHooked) {
        ws._expandCloseHooked = true;
        ws.on('close', () => unsubscribeWs(ws));
      }
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) {
          ws.send(JSON.stringify({ code: 400, message: 'invalid JSON' })); return;
        }
        const username = msg.username || msg.botname;
        if (!username) { ws.send(JSON.stringify({ code: 400, message: 'username required' })); return; }

        const bot = bots[username];
        if (!bot) { ws.send(JSON.stringify({ code: 404, message: '机器人未运行，请先启动机器人' })); return; }

        const q = claimQueues.get(username);
        if (!q) { ws.send(JSON.stringify({ code: 500, message: '扩地队列未就绪' })); return; }

        // 订阅这个机器人的数据流，进度才能推回去
        subscribeBot(username, ws);

        // ── 停止 ──
        if (msg.action === 'stop' || msg.type === 'stop') {
          const was = q.stop();
          ws.send(JSON.stringify({
            code: 200,
            message: was ? '已停止扩地' : '当前没有正在执行的扩地任务',
            stopped: was,
          }));
          broadcastExpand(username, Object.assign({ type: 'progress' }, q.snapshot('空闲')));
          return;
        }

        // ── 开始 ──
        const cells = Array.isArray(msg.chunks) ? msg.chunks : [];
        if (cells.length === 0) {
          ws.send(JSON.stringify({ code: 400, message: 'chunks 为空，没有可扩的区块' })); return;
        }
        const pc = playerChunkOf(bot);
        if (!pc) {
          ws.send(JSON.stringify({ code: 409, message: '机器人尚未进入世界（拿不到位置），请稍后重试' })); return;
        }

        const occupied = Array.isArray(msg.occupied) ? msg.occupied : [];
        let ordered;
        try {
          ordered = orderSquare(cells, occupied, pc);
        } catch (e) {
          console.error(`[${username}] orderSquare 异常: ${e.message}`);
          ordered = cells;
        }

        const ok = q.submit(ordered, occupied);
        console.log(`[${username}] 扩地下发: ${cells.length} 块 → 排序后 ${ordered.length} 块，已占 ${occupied.length} 块`);
        ws.send(JSON.stringify({
          code: ok ? 200 : 400,
          message: ok ? `已下发 ${ordered.length} 个区块，开始扩地` : '队列为空，未启动',
          count: ordered.length,
        }));
      });
    }

    else {
      ws.close(1002, 'unknown path');
    }
  });

  wss.on('listening', () => console.log('WebSocket API running on ws://0.0.0.0:' + WS_PORT + ' (startbot, sendinfo, botstatus, botlogs)'));
  wss.on('error', (e) => console.error('WebSocket server error:', e && e.message));
} catch (e) {
  console.error('Failed to start WebSocket server (port busy?):', e && e.message);
}

console.log('Launcher ready — /ws/api/startbot, /ws/api/sendinfo, /ws/api/expand');

// Start SSH control server from ssh.js (if present)
try {
  const startSshServer = require('./ssh');
  startSshServer({ port: process.env.SSH_PORT || 2222 }, {
    start: (cfg) => startBot((cfg && cfg.username) || 'wans7891'),
    stop: stopBot,
    status: () => Object.keys(bots)
  });
} catch (e) {
  console.error('Failed to start SSH module:', e && e.message);
}

// enter title screen (main waiting state)
showTitleScreen();