const mineflayer = require('mineflayer');

const DEFAULT_HOST = 'bgjq.simpfun.cn';
const DEFAULT_PORT = 25565;
const DEFAULT_VERSION = '1.21.4';

function createBot(username) {
  if (!username) throw new Error('username required');
  const bot = mineflayer.createBot({ host: DEFAULT_HOST, port: DEFAULT_PORT, username, version: DEFAULT_VERSION });
  console.log(`core: creating bot ${username} -> ${DEFAULT_HOST}:${DEFAULT_PORT} (v${DEFAULT_VERSION})`);
  bot.once('spawn', () => console.log(`core: bot ${username} spawned`));
  bot.on('end', () => console.log(`core: bot ${username} ended`));
  bot.on('error', (err) => console.error(`core: bot ${username} error:`, err && err.message));
  return bot;
}

module.exports = { createBot };
