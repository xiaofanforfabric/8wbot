// diagnose-move.js — 移动 / 反作弊（GrimAC）诊断
//
// 现象：「移动后被拉回原位」。这通常意味着服务端反作弊判定移动包非法，
// 把玩家位置回滚到它认可的位置。
//
// 这个脚本的目的不是"修"，是"取证"：把机器人自己的坐标和
// 服务端下发的坐标摆在一起，看它们是不是在打架。
//
// 用法：
//   node diagnose-move.js <机器人用户名> [目标X] [目标Z] [秒数]
// 例：
//   node diagnose-move.js wans7891 -7288 -11528 40
//
// 判读：
//   - serverPos 频繁出现而 selfPos 不动 → 服务端在拉回，反作弊拦截
//   - selfPos 在动但 serverPos 不跟着变   → 移动包没被受理
//   - 两者一致但都不动                     → 不是反作弊，是寻路没启动
//   - 频繁 position 校正 + 小幅度来回       → 典型的移动校验失败

'use strict';

const mineflayer = require('mineflayer');
const { pathfinder, goals } = require('mineflayer-pathfinder');
const fs = require('fs');
const path = require('path');

// ── .env（与 index.js 同源，优先读同目录的）──
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
const targetX = process.argv[3] ? Number(process.argv[3]) : null;
const targetZ = process.argv[4] ? Number(process.argv[4]) : null;
const seconds = process.argv[5] ? Number(process.argv[5]) : 30;

if (!username) {
  console.error('用法: node diagnose-move.js <机器人用户名> [目标X] [目标Z] [秒数]');
  process.exit(1);
}

const HOST = env.MC_HOST || 'bgjq.simpfun.cn';
const PORT = Number(env.MC_PORT || 25565);
const VERSION = env.MC_VERSION || '1.21.4';

console.log(`\n════ 移动诊断 ════`);
console.log(`  机器人  : ${username}`);
console.log(`  服务器  : ${HOST}:${PORT} (${VERSION})`);
console.log(`  目标    : ${targetX === null ? '（不寻路，只观察）' : `${targetX}, ${targetZ}`}`);
console.log(`  时长    : ${seconds}s\n`);

const bot = mineflayer.createBot({
  host: HOST,
  port: PORT,
  username,
  version: VERSION,
  auth: 'offline',
});

// ── 坐标轨迹 ──
const trail = [];
let lastSelf = null;
let pulls = 0;          // 被拉回的次数
let corrections = 0;    // 服务端位置校正次数
let movePackets = 0;    // 我们发出的移动包估算

function now() {
  return ((Date.now() - t0) / 1000).toFixed(1).padStart(5);
}
const t0 = Date.now();

bot.once('spawn', () => {
  console.log(`  [${now()}] ✅ 已进入游戏`);
  try {
    bot.loadPlugin(pathfinder);
    console.log(`  [${now()}] ✅ pathfinder 已加载`);
  } catch (e) {
    console.log(`  [${now()}] ⚠️ pathfinder 加载失败: ${e.message}`);
  }
  lastSelf = { ...bot.entity.position };

  if (targetX !== null && targetZ !== null) {
    setTimeout(() => {
      console.log(`  [${now()}] → setGoal GoalXZ(${targetX}, ${targetZ})`);
      try {
        bot.pathfinder.setGoal(new goals.GoalXZ(targetX, targetZ));
      } catch (e) {
        console.log(`  [${now()}] ❌ setGoal 失败: ${e.message}`);
      }
    }, 1500);
  }
});

// ── 关键：服务端下发的权威位置 ──
//
// mineflayer 处理 position 包时会直接把 entity.position 设成服务端的值。
// 我们在这里记一笔，看它出现的频率和幅度。
bot._client.on('position', (packet) => {
  corrections++;
  const p = bot.entity ? bot.entity.position : null;
  console.log(
    `  [${now()}] 🔄 服务端位置校正 #${corrections} → ` +
    `x=${packet.x.toFixed(2)} y=${packet.y.toFixed(2)} z=${packet.z.toFixed(2)}` +
    (p ? `  (校正前本地 x=${p.x.toFixed(2)} z=${p.z.toFixed(2)})` : '')
  );
});

// ── 每 500ms 采样一次坐标 ──
const sampler = setInterval(() => {
  if (!bot.entity) return;
  const p = bot.entity.position;
  const cur = { x: p.x, y: p.y, z: p.z };

  if (lastSelf) {
    const d = Math.hypot(cur.x - lastSelf.x, cur.z - lastSelf.z);
    // 相邻采样间的位移。反作弊拉回的典型特征是：走了一小段又弹回去
    if (d > 8) {
      pulls++;
      console.log(
        `  [${now()}] ⚠️ 瞬移 ${d.toFixed(1)} 格  ` +
        `(${lastSelf.x.toFixed(1)},${lastSelf.z.toFixed(1)}) → (${cur.x.toFixed(1)},${cur.z.toFixed(1)})`
      );
    }
  }

  trail.push(cur);
  lastSelf = cur;

  let moving = false;
  try { moving = bot.pathfinder && bot.pathfinder.isMoving(); } catch (_) {}
  console.log(
    `  [${now()}] pos x=${cur.x.toFixed(2)} y=${cur.y.toFixed(2)} z=${cur.z.toFixed(2)}` +
    `  寻路中=${moving ? '是' : '否'}  校正=${corrections}  瞬移=${pulls}`
  );
}, 500);

bot.on('death', () => console.log(`  [${now()}] ☠️ 机器人死亡`));
bot.on('kicked', (r) => console.log(`  [${now()}] 👢 被踢: ${typeof r === 'string' ? r : JSON.stringify(r)}`));
bot.on('error', (e) => console.log(`  [${now()}] ❌ 错误: ${e.message}`));
bot.on('end', (r) => console.log(`  [${now()}] 🔌 断开: ${r}`));

// ── 收尾报告 ──
setTimeout(() => {
  clearInterval(sampler);

  console.log(`\n════ 结论 ════`);
  console.log(`  采样点数    : ${trail.length}`);
  console.log(`  位置校正次数: ${corrections}   ← 服务端主动纠正我们`);
  console.log(`  瞬移次数    : ${pulls}   ← 单次位移 > 8 格`);

  if (trail.length >= 2) {
    const a = trail[0];
    const b = trail[trail.length - 1];
    const total = Math.hypot(b.x - a.x, b.z - a.z);
    console.log(`  净位移      : ${total.toFixed(2)} 格`);
  }

  console.log('');
  if (corrections === 0 && pulls === 0) {
    console.log('  → 没有观察到服务端校正，也不存在瞬移。');
    console.log('    如果机器人确实没动，问题在寻路没启动，不是反作弊。');
  } else if (corrections > 4) {
    console.log('  → 服务端在频繁校正位置，这是反作弊拦截移动的典型特征。');
    console.log('    可能原因：移动包节奏异常、穿墙/飞行判定、区块未加载时的位移。');
  } else {
    console.log('  → 有零星校正，属于网络抖动的正常范围。');
  }

  try { bot.quit(); } catch (_) {}
  setTimeout(() => process.exit(0), 500);
}, seconds * 1000);
