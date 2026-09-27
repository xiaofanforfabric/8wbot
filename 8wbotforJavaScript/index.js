require('dotenv').config();

// launcher.js — 启动器，通过 WS /ws/api/startbot 启动机器人并实时推送日志
const mineflayer = require('mineflayer');
const WebSocket = require('ws');
const url = require('url');
const fs = require('fs');
const path = require('path');

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
  });

  // Listen for /server command responses to determine current server
  bot._currentServer = null;
  bot._serverCheckResolve = null;
  bot.on('message', (jsonMsg) => {
    const text = jsonMsg.toString();
    const m = text.match(/You are currently connected to (\w+)/);
    if (m) {
      bot._currentServer = m[1];
      if (bot._serverCheckResolve) {
        bot._serverCheckResolve(m[1]);
        bot._serverCheckResolve = null;
      }
    }
  });

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

        bot.once('spawn', () => {
          console.log(`[${username}] spawned`);
          ws.send(JSON.stringify([{ botname: username, data: [{ chat: `[系统] ${username} 已进入游戏` }] }]));
          // Select first hotbar slot (map is there)
          try {
            bot.setQuickBarSlot(0);
            console.log(`[${username}] selected hotbar slot 0`);
          } catch (e) {
            console.log(`[${username}] setQuickBarSlot error:`, e.message);
          }
        });
        bot.on('chat', (who, message) => {
          ws.send(JSON.stringify([{ botname: username, data: [{ chat: `[聊天] ${who}: ${message}` }] }]));
        });
        bot.on('message', (jsonMsg) => {
          ws.send(JSON.stringify([{ botname: username, data: [{ chat: `[消息] ${jsonMsg.toString()}` }] }]));
        });
        bot.on('map', (map) => {
          console.log(`[${username}] map event:`, map ? 'received' : 'null', map ? `data=${map.data ? map.data.length : 'no'}` : '');
          if (map && map.data && map.data.length > 0) {
            const b64 = map.data.toString('base64');
            const w = map.data.length > 20000 ? 256 : 128;
            sendMapIfNotMain(username, b64, w, (b, w2) => {
              ws.send(JSON.stringify([{ botname: username, data: [{ mapdata: b, width: w2 }] }]));
            });
          }
        });
        // Listen for raw map data packet
        bot._client.on('map', (data) => {
          if (data && data.data && data.columns > 0 && data.rows > 0) {
            const buf = Buffer.isBuffer(data.data) ? data.data : Buffer.from(data.data);
            console.log(`[${username}] map data: ${buf.length} bytes, ${data.columns}x${data.rows}`);
            if (buf.length > 0) {
              const b64 = buf.toString('base64');
              const w = data.columns;
              // Check server before sending — only send if NOT on main
              sendMapIfNotMain(username, b64, w, (b, w2) => {
                ws.send(JSON.stringify([{ botname: username, data: [{ mapdata: b, width: w2 }] }]));
              });
            }
          }
        });
        bot.on('error', (err) => {
          console.error(`[${username}] error:`, err && err.message);
          ws.send(JSON.stringify({ code: 401, message: `启动失败，连接已丢失：${err && err.message || '未知错误'}` }));
        });
        bot.on('end', (reason) => {
          console.log(`[${username}] ended:`, reason);
          delete bots[username];
          ws.send(JSON.stringify([{ botname: username, data: [{ chat: `[系统] ${username} 已断开连接: ${reason || '未知'}` }] }]));
          ws.send(JSON.stringify([{ botname: username, data: [{ bot_offline: true, reason: reason || '未知' }] }]));
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
          last_exit_time: exit ? exit.time : ''
        };
        ws.send(JSON.stringify(resp));
      });
    }

    // ─── /ws/api/botlogs ───
    else if (path === '/ws/api/botlogs') {
      ws.send(JSON.stringify({ ok: true, msg: 'connected to botlogs API' }));
      ws.on('message', (data) => {
        let msg;
        try { msg = JSON.parse(data); } catch (_) { ws.send(JSON.stringify({ code: 400 })); return; }
        const username = msg.username;
        if (!username) { ws.send(JSON.stringify({ code: 400, message: 'username required' })); return; }
        const bot = bots[username];
        if (!bot) {
          ws.send(JSON.stringify({ code: 404, message: 'bot not running' }));
          return;
        }
        ws.send(JSON.stringify({ code: 200, message: 'monitoring started' }));
        // Forward all bot events to this WS connection
        const fwd = (evt) => {
          try { ws.send(JSON.stringify(evt)); } catch (_) {}
        };
        bot.on('chat', (who, message) => {
          fwd([{ botname: username, data: [{ chat: `[聊天] ${who}: ${message}` }] }]);
        });
        bot.on('message', (jsonMsg) => {
          fwd([{ botname: username, data: [{ chat: `[消息] ${jsonMsg.toString()}` }] }]);
        });
        bot.on('map', (map) => {
          console.log(`[${username}] botlogs map event:`, map ? 'received' : 'null');
          if (map && map.data && map.data.length > 0) {
            const b64 = map.data.toString('base64');
            const w = map.data.length > 20000 ? 256 : 128;
            sendMapIfNotMain(username, b64, w, (b, w2) => {
              fwd([{ botname: username, data: [{ mapdata: b, width: w2 }] }]);
            });
          }
        });
        // Listen for raw map data packet (works for Minecraft 1.21.4)
        bot._client.on('map', (data) => {
          if (data && data.data && data.columns > 0 && data.rows > 0) {
            const buf = Buffer.isBuffer(data.data) ? data.data : Buffer.from(data.data);
            console.log(`[${username}] botlogs map data: ${buf.length} bytes, ${data.columns}x${data.rows}`);
            if (buf.length > 0) {
              const b64 = buf.toString('base64');
              const w = data.columns;
              // Check server before sending — only send if NOT on main
              sendMapIfNotMain(username, b64, w, (b, w2) => {
                fwd([{ botname: username, data: [{ mapdata: b, width: w2 }] }]);
              });
            }
          }
        });
        bot.on('end', (reason) => {
          fwd([{ botname: username, data: [{ chat: `[系统] ${username} 已断开连接: ${reason || '未知'}` }] }]);
          fwd([{ botname: username, data: [{ bot_offline: true, reason: reason || '未知' }] }]);
        });
        ws.on('close', () => {
          // Remove listeners by re-assigning noop — bot stays running
        });
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

console.log('Launcher ready — /ws/api/startbot and /ws/api/sendinfo');

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