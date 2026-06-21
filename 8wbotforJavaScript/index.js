// launcher.js — 启动器，支持通过 WebSocket 控制启动/停止 Node bot
const mineflayer = require('mineflayer');
const WebSocket = require('ws');

const BOT_CONFIG_DEFAULT = {
  host: 'bgjq.simpfun.cn',
  port: 25565,
  username: 'wans7891',
  version: '1.21.4'
};

let bot = null;
const bots = {}; // map username -> bot (for multiple bots added via WS)
const core = require('./core');
let titleSelectedIndex = 0;
const titleControls = ['Start', 'Options', 'Quit'];

function startBot(config = {}) {
  if (bot) { console.log('Bot already running'); return; }
  const cfg = Object.assign({}, BOT_CONFIG_DEFAULT, config);
  console.log('Starting bot with', cfg.username, '@', cfg.host + ':' + cfg.port);
  bot = mineflayer.createBot({ host: cfg.host, port: cfg.port, username: cfg.username, version: cfg.version });
  bot.once('spawn', () => console.log('Bot spawned'));
  bot.on('end', () => { console.log('Bot ended'); bot = null; });
  bot.on('error', (e) => console.error('Bot error:', e && e.message));
}

function stopBot() {
  if (!bot) { console.log('Bot not running'); return; }
  try { bot.quit(); } catch (e) { /* ignore */ }
  bot = null;
  console.log('Bot stopped');
}

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
        if (sel === 'Start') startBot();
        else if (sel === 'Quit') process.exit();
      }
    });
  } catch (e) {
    console.error('Title screen error:', e && e.message);
  }
}

// WebSocket control server
const WS_PORT = parseInt(process.env.WS_PORT) || 8888;
try {
  const wss = new WebSocket.Server({ port: WS_PORT, path: '/ws' });
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      try {
        let msg = data;
        try { msg = JSON.parse(data); } catch (_) { /* allow plain strings */ }
        if (typeof msg === 'string') msg = { cmd: msg };

        // new WS API: addbot -> creates a bot connected to fixed server
        if (msg.cmd === 'addbot') {
          const username = msg.username;
          if (!username) { ws.send(JSON.stringify({ ok: false, error: 'username required' })); return; }
          if (bots[username]) { ws.send(JSON.stringify({ ok: false, error: 'bot exists' })); return; }
          try {
            const b = core.createBot(username);
            bots[username] = b;
            ws.send(JSON.stringify({ ok: true, msg: 'bot added', username }));
          } catch (e) { ws.send(JSON.stringify({ ok: false, error: e && e.message })); }
          return;
        }

        if (msg.cmd === 'start') { startBot(msg.config || {}); ws.send(JSON.stringify({ ok: true, msg: 'started' })); }
        else if (msg.cmd === 'stop') { stopBot(); ws.send(JSON.stringify({ ok: true, msg: 'stopped' })); }
        else if (msg.cmd === 'status') { ws.send(JSON.stringify({ ok: true, running: !!bot, bots: Object.keys(bots) })); }
        else { ws.send(JSON.stringify({ ok: false, error: 'unknown command' })); }
      } catch (e) {
        ws.send(JSON.stringify({ ok: false, error: e && e.message }));
      }
    });
    ws.send(JSON.stringify({ ok: true, msg: 'connected' }));
  });
  wss.on('listening', () => console.log('WebSocket control running on ws://0.0.0.0:' + WS_PORT + '/ws'));
  wss.on('error', (e) => console.error('WebSocket server error:', e && e.message));
} catch (e) {
  console.error('Failed to start WebSocket server (port busy?):', e && e.message);
}

console.log('Launcher ready — use WebSocket messages {cmd:"start"|"stop"|"status"}');

// Start SSH control server from ssh.js (if present)
try {
  const startSshServer = require('./ssh');
  startSshServer({ port: process.env.SSH_PORT || 2222 }, {
    start: startBot,
    stop: stopBot,
    status: () => !!bot
  });
} catch (e) {
  console.error('Failed to start SSH module:', e && e.message);
}

// enter title screen (main waiting state)
showTitleScreen();