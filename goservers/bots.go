package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Bot CRUD functions

func createBot(db *sql.DB, b *BotData) error {
	_, err := db.Exec(`INSERT INTO bots(belong, creation_time, username, dsl, status, auto_restore, auto_reconnect) VALUES(?,?,?,?,?,?,?)`,
		b.Belong, b.CreationTime, b.Username, boolToInt(b.DSL), b.Status, boolToInt(b.AutoRestore), boolToInt(b.AutoReconnect))
	return err
}

func getBotsByUser(db *sql.DB, belong string) ([]BotData, error) {
	rows, err := db.Query("SELECT belong, creation_time, username, dsl, COALESCE(status,'no'), COALESCE(auto_restore,1), COALESCE(auto_reconnect,1), COALESCE(last_exit_reason,''), COALESCE(last_exit_type,''), COALESCE(last_exit_time,'') FROM bots WHERE belong = ? ORDER BY id", belong)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []BotData
	for rows.Next() {
		var b BotData
		var dslInt int64
		var statusStr string
		var autoRestoreInt int64
		var autoReconnectInt int64
		if err := rows.Scan(&b.Belong, &b.CreationTime, &b.Username, &dslInt, &statusStr, &autoRestoreInt, &autoReconnectInt,
			&b.LastExitReason, &b.LastExitType, &b.LastExitTime); err != nil {
			return nil, err
		}
		b.DSL = dslInt != 0
		b.Status = statusStr
		b.AutoRestore = autoRestoreInt != 0
		b.AutoReconnect = autoReconnectInt != 0
		bots = append(bots, b)
	}
	return bots, nil
}

func countBotsByUser(db *sql.DB, belong string) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM bots WHERE belong = ?", belong).Scan(&count)
	return count, err
}

func deleteBot(db *sql.DB, belong string, username string) error {
	_, err := db.Exec("DELETE FROM bots WHERE belong = ? AND username = ?", belong, username)
	return err
}

func setBotDSL(db *sql.DB, belong string, username string, enabled bool) error {
	_, err := db.Exec("UPDATE bots SET dsl = ? WHERE belong = ? AND username = ?", boolToInt(enabled), belong, username)
	return err
}

