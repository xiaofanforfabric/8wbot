// status.js — 机器人状态文本解析
//
// 抽成独立模块是为了能脱离 mineflayer 单测：这一段要处理「同一句话
// 中英文两种形态」，还要把插件吐出的多行邦国信息拼成一个块，是整条
// 状态链路上最容易出错的地方。
//
// 语言背景：mineflayer 默认 locale 是 en_US（lib/plugins/settings.js），
// 所以原版翻译消息（/server 的回复）机器人看到的是英文；而 /u info 是
// 服务器插件自己写的文本，机器人看到的中文和玩家客户端一致。
// 两条路径的语言不一样，所以这里两套模式都留着。

// 「--- 邦国信息: 雅典维亚城邦（二世） ---」
const KINGDOM_HEAD_RE = /^[-—\s]*邦国信息\s*[:：]\s*(.+?)\s*[-—\s]*$/;

// 「You are currently connected to main.」/「您已连接至 main。」
const SERVER_EN_RE = /You are currently connected to\s+([A-Za-z0-9_-]+)/;
const SERVER_ZH_RE = /已连接至\s*([A-Za-z0-9_-]+)/;

// 「君主: xiaofan」「邦国特产：陶瓦」——半角/全角冒号都认
const KV_RE = /^\s*([^:：]{1,20})\s*[:：]\s*(.*)$/;

// 邦国宣言通常是块的最后一行
const KINGDOM_END_RE = /邦国宣言|Kingdom\s+Motto/i;

function emptyStatus() {
  return {
    pos: null,        // {x,y,z}
    dimension: null,  // 'minecraft:overworld'
    server: null,     // 'main'
    kingdom: null,    // {title, fields, raw, updatedAt}
    updatedAt: null,
  };
}

// 返回邦国信息块的标题，不是首行则返回 null
function parseKingdomHead(text) {
  const m = String(text || '').match(KINGDOM_HEAD_RE);
  return m ? m[1] : null;
}

// 返回服务器名，认不出则返回 null
function parseServer(text) {
  const s = String(text || '');
  const m = s.match(SERVER_EN_RE) || s.match(SERVER_ZH_RE);
  return m ? m[1] : null;
}

// 解析「键: 值」行，返回 {key, value} 或 null
function parseField(text) {
  const m = String(text || '').match(KV_RE);
  if (!m) return null;
  const key = m[1].trim();
  const value = m[2].trim();
  if (!key || !value) return null;
  return { key, value };
}

function isKingdomEnd(text) {
  return KINGDOM_END_RE.test(String(text || ''));
}

// 把累积到的原始行拼成结构化结果。
// 只有标题、没有正文的残缺块会被判为无效并返回 null。
function parseKingdomBlock(title, rawLines) {
  const lines = Array.isArray(rawLines) ? rawLines : [];
  const fields = {};
  for (const line of lines) {
    // 跳过标题行本身 —— 它也是「键: 值」形态，不排除会多出一个
    // 名为「--- 邦国信息」的伪字段
    if (KINGDOM_HEAD_RE.test(String(line || ''))) continue;
    const kv = parseField(line);
    if (kv) fields[kv.key] = kv.value;
  }
  if (Object.keys(fields).length === 0 && lines.length < 3) return null;
  return { title: title || '', fields: fields, raw: lines.slice() };
}

module.exports = {
  emptyStatus,
  parseKingdomHead,
  parseServer,
  parseField,
  isKingdomEnd,
  parseKingdomBlock,
  KINGDOM_HEAD_RE,
  SERVER_EN_RE,
  SERVER_ZH_RE,
};
