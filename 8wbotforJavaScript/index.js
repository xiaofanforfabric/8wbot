// launcher.js — 启动器，通过 WS /ws/api/startbot 启动机器人并实时推送日志
const mineflayer = require('mineflayer');
const WebSocket = require('ws');

const SERVER_HOST = 'bgjq.simpfun.cn';
const SERVER_PORT = 25565;
const SERVER_VERSION = '1.21.4';

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
    const path = req.url || '/';
    console.log('WS client connected:', path);

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
          for (const item of dataArr) {
            if (item.chat) {
              bot.chat(item.chat);
            } else if (item.command) {
              bot.chat('/' + item.command);
            }
          }
          ws.send(JSON.stringify({ code: 200, message: '成功执行' }));
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
        if (!bot) {
          ws.send(JSON.stringify({ code: 200, online: false }));
        } else {
          ws.send(JSON.stringify({ code: 200, online: true }));
        }
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