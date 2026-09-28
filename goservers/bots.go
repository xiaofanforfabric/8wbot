package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
			// 扩地进度/结果。原样透传给浏览器，Go 不解析里面字段 ——
			// 结构由 JS 节点与前端约定，中间层跟着解析容易两边不同步。
			Expand json.RawMessage `json:"expand"`
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
			if len(d.Expand) > 0 && string(d.Expand) != "null" {
				out, _ := json.Marshal([]map[string]interface{}{{
					"botname": ev.BotName,
					"data":    []map[string]interface{}{{"expand": d.Expand}},
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
// squaremap 地图数据代理
//
// 设计取舍：这里【只做代理】，不做任何解析。
//
// 解析（160KB JSON、6500 多个多边形顶点、点在多边形判定）全部放在浏览器里做，
// 后端只负责把原始 JSON 缓存住再转发出去，CPU 开销降到几乎为零。
//
// 为什么不让浏览器直接抓 squaremap：
//   bgjq.simpfun.cn 是裸 nginx，任何路径都没有 Access-Control-Allow-Origin，
//   OPTIONS 预检直接 405。跨域 fetch 拿到的响应浏览器读不了。
//
// 为什么后端缓存仍然比浏览器直抓省：
//   全局只抓一次给所有用户用，而不是每个用户各抓一次。
//
// 按需懒加载：没有 /api/mapmarkers 请求就不抓，没人用就没有任何流量。
// 配合 ETag 条件请求，数据没变时 squaremap 返回 304，正文零传输。
// ════════════════════════════════════════════════════════════════

const defaultSquaremapBase = "https://bgjq.simpfun.cn"

func squaremapBase() string {
	if v := os.Getenv("SQUAREMAP_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultSquaremapBase
}

func squaremapTTL() time.Duration {
	if v := os.Getenv("SQUAREMAP_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Minute {
			return d
		}
	}
	return 30 * time.Minute
}

type squaremapEntry struct {
	Body []byte
	ETag string
	At   time.Time
}

var (
	squaremapMu    sync.Mutex
	squaremapCache = map[string]*squaremapEntry{} // 路径 -> 缓存
	squaremapHTTP  = &http.Client{Timeout: 30 * time.Second}
)

// fetchSquaremapRaw 取一个路径的原始 JSON。
// 带 If-None-Match：squaremap 没变化时回 304，正文不传，直接沿用旧缓存。
func fetchSquaremapRaw(path string) ([]byte, error) {
	full := squaremapBase() + path

	squaremapMu.Lock()
	entry := squaremapCache[path]
	ttl := squaremapTTL()
	if entry != nil && time.Since(entry.At) < ttl {
		body := entry.Body
		squaremapMu.Unlock()
		return body, nil
	}
	etag := ""
	if entry != nil {
		etag = entry.ETag
	}
	squaremapMu.Unlock()

	req, err := http.NewRequest("GET", full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "8wbot/1.0 (+https://8w.bgjq.top)")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := squaremapHTTP.Do(req)
	if err != nil {
		// 网络不通时退回旧缓存，总比让前端拿不到数据好
		squaremapMu.Lock()
		if entry != nil {
			body := entry.Body
			entry.At = time.Now()
			squaremapMu.Unlock()
			log.Printf("[MAP] %s 抓取失败，沿用旧缓存: %v", path, err)
			return body, nil
		}
		squaremapMu.Unlock()
		return nil, err
	}
	defer resp.Body.Close()

	// 304：内容没变，续期即可
	if resp.StatusCode == http.StatusNotModified {
		squaremapMu.Lock()
		if entry != nil {
			entry.At = time.Now()
			body := entry.Body
			squaremapMu.Unlock()
			log.Printf("[MAP] %s 未变化（304），沿用缓存", path)
			return body, nil
		}
		squaremapMu.Unlock()
		return nil, fmt.Errorf("收到 304 但没有本地缓存")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 上限 8MB，防异常大响应
	if err != nil {
		return nil, err
	}

	squaremapMu.Lock()
	squaremapCache[path] = &squaremapEntry{Body: body, ETag: resp.Header.Get("ETag"), At: time.Now()}
	squaremapMu.Unlock()

	log.Printf("[MAP] %s 已抓取 %d 字节", path, len(body))
	return body, nil
}

// squaremapWorldRe 世界名白名单。世界名会被拼进 squaremap 的 URL 路径，
// 不做限制的话 ../ 就能打到任意路径。
var squaremapWorldRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// checkJWT 只做令牌有效性判断，用于不需要区分用户的只读接口
func checkJWT(tok string) bool {
	if tok == "" {
		return false
	}
	t, err := jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return jwtSecret, nil
	})
	return err == nil && t.Valid
}