// findBotByUsername returns the bot and its status, or nil if not found.
func findBotByUsername(db *sql.DB, username string) (*BotData, error) {
	row := db.QueryRow("SELECT belong, creation_time, username, dsl, COALESCE(status,'no'), COALESCE(auto_restore,1), COALESCE(auto_reconnect,1) FROM bots WHERE username = ?", username)
	var b BotData
	var dslInt int64
	var autoRestoreInt int64
	var autoReconnectInt int64
	err := row.Scan(&b.Belong, &b.CreationTime, &b.Username, &dslInt, &b.Status, &autoRestoreInt, &autoReconnectInt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.DSL = dslInt != 0
	b.AutoRestore = autoRestoreInt != 0
	b.AutoReconnect = autoReconnectInt != 0
	return &b, nil
}

// updateBotOwner reassigns a bot to a new owner (used when username exists with status 'no')
func updateBotOwner(db *sql.DB, username string, newBelong string) error {
	_, err := db.Exec("UPDATE bots SET belong = ? WHERE username = ?", newBelong, username)
	return err
}

// updateBotStatus changes a bot's status, scoped to its owner.
// 必须带上 belong 条件: 归属校验与写入之间存在竞态窗口，
// 若在此期间机器人被他人重新认领，无条件更新会误改他人的机器人。
// 返回受影响行数，调用方可用它判断是否真的改中（0 表示归属已变更）。
func updateBotStatus(db *sql.DB, username string, belong string, status string) (int64, error) {
	res, err := db.Exec("UPDATE bots SET status = ? WHERE username = ? AND belong = ?", status, username, belong)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// setBotAutoReconnect updates the auto_reconnect flag for a bot.
func setBotAutoReconnect(db *sql.DB, username string, enabled bool) error {
	_, err := db.Exec("UPDATE bots SET auto_reconnect = ? WHERE username = ?", boolToInt(enabled), username)
	return err
}

// setBotAutoRestore updates the auto_restore flag for a bot.
func setBotAutoRestore(db *sql.DB, username string, enabled bool) error {
	_, err := db.Exec("UPDATE bots SET auto_restore = ? WHERE username = ?", boolToInt(enabled), username)
	return err
}

// setBotManualStop 标记「这个机器人是用户主动下线的」。
//
// 全局自动重连巡护必须靠它区分两种离线状态，否则会出事：
//
//	manual_stop = 0 —— 意外掉线（被服务器踢、网络抖动），应该自动拉起来
//	manual_stop = 1 —— 用户自己点了「下线」，必须保持关闭
//
// 少了这个标记，巡护会在用户下线后立刻把机器人重新拉起来，下线按钮等于失效。
func setBotManualStop(db *sql.DB, username string, stopped bool) error {
	_, err := db.Exec("UPDATE bots SET manual_stop = ? WHERE username = ?", boolToInt(stopped), username)
	return err
}

// autoReconnectCandidates 返回需要自动重连巡护的机器人名单：
// 开了自动重连、已完成归属验证、并且不是用户主动下线的。
func autoReconnectCandidates(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT username FROM bots WHERE auto_reconnect = 1 AND status = 'confirmed' AND COALESCE(manual_stop,0) = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// ════════════════════════════════════════════════════════════════
// 浏览器实时状态流
//
// 前端只开一条 /ws/api/stream 订阅自己名下所有机器人，切页面也不断开。
// 数据来自 Go 到 JS 节点的一条常驻订阅（/ws/api/events），而不是让每个
// 浏览器各自去连 JS 节点 —— 后者会让连接数随「用户数 × 机器人数」爆炸，
// 而且浏览器并不知道内部密钥。
// ════════════════════════════════════════════════════════════════

type botStreamSub struct {
	conn *websocket.Conn
	bots map[string]bool // 该用户名下的机器人，作为转发白名单
	send chan []byte
	once sync.Once
	done chan struct{}
}

// writeLoop 是这条连接唯一的写入者。
//
// gorilla/websocket 不允许多个 goroutine 并发写同一条连接，而心跳 ping 和
// 业务推送天然来自两个 goroutine，直接写会触发 concurrent write 崩溃。
// 把写操作全部收敛到这个循环里就根除了这个隐患。
func (s *botStreamSub) writeLoop() {
	const pingPeriod = 30 * time.Second
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case msg := <-s.send:
			s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *botStreamSub) close() {
	s.once.Do(func() { close(s.done) })
}

// push 非阻塞投递。客户端太慢时丢帧，而不是拖住整个事件循环。
func (s *botStreamSub) push(msg []byte) {
	select {
	case s.send <- msg:
	case <-s.done:
	default:
	}
}

var (
	botStreamMu   sync.RWMutex
	botStreamSubs = map[*botStreamSub]bool{}
)

func addBotStreamSub(s *botStreamSub) {
	botStreamMu.Lock()
	botStreamSubs[s] = true
	botStreamMu.Unlock()
}

func removeBotStreamSub(s *botStreamSub) {
	botStreamMu.Lock()
	delete(botStreamSubs, s)
	botStreamMu.Unlock()
	s.close()
}

// pushToBrowsers 把某个机器人的事件推给所有有权看它的浏览器连接
func pushToBrowsers(botname string, payload []byte) {
	botStreamMu.RLock()
	var targets []*botStreamSub
	for s := range botStreamSubs {
		if s.bots[botname] {
			targets = append(targets, s)
		}
	}
	botStreamMu.RUnlock()
	for _, s := range targets {
		s.push(payload)
	}
}

// saveBotStatus 把 JS 节点上报的实时状态落库。
// 落库的意义：用户切页面、关掉控制台、或者后端重启之后，卡片上仍然能显示
// 最近一次已知的位置与邦国信息，而不是一片空白。
func saveBotStatus(db *sql.DB, botname string, status json.RawMessage) error {
	_, err := db.Exec("UPDATE bots SET status_json = ?, status_time = ? WHERE username = ?",
		string(status), time.Now().Format(time.RFC3339), botname)
	return err
}

// botStatusRow 供 /api/getmybotslist 与 /ws/api/stream 读取缓存状态
func botStatusRow(db *sql.DB, botname string) (string, string) {
	var js, ts string
	row := db.QueryRow("SELECT COALESCE(status_json,''), COALESCE(status_time,'') FROM bots WHERE username = ?", botname)
	if err := row.Scan(&js, &ts); err != nil {
		return "", ""
	}
	return js, ts
}

// jsEventsLoop 常驻订阅 JS 节点的全局事件流并扇出给浏览器。
// 这条链路断了等于所有实时状态都停摆，所以断线要无限重连。
func jsEventsLoop(db *sql.DB) {
	for {
		if err := jsEventsOnce(db); err != nil {
			log.Printf("[EVENTS] 事件订阅中断，5 秒后重连: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func jsEventsOnce(db *sql.DB) error {
	conn, _, err := jsDialer.Dial(getJSNodeURL("/ws/api/events"), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"all":true}`)); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	log.Printf("[EVENTS] 已订阅 JS 节点全局事件流")

	stopPing := wsPoller(conn)
	defer stopPing()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		handleNodeEvent(db, msg)
	}
}

// handleNodeEvent 处理 JS 节点推来的一帧事件。
// 目前只关心 status（落库 + 推送）与 bot_offline（推送），
// 聊天和地图仍然走 /ws/api/connectbot，避免重复占用带宽。
func handleNodeEvent(db *sql.DB, raw []byte) {
	var events []struct {
		BotName string `json:"botname"`
		Data    []struct {
			Status     json.RawMessage `json:"status"`
			BotOffline bool            `json:"bot_offline"`
			Reason     string          `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &events); err != nil {
		return
	}
	for _, ev := range events {
		if ev.BotName == "" {
			continue
		}
		for _, d := range ev.Data {
			if len(d.Status) > 0 && string(d.Status) != "null" {
				if err := saveBotStatus(db, ev.BotName, d.Status); err != nil {
					log.Printf("[EVENTS] 状态落库失败 %s: %v", ev.BotName, err)
				}
				out, _ := json.Marshal([]map[string]interface{}{{
					"botname": ev.BotName,
					"data":    []map[string]interface{}{{"status": d.Status}},
				}})
				pushToBrowsers(ev.BotName, out)
			}
			if d.BotOffline {
				out, _ := json.Marshal([]map[string]interface{}{{
					"botname": ev.BotName,
					"data":    []map[string]interface{}{{"bot_offline": true, "reason": d.Reason}},
				}})
				pushToBrowsers(ev.BotName, out)
			}
		}
	}
}

// 给 /api/getmybotslist 用的小包装：取不到就返回空串，不让单个机器人
// 的状态缺失影响整个列表接口。
func mustBotStatusJSON(botname string) string {
	js, _ := botStatusRow(globalDB, botname)
	if js == "" || !json.Valid([]byte(js)) {
		return ""
	}
	return js
}

func mustBotStatusTime(botname string) string {
	_, ts := botStatusRow(globalDB, botname)
	return ts
}

// ════════════════════════════════════════════════════════════════
// squaremap 地图数据
//
// 服务器自带 squaremap 网页地图（默认 https://bgjq.simpfun.cn）。
// 爬它有两个用处：
//   ① 生成「在地图上查看该坐标」的链接 —— URL 形如
//      ?world=<世界名>&zoom=<缩放>&x=<x>&z=<z>
//   ② 用 markers.json 里的疆土多边形判断机器人当前站在谁的领地上，
//      以及离哪个王城最近 —— 光看 XYZ 是不知道自己在哪儿的
//
// 重要事实（实测确认）：
//   - squaremap 的标记坐标就是游戏方块坐标，不需要任何换算。
//     用 /u info 报的王城坐标 (-7032, -9447) 去对 markers.json 里
//     同名的王城标记，偏差 dx=0, dz=0。下界也没有坐标缩放。
//   - 三个世界的 player_tracker.enabled 都是 false，地图上【没有】玩家
//     位置，所以机器人坐标只能靠 mineflayer 上报，不能从地图拿。
//
// 缓存放服务端而不是让浏览器直接拉：markers.json 有 160KB 且 6500 多个
// 多边形顶点，每个用户各自下载和做点在多边形判定并不划算。
// ════════════════════════════════════════════════════════════════

const defaultSquaremapBase = "https://bgjq.simpfun.cn"

type mapPoint struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

type mapClaim struct {
	Owner  string       `json:"owner"`
	Shield bool         `json:"shield"`
	Rings  [][]mapPoint `json:"-"`
}

type mapCapital struct {
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Z    float64 `json:"z"`
}

type mapWorldInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

type mapWorldData struct {
	Capitals []mapCapital
	Claims   []mapClaim
}

type squaremapCache struct {
	Base    string
	Worlds  []mapWorldInfo
	ByWorld map[string]*mapWorldData
}

var (
	squaremapMu   sync.RWMutex
	squaremapData *squaremapCache
)

func squaremapBase() string {
	if v := os.Getenv("SQUAREMAP_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultSquaremapBase
}

func fetchSquaremapJSON(url string, out interface{}) error {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	// 带个正经 UA，别给人家服务器添乱
	req.Header.Set("User-Agent", "8wbot/1.0 (+https://8w.bgjq.top)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// collectRings 递归收集「点字典构成的列表」。
//
// 必须递归的原因：squaremap 不同标记组的嵌套深度不一样。
//
//	income_zones:  points = [ring, ring]              环的列表
//	union_claims:  points = [[ring], [ring], [ring]]  多边形的列表，每个多边形含若干环
//
// 硬编码任一种都会在另一种上失效。
func collectRings(v interface{}, out *[][]mapPoint) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return
	}
	// 先试着把这一层整体当成一个环
	pts := make([]mapPoint, 0, len(arr))
	allPoints := true
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			allPoints = false
			break
		}
		x, okx := m["x"].(float64)
		z, okz := m["z"].(float64)
		if !okx || !okz {
			allPoints = false
			break
		}
		pts = append(pts, mapPoint{X: x, Z: z})
	}
	if allPoints && len(pts) >= 3 {
		*out = append(*out, pts)
		return
	}
	for _, e := range arr {
		collectRings(e, out)
	}
}

// pointInRings 判断点是否落在这些环围成的区域内，用奇偶规则。
// 奇偶规则对两种情形都正确：多块互不相连的领地（落在其中任一块内即为真），
// 以及带空洞的领地（外环内、洞内 → 命中两次 → 判为不在）。
func pointInRings(x, z float64, rings [][]mapPoint) bool {
	inside := false
	for _, ring := range rings {
		n := len(ring)
		if n < 3 {
			continue
		}
		for i, j := 0, n-1; i < n; j, i = i, i+1 {
			xi, zi := ring[i].X, ring[i].Z
			xj, zj := ring[j].X, ring[j].Z
			// zj == zi 时这个条件必然为假，不会出现除零
			if (zi > z) != (zj > z) {
				if x < (xj-xi)*(z-zi)/(zj-zi)+xi {
					inside = !inside
				}
			}
		}
	}
	return inside
}

var claimOwnerRe = regexp.MustCompile(`疆土归属:\s*([^<]*)`)
var claimShieldRe = regexp.MustCompile(`护盾开启:\s*([^<]*)`)

// loadSquaremap 抓取一次全量地图数据。失败时保留旧缓存。
func loadSquaremap() (*squaremapCache, error) {
	base := squaremapBase()

	var settings struct {
		Worlds []mapWorldInfo `json:"worlds"`
	}
	if err := fetchSquaremapJSON(base+"/tiles/settings.json", &settings); err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	if len(settings.Worlds) == 0 {
		return nil, fmt.Errorf("settings.json 里没有世界列表")
	}

	cache := &squaremapCache{Base: base, Worlds: settings.Worlds, ByWorld: map[string]*mapWorldData{}}

	for _, w := range settings.Worlds {
		// markers.json 的元素结构与标记组一一对应；points 用 RawMessage
		// 原样接住，交给 collectRings 递归处理嵌套差异。
		var raw []struct {
			ID      string `json:"id"`
			Markers []struct {
				Type   string          `json:"type"`
				Popup  string          `json:"popup"`
				Point  *mapPoint       `json:"point"`
				Points json.RawMessage `json:"points"`
			} `json:"markers"`
		}
		url := base + "/tiles/" + w.Name + "/markers.json"
		if err := fetchSquaremapJSON(url, &raw); err != nil {
			// 某个世界抓失败不影响其他世界
			log.Printf("[MAP] %s 标记抓取失败: %v", w.Name, err)
			cache.ByWorld[w.Name] = &mapWorldData{}
			continue
		}

		wd := &mapWorldData{}
		for _, group := range raw {
			for _, m := range group.Markers {
				switch group.ID {
				case "union_capitals":
					if m.Point == nil {
						continue
					}
					name := stripHTMLTags(m.Popup)
					name = strings.TrimSuffix(name, "的王城")
					name = strings.TrimSuffix(name, "的据点")
					name = strings.Trim(name, "「」 ")
					if name == "" {
						continue
					}
					wd.Capitals = append(wd.Capitals, mapCapital{Name: name, X: m.Point.X, Z: m.Point.Z})
				case "union_claims":
					if len(m.Points) == 0 {
						continue
					}
					var pts interface{}
					if err := json.Unmarshal(m.Points, &pts); err != nil {
						continue
					}
					var rings [][]mapPoint
					collectRings(pts, &rings)
					if len(rings) == 0 {
						continue
					}
					owner, shield := "", false
					if mm := claimOwnerRe.FindStringSubmatch(m.Popup); len(mm) > 1 {
						owner = strings.TrimSpace(mm[1])
					}
					if mm := claimShieldRe.FindStringSubmatch(m.Popup); len(mm) > 1 {
						shield = strings.TrimSpace(mm[1]) == "是"
					}
					wd.Claims = append(wd.Claims, mapClaim{Owner: owner, Shield: shield, Rings: rings})
				}
			}
		}
		cache.ByWorld[w.Name] = wd
	}

	return cache, nil
}

// stripHTMLTags 去掉 popup 里的标签与实体，只留纯文本
func stripHTMLTags(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return strings.TrimSpace(s)
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// squaremapLoop 周期性刷新地图缓存。
// 这些数据变化很慢（疆土和王城），10 分钟一次足够，也不用给地图服务器压力。
func squaremapLoop() {
	interval := 10 * time.Minute
	if v := os.Getenv("SQUAREMAP_REFRESH"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Minute {
			interval = d
		}
	}

	refresh := func() {
		cache, err := loadSquaremap()
		if err != nil {
			log.Printf("[MAP] 地图数据刷新失败（保留旧缓存）: %v", err)
			return
		}
		squaremapMu.Lock()
		squaremapData = cache
		squaremapMu.Unlock()
		total := 0
		for _, wd := range cache.ByWorld {
			total += len(wd.Claims)
		}
		log.Printf("[MAP] 地图数据已加载: %d 个世界，%d 块疆土", len(cache.Worlds), total)
	}

	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		refresh()
	}
}

// squaremapSnapshot 取当前缓存的只读快照
func squaremapSnapshot() *squaremapCache {
	squaremapMu.RLock()
	defer squaremapMu.RUnlock()
	return squaremapData
}

// mapLookup 查询某个坐标所在的世界信息、所处疆土与最近王城
func mapLookup(world string, x, z float64) (map[string]interface{}, bool) {
	cache := squaremapSnapshot()
	if cache == nil {
		return nil, false
	}

	// 前端传的是 minecraft:overworld 这种维度名，地图用的是 minecraft_overworld
	worldName := world
	for _, w := range cache.Worlds {
		if w.Name == world || w.DisplayName == world {
			worldName = w.Name
			break
		}
	}

	out := map[string]interface{}{
		"base":   cache.Base,
		"worlds": cache.Worlds,
		"world":  worldName,
	}

	// 固定 zoom 3 是 squaremap 的默认与最大缩放，打开即是街区级视角
	out["map_url"] = fmt.Sprintf("%s/?world=%s&zoom=3&x=%d&z=%d",
		cache.Base, worldName, int(math.Floor(x)), int(math.Floor(z)))

	wd := cache.ByWorld[worldName]
	if wd == nil {
		return out, true
	}

	// 所处疆土：可能同时落在多块里（重叠领地），只报第一块命中的
	for _, c := range wd.Claims {
		if pointInRings(x, z, c.Rings) {
			out["territory"] = map[string]interface{}{"owner": c.Owner, "shield": c.Shield}
			break
		}
	}

	// 最近王城
	best := -1
	bestDist := 0.0
	for i, c := range wd.Capitals {
		dx, dz := c.X-x, c.Z-z
		d := math.Sqrt(dx*dx + dz*dz)
		if best < 0 || d < bestDist {
			best, bestDist = i, d
		}
	}
	if best >= 0 {
		c := wd.Capitals[best]
		out["nearest_capital"] = map[string]interface{}{
			"name": c.Name, "x": int(c.X), "z": int(c.Z), "distance": int(bestDist),
		}
	}
	return out, true
}
