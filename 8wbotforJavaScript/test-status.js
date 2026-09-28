// test-status.js — status.js 的单测
// 直接用真实服务器 /u info 与 /server 的原始输出，验证解析结果。
const ST = require('./status');

let pass = 0, fail = 0;
function eq(name, got, want) {
  const ok = JSON.stringify(got) === JSON.stringify(want);
  if (ok) { pass++; console.log(`  ✅ ${name}`); }
  else { fail++; console.log(`  ❌ ${name}\n     实际: ${JSON.stringify(got)}\n     期望: ${JSON.stringify(want)}`); }
}

// ══════════════════════════════════════════════════════════
// 1. /u info 的原始输出（服务器插件中文原文，共 15 行）
// ══════════════════════════════════════════════════════════
const KINGDOM_LINES = [
  '--- 邦国信息: 雅典维亚城邦（二世） ---',
  '君主: xiaofan',
  '创建日期: 2025-11-03 18:17:54',
  '邦国特产: 陶瓦',
  '邦国疆土: 18079 区块（主世界 17983 + 末地 96）',
  '加入方式: 仅邀请',
  '邦国护盾: 未激活',
  '王城坐标: -7032, 168, -9447 [观看]',
  '激活的科技: 无',
  '授予的外交关系: 哔哩哔哩: 访问, 雅典维亚城邦(二世)附属国: 交互, 桃源: 交互, 浪白共和国: 交互',
  '被授予的外交关系: 牛来: 访问, 海鸢与稻荷协会: 访问, 雅典维亚城邦(附属国): 访问, 新日盟约: 访问, 哔哩哔哩: 拒绝, 云杉协会: 访问, 养老院: 交互, <拉普达>: 访问, 伏见稻荷狐域: 交互, 中立港: 访问... [显示更多]',
  '国民(5/27): xiaofan (君主 - 飞跃者 (Lv.2)), guangbo1314 (大臣), xiaofanbot (大臣 - 斥候 (Lv.2)), tianyouheWE (国民), BOHEbaicha (国民 - 飞跃者 (Lv.2))',
  '上限解锁: 国民(+1)，职业(+2)',
  '邦国宣言: 脖子右拧(for the union)',
];

console.log('\n【1】邦国信息标题');
eq('识别标题', ST.parseKingdomHead(KINGDOM_LINES[0]), '雅典维亚城邦（二世）');
eq('普通聊天不误判', ST.parseKingdomHead('君主: xiaofan'), null);
eq('冒号在中间也能认', ST.parseKingdomHead('--- 邦国信息: 桃源 ---'), '桃源');

console.log('\n【2】邦国信息整块解析');
const k = ST.parseKingdomBlock('雅典维亚城邦（二世）', KINGDOM_LINES);
eq('君主', k.fields['君主'], 'xiaofan');
eq('创建日期', k.fields['创建日期'], '2025-11-03 18:17:54');
eq('邦国特产', k.fields['邦国特产'], '陶瓦');
eq('邦国疆土（含括号和加号）', k.fields['邦国疆土'], '18079 区块（主世界 17983 + 末地 96）');
eq('加入方式', k.fields['加入方式'], '仅邀请');
eq('邦国护盾', k.fields['邦国护盾'], '未激活');
eq('王城坐标（含 [观看]）', k.fields['王城坐标'], '-7032, 168, -9447 [观看]');
eq('激活的科技', k.fields['激活的科技'], '无');
eq('外交关系值里有冒号不被截断', k.fields['授予的外交关系'].startsWith('哔哩哔哩: 访问, '), true);
eq('被授予的外交关系含 [显示更多]', k.fields['被授予的外交关系'].endsWith('... [显示更多]'), true);
eq('国民（键里带括号）', k.fields['国民(5/27)'].includes('xiaofanbot (大臣 - 斥候 (Lv.2))'), true);
eq('邦国宣言（最后一个字段）', k.fields['邦国宣言'], '脖子右拧(for the union)');
eq('上限解锁（值里有全角逗号）', k.fields['上限解锁'], '国民(+1)，职业(+2)');
eq('字段总数（标题行不计入）', Object.keys(k.fields).length, 13);
eq('没有「--- 邦国信息」伪字段', Object.keys(k.fields).some(x => x.includes('邦国信息')), false);

console.log('\n【3】结束行识别');
eq('邦国宣言是结束行', ST.isKingdomEnd('邦国宣言: 脖子右拧(for the union)'), true);
eq('普通行不是结束行', ST.isKingdomEnd('君主: xiaofan'), false);
eq('英文 Kingdom Motto 也认', ST.isKingdomEnd('Kingdom Motto: test'), true);

console.log('\n【4】/server 回复（机器人 locale 是 en_US，看到英文）');
eq('英文（mineflayer 视角）', ST.parseServer('You are currently connected to main.'), 'main');
eq('中文（玩家客户端视角）', ST.parseServer('您已连接至 main。'), 'main');
eq('服务器名带下划线', ST.parseServer('You are currently connected to lobby_2.'), 'lobby_2');
eq('可用服务器列表不误判', ST.parseServer('可用的服务器： auth, main'), null);
eq('无关聊天不误判', ST.parseServer('你好世界'), null);

console.log('\n【5】残缺块要丢弃');
eq('只有标题', ST.parseKingdomBlock('测试', ['--- 邦国信息: 测试 ---']), null);
eq('有正文则保留', ST.parseKingdomBlock('测试', ['--- 邦国信息: 测试 ---', '君主: a']).fields['君主'], 'a');

console.log('\n【6】半角/全角冒号');
eq('半角', ST.parseField('君主: xiaofan').value, 'xiaofan');
eq('全角', ST.parseField('邦国特产：陶瓦').value, '陶瓦');
eq('值里含全角冒号', ST.parseField('时间: 12：30').value, '12：30');
eq('不以冒号开头则不是字段', ST.parseField('这只是一句话'), null);

console.log(`\n══════ 通过 ${pass} 项，失败 ${fail} 项 ══════\n`);
process.exit(fail === 0 ? 0 : 1);
