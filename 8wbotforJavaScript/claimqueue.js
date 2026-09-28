// claimqueue.js — 网页操控的疆域开拓队列
//
// 移植自 xiaofanbot（Java / Fabric 模组）的 ClaimQueue.java + ChatCommandHandler 的开拓通知解析。
// 参考项目只读，这里是把逻辑重写成 Node / mineflayer 版本。
//
// 与「全自动扩地」的区别：这里的区块列表由网页下发，机器人只负责按顺序走过去。
// 区块的开拓由服务器自动完成（走过去即开拓），机器人靠聊天通知确认结果。
//
// 工作流：
//   1. 网页下发 { chunks: [[cx,cz],...], occupied: [[cx,cz],...] }
//   2. orderSquare 按「接壤优先 + 就近」排序，避免机器人来回横跳
//   3. 逐块：寻路到区块中心 → 等到达 → 等服务器聊天确认「已为你的邦国开拓」
//   4. 全程回报进度，可随时中断

'use strict';

/**
 * 服务器开拓成功通知。
 * 例：疆土 (-456, -721) [-7288, -11528] 已为你的邦国开拓！
 *      └─区块坐标─┘  └──区块中心的方块坐标──┘
 * 四个捕获组：区块 x、区块 z、方块 x、方块 z。
 */
const CLAIM_NOTIFY_RE =
  /疆土\s*\(\s*(-?\d+)\s*,\s*(-?\d+)\s*\)\s*\[\s*(-?\d+)\s*,\s*(-?\d+)\s*\]\s*已为你的邦国开拓/;

/** 单块超时（秒）。到达目标与等待服务器通知都用它，与参考实现一致。 */
const CHUNK_TIMEOUT_SEC = 300;

/** 寻路中断后，至少等了这么多秒才重设目标，避免刚起步就误判 */
// 已废弃：这是给 mineflayer-pathfinder 那个「每秒检查 isMoving、不动就
// 重设目标」的循环用的。换成 baritone 后 gotoSmart 自己会走到底，
// 不需要外部重设计划（反复重设计划反而让移动包节奏抖动，更容易被反作弊抓）。
// 保留导出只是为了不破坏可能引用它的地方。
const REPATH_AFTER_SEC = 3;

// ════════════════════════════════════════════════════════════════
// 排序
// ════════════════════════════════════════════════════════════════

// 区块坐标打包成一个数值当 Set 的键。参考实现用 long 位运算，
// 这里用字符串更直观，且区块数量级（几百到几千）下性能无差别。
const k = (cx, cz) => cx + ',' + cz;

function neighborsIn(set, cx, cz) {
  let n = 0;
  if (set.has(k(cx + 1, cz))) n++;
  if (set.has(k(cx - 1, cz))) n++;
  if (set.has(k(cx, cz + 1))) n++;
  if (set.has(k(cx, cz - 1))) n++;
  return n;
}

/**
 * 扩地顺序：接壤优先 + 越方越好。
 *
 * 贪心算法：
 *   1. 以「已占区块集合」为起点（网页传来的 occupied，外加玩家当前区块作种子，
 *      保证第一块一定贴着自己扩）
 *   2. 每步从待扩集合里选：与已占集合接壤数最多的（4 > 3 > 2 > 1）
 *      并列时选离上一格最近的，避免跳来跳去
 *   3. 选中后并入已占集合，继续下一步 —— 自然形成贴边向外的方正形状
 *
 * @param {Array<[number,number]>} cells     待扩区块
 * @param {Array<[number,number]>} occupied  服务器已知已占区块
 * @param {[number,number]|null}   playerChunk 玩家当前区块，作种子与起点参考
 * @returns {Array<[number,number]>} 排序后的执行顺序
 */
