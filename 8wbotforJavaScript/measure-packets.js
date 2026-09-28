// measure-packets.js — 量出机器人到底往服务器发了多少移动包
//
// 为什么需要这个：GrimAC 的 Timer / TickTimer 检查抓的是「发包节奏」。
// 服务器 tick 是 20/秒，正常客户端每秒最多 20 个移动包。超出的部分
// 在 GrimAC 眼里就是非法的。
//
// 这个脚本在【客户端侧】数实际发出的包，跟服务器日志里的 packets 数字
// 对照，就能确认多出来的包是哪个循环发的。
//
// 用法：
//   node measure-packets.js <机器人用户名> [秒数]
// 例：
//   node measure-packets.js wans7891 30
//
// 判读：
//   静止时约 1-20 包/秒   → 正常
//   静止时 40-70 包/秒    → 双循环发包（原版 + reworked 各发一份）
//   一直 0                → 客户端没发包，问题在服务端侧

'use strict';

const mineflayer = require('mineflayer');
const fs = require('fs');
const path = require('path');

function loadEnv() {
  const f = path.join(__dirname, '.env');
  if (!fs.existsSync(f)) return {};
  const out = {};
  for (const line of fs.readFileSync(f, 'utf8').split('\n')) {
    const s = line.trim();
    if (!s || s.startsWith('#')) continue;
    const i = s.indexOf('=');
    if (i < 0) continue;
    out[s.slice(0, i).trim()] = s.slice(i + 1).trim().replace(/^["']|["']$/g, '');
  }
  return out;
}

const env = loadEnv();
const username = process.argv[2];
const seconds = process.argv[3] ? Number(process.argv[3]) : 30;

if (!username) {
  console.error('用法: node measure-packets.js <机器人用户名> [秒数]');
  process.exit(1);
}

const HOST = process.env.MC_HOST || 'bgjq.simpfun.cn';
const PORT = parseInt(process.env.MC_PORT) || 25565;
const VERSION = process.env.MC_VERSION || '1.21.4';

console.log(`\n════ 发包率测量 ════`);
console.log(`  机器人: ${username}`);
console.log(`  服务器: ${HOST}:${PORT} (${VERSION})`);
console.log(`  时长  : ${seconds}s\n`);

// ── 关键：分类型计数 ──
const counts = {
  position: 0,
  position_look: 0,
  look: 0,
  flying: 0,
};
let total = 0;
const timeline = [];   // [秒, 累计总数]
const t0 = Date.now();
const now = () => ((Date.now() - t0) / 1000).toFixed(1).padStart(5);

const bot = mineflayer.createBot({
  host: HOST,
  port: PORT,
  username,
  version: VERSION,
  // 和 index.js 保持一致，这样测出来的就是生产环境的真实行为
  maxCatchupTicks: 1,
});

// ── 在协议层拦下所有出站移动包 ──
//
// 双保险：mineflayer 内部一律走 bot._client.write('包名', ...)，
// 所以包一层 write 就够了；但为了防止某个插件缓存了函数引用导致钩子失效，
// 再挂一个 'write' 事件做交叉验证，两边对不上就说明有漏网。
const origWrite = bot._client.write.bind(bot._client);
bot._client.write = function (name, params) {
  if (Object.prototype.hasOwnProperty.call(counts, name)) {
    counts[name]++;
    total++;
  }
  return origWrite(name, params);
};

let eventTotal = 0;
bot._client.on('write', (name, params) => {
  void params;
  if (Object.prototype.hasOwnProperty.call(counts, name)) eventTotal++;
});

bot.once('spawn', () => {
  console.log(`  [${now()}] ✅ 已进入游戏，开始计数（此时机器人静止不动）\n`);
});

// 每秒打印一次
const ticker = setInterval(() => {
  const sec = Math.round((Date.now() - t0) / 1000);
  timeline.push([sec, total]);
  const recent = timeline.length >= 2
    ? total - timeline[timeline.length - 2][1]
    : total;
  console.log(
    `  [${now()}] 累计=${String(total).padStart(5)}  本秒=${String(recent).padStart(3)}` +
    `  pos=${counts.position} poslook=${counts.position_look} look=${counts.look} flying=${counts.flying}`
  );
}, 1000);

bot.on('kicked', (r) => console.log(`  [${now()}] 👢 被踢: ${typeof r === 'string' ? r : JSON.stringify(r)}`));
bot.on('error', (e) => console.log(`  [${now()}] ❌ ${e.message}`));
bot.on('end', (r) => console.log(`  [${now()}] 🔌 断开: ${r}`));

setTimeout(() => {
  clearInterval(ticker);
  const elapsed = (Date.now() - t0) / 1000;

  console.log(`\n════ 结果 ════`);
  console.log(`  总移动包  : ${total}`);
  console.log(`  实际时长  : ${elapsed.toFixed(1)}s`);
  console.log(`  平均发包率: ${(total / elapsed).toFixed(1)} 包/秒`);
  console.log(`    其中 position      : ${counts.position}`);
  console.log(`    其中 position_look : ${counts.position_look}`);
  console.log(`    其中 look          : ${counts.look}`);
  console.log(`    其中 flying        : ${counts.flying}`);

  if (eventTotal > 0 && eventTotal !== total) {
    console.log(`\n  ⚠️ 交叉验证不一致：write 事件数=${eventTotal}，包装计数=${total}`);
    console.log(`     说明有插件绕过了包装直接发包（可能有漏计数）`);
  } else if (eventTotal > 0) {
    console.log(`\n  ✅ 交叉验证一致（write 事件数=${eventTotal}）`);
  }

  const rate = total / elapsed;
  console.log('');
  if (rate < 25) {
    console.log('  → 发包率正常（服务器 tick 20/秒）。');
    console.log('    如果 GrimAC 还在报 flying，那问题不在发包量，在物理位置本身。');
  } else if (rate < 40) {
    console.log('  → 略高于正常值，可能有轻微补帧。');
  } else {
    console.log('  → 发包率严重超标，接近正常值的两倍以上。');
    console.log('    这符合「两个物理循环各发一份」的特征：');
    console.log('    mineflayer 原版的 updatePosition 不受 physicsEnabled 控制，');
    console.log('    physics-reworked 又起了自己的 setInterval，两边都在发。');
  }

  try { bot.quit(); } catch (_) {}
  setTimeout(() => process.exit(0), 500);
}, seconds * 1000);