function orderSquare(cells, occupied, playerChunk) {
  if (!Array.isArray(cells) || cells.length === 0) return [];

  try {
    const occ = new Set();
    if (Array.isArray(occupied)) {
      for (const c of occupied) {
        if (Array.isArray(c) && c.length >= 2) occ.add(k(c[0], c[1]));
      }
    }
    if (playerChunk) occ.add(k(playerChunk[0], playerChunk[1]));

    // 待扩集合。用 Map 保序，兜底分支按原顺序追加时行为可预期。
    const remain = new Map();
    for (const c of cells) remain.set(k(c[0], c[1]), [c[0], c[1]]);

    // 起点：离玩家最近的格子，避免一上来横穿整个选区
    const pcx = playerChunk ? playerChunk[0] : 0;
    const pcz = playerChunk ? playerChunk[1] : 0;
    let cur = null;
    let bestD = Infinity;
    for (const c of remain.values()) {
      const d = Math.abs(c[0] - pcx) + Math.abs(c[1] - pcz);
      if (d < bestD) {
        bestD = d;
        cur = c;
      }
    }
    if (!cur) return [];

    const ordered = [];
    while (remain.size > 0) {
      let found = false;
      let bestKey = null;
      let bestNeighbor = -1;
      let bestDist = Infinity;
      let best = null;

      for (const [key, c] of remain) {
        const nb = neighborsIn(occ, c[0], c[1]);
        const dist = cur ? Math.abs(c[0] - cur[0]) + Math.abs(c[1] - cur[1]) : 0;
        if (!found || nb > bestNeighbor || (nb === bestNeighbor && dist < bestDist)) {
          found = true;
          bestKey = key;
          bestNeighbor = nb;
          bestDist = dist;
          best = c;
        }
      }

      // 兜底：选不出来就把剩下的按原顺序追加，绝不丢块
      if (!found || !best) {
        for (const c of remain.values()) ordered.push([c[0], c[1]]);
        break;
      }

      remain.delete(bestKey);
      occ.add(bestKey);
      ordered.push([best[0], best[1]]);
      cur = best;
    }

    return ordered;
  } catch (err) {
    // 排序只是为了少跑路，出错不该让整个扩地失败 —— 退化为原始顺序
    console.error('[ClaimQueue] orderSquare 异常，退化为原始顺序:', err.message);
    return cells.map((c) => [c[0], c[1]]);
  }
}

// ════════════════════════════════════════════════════════════════
// 队列
// ════════════════════════════════════════════════════════════════

/**
 * 一个机器人对应一个队列。
 *
 * @param {object} bot                  mineflayer 机器人实例
 * @param {object} opts
 * @param {(cx:number,cz:number)=>Promise<boolean>} opts.walkToChunk  走到区块中心，返回是否到达
 * @param {(ev:object)=>void}        opts.onEvent     进度与结果回调
 * @param {()=>[number,number]|null} opts.playerChunk  取玩家当前区块
 * @param {()=>boolean}              opts.isUsable     机器人是否还能干活（在线/未销毁）
 */
class ClaimQueue {
  constructor(bot, opts) {
    this.bot = bot;
    this.walkToChunk = opts.walkToChunk;
    this.onEvent = opts.onEvent || (() => {});
    this.playerChunk = opts.playerChunk || (() => null);
    this.isUsable = opts.isUsable || (() => true);

    /** @type {Array<[number,number]>} 已排序的执行队列 */
    this.queue = [];
    this.cursor = 0;
    this.running = false;
    this.current = null;

    /** 服务器已确认开拓的区块。聊天监听调用 markClaimed 往里塞。 */
    this.claimed = new Set();
    /** 已占集合，随开拓进度实时扩充，供后续接壤判断 */
    this.occupied = new Set();
  }

  get total() {
    return this.queue.length;
  }
  get done() {
    return this.cursor;
  }
  get remaining() {
    return Math.max(0, this.queue.length - this.cursor);
  }

  /** 剩余待扩区块，网页刷新后用它还原进度 */
  remainingCells() {
    return this.queue.slice(this.cursor).map((c) => [c[0], c[1]]);
  }

  /** 当前状态快照 */
  snapshot(task) {
    return {
      running: this.running,
      task: task || (this.running ? '扩地中' : '空闲'),
      total: this.total,
      done: this.done,
      remaining: this.remaining,
      current: this.current ? [this.current[0], this.current[1]] : null,
      remainingChunks: this.remainingCells(),
    };
  }

  /**
   * 聊天监听调用：某区块已被服务器确认开拓。
   * 立刻并入已占集合，后续接壤判断才能把新开拓的算进去。
   */
  markClaimed(cx, cz) {
    this.claimed.add(k(cx, cz));
    this.occupied.add(k(cx, cz));
  }

  /** 中断当前任务 */
  stop() {
    if (!this.running) return false;
    this.running = false;
    return true;
  }

  /**
   * 提交一批区块并开始执行。
   * @param {Array<[number,number]>} ordered 已排序的区块列表
   * @param {Array<[number,number]>} occupied 服务器已知已占区块
   */
  submit(ordered, occupied) {
    this.stop();

    this.queue = Array.isArray(ordered) ? ordered.map((c) => [c[0], c[1]]) : [];
    this.cursor = 0;
    this.current = null;

    this.occupied = new Set();
    if (Array.isArray(occupied)) {
      for (const c of occupied) {
        if (Array.isArray(c) && c.length >= 2) this.occupied.add(k(c[0], c[1]));
      }
    }

    if (this.queue.length === 0) {
      console.error('[ClaimQueue] 提交了空队列，不启动');
      this.onEvent({ type: 'log', msg: '扩地队列为空：排序后无可用区块，请检查选区' });
      return false;
    }

    this.running = true;
    this.emitProgress();
    // 不 await —— 后台跑，调用方立刻返回
    this._run().catch((err) => {
      console.error('[ClaimQueue] 运行异常:', err);
      this.onEvent({ type: 'log', msg: '扩地异常: ' + err.message });
    });
    return true;
  }

  emitProgress(extra) {
    this.onEvent(Object.assign({ type: 'progress' }, this.snapshot(), extra || {}));
  }

  async _run() {
    try {
      while (this.running && this.cursor < this.queue.length) {
        const c = this.queue[this.cursor];
        this.current = c;
        this.emitProgress();

        const cx = c[0];
        const cz = c[1];

        if (!this.isUsable()) {
          this.onEvent({ type: 'log', msg: '机器人已离线，扩地中止' });
          break;
        }

        // 先清掉可能残留的旧通知，避免上一块的确认被这一块误收
        const key = k(cx, cz);
        this.claimed.delete(key);

        // ── 走过去 ──
        let arrived = false;
        try {
          arrived = await this.walkToChunk(cx, cz);
        } catch (err) {
          console.error(`[ClaimQueue] 前往 (${cx},${cz}) 异常:`, err.message);
        }

        if (!this.running) return;

        // ── 等服务器确认 ──
        let confirmed = false;
        if (arrived) {
          confirmed = await this._waitNotify(cx, cz);
        }

        this.cursor++;
        this.current = null;

        if (confirmed) {
          this.onEvent({ type: 'claimed', cx, cz });
        } else {
          this.onEvent({
            type: 'failed',
            cx,
            cz,
            reason: arrived ? '未收到服务器开拓通知' : '未到达目标',
          });
        }
        this.emitProgress();
      }
    } finally {
      const processed = this.cursor;
      this.running = false;
      this.current = null;
      // 收尾再推一次状态，让网页把任务显示为「空闲」
      this.onEvent(Object.assign({ type: 'progress' }, this.snapshot('空闲')));
      this.onEvent({ type: 'log', msg: `扩地结束，共处理 ${processed} 个区块` });
    }
  }

  /** 等服务器聊天通知；最多 CHUNK_TIMEOUT_SEC 秒 */
  _waitNotify(cx, cz) {
    const key = k(cx, cz);
    return new Promise((resolve) => {
      let waited = 0;
      const timer = setInterval(() => {
        if (!this.running) {
          clearInterval(timer);
          return resolve(false);
        }
        if (this.claimed.has(key)) {
          this.claimed.delete(key);
          clearInterval(timer);
          return resolve(true);
        }
        waited += 0.5;
        if (waited >= CHUNK_TIMEOUT_SEC) {
          clearInterval(timer);
          return resolve(false);
        }
      }, 500);
    });
  }
}

// ════════════════════════════════════════════════════════════════
// 聊天监听
// ════════════════════════════════════════════════════════════════

/**
 * 从一行聊天文本里解析开拓通知。
 * @returns {{cx:number,cz:number,bx:number,bz:number}|null}
 */
function parseClaimNotify(text) {
  if (!text || typeof text !== 'string') return null;
  if (!text.includes('已为你的邦国开拓')) return null;
  const m = CLAIM_NOTIFY_RE.exec(text);
  if (!m) return null;
  return {
    cx: parseInt(m[1], 10),
    cz: parseInt(m[2], 10),
    bx: parseInt(m[3], 10),
    bz: parseInt(m[4], 10),
  };
}

module.exports = {
  ClaimQueue,
  orderSquare,
  parseClaimNotify,
  CLAIM_NOTIFY_RE,
  CHUNK_TIMEOUT_SEC,
  REPATH_AFTER_SEC,
};
