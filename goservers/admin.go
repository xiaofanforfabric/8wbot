package main

// admin.go —— 管理员通道
//
// ════════════════════════════════════════════════════════════════
// 设计要点
// ════════════════════════════════════════════════════════════════
//
// 管理员 = FanVerify UID 为 0 的用户（见 isAdminUser）。借用 Linux 的约定：
// UID 0 就是 root。站长在 FanVerify 那边是 UID 0，所以在本站也是管理员。
//
// 管理接口【只挂在 WebSocket 上】，一个 HTTP 路由都不注册。
// 这不是靠权限判断实现的，而是靠「路径根本不存在」：直接 curl /api/admin/*
// 会落到 mux 的兜底分支返回 404，跟访问一个不存在的普通路径完全一样。
// 连探测「这里有没有管理接口」都做不到。
//
// 所有管理操作共用一条 WS：/ws/api/admin
//   连上 → 用 access_token 鉴权 → 非管理员直接 4003 关闭
//   然后发 {action: "...", ...} 请求，服务端回 {code, ...}
//
// ════════════════════════════════════════════════════════════════

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

// adminAuth 从 JWT 解析出管理员身份。
//
// 返回值 ok=false 时，message 是可以直接给用户看的原因。
// 注意这里对「令牌无效」和「不是管理员」返回同样的处理路径 ——
// 不给探测者区分「这个 token 有效但没权限」的机会。
func adminAuth(tokenStr string) (uid string, ok bool, message string) {
	if tokenStr == "" {
		return "", false, "access_token required"
	}

	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, isHMAC := t.Method.(*jwt.SigningMethodHMAC); !isHMAC {
			return nil, jwt.ErrSignatureInvalid
		}
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return "", false, "无效的access_token"
	}

	claims, _ := token.Claims.(jwt.MapClaims)
	uid, _ = claims["jht_uid"].(string)
	if uid == "" {
		return "", false, "无效的access_token"
	}

	u, err := findUser(globalDB, uid)
	if err != nil {
		log.Printf("[ADMIN] 查询用户失败 %s: %v", uid, err)
		return "", false, "服务异常"
	}
	if u == nil {
		return "", false, "用户不存在"
	}
	if !isAdminUser(u) {
		// 日志里记下是谁在敲门，但回给客户端的和对无效令牌一样，不泄露信息
		log.Printf("[ADMIN] 拒绝非管理员访问: uid=%s fanverify_uid=%d", uid, u.FanverifyUID)
		return "", false, "403 forbidden"
	}
	return uid, true, ""
}

// registerAdminWS 注册管理员 WebSocket 通道。
//
// 唯一的入口。任何 HTTP 路径都不注册 —— 这是「硬连也只会 404」的实现方式。
func registerAdminWS(mux *http.ServeMux, upgrader *websocket.Upgrader) {
	mux.HandleFunc("/ws/api/admin", func(w http.ResponseWriter, r *http.Request) {
		// ── 第一层：先升级成 WS，再在 WS 里鉴权 ──
		//
		// 为什么鉴权不放 URL query 里？query 会进 access log、浏览器历史、
		// Referer。令牌走 WS 的第一帧，不进 URL。
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// ── 第二层：等鉴权帧 ──
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var auth struct {
			AccessToken string `json:"access_token"`
		}
		if err := conn.ReadJSON(&auth); err != nil {
			return
		}

		uid, ok, msg := adminAuth(auth.AccessToken)
		if !ok {
			// 4003 = 自定义的「禁止」关闭码。前端靠它区分「没权限」和「网络断了」。
			conn.WriteJSON(map[string]interface{}{"code": 403, "message": msg})
			conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(4003, "forbidden"), time.Now().Add(3*time.Second))
			log.Printf("[ADMIN] 鉴权失败: %s", msg)
			return
		}

		log.Printf("[ADMIN] 管理员已连接: uid=%s", uid)
		conn.SetReadDeadline(time.Time{})

		conn.WriteJSON(map[string]interface{}{
			"code":    200,
			"message": "admin channel ready",
			"uid":     uid,
		})

		// 注册进管理员表，才能收到周期性的全局地图推送
		ac := &adminConn{uid: uid, send: make(chan []byte, 32), done: make(chan struct{})}
		adminMu.Lock()
		adminConns[ac] = true
		adminMu.Unlock()
		defer func() {
			adminMu.Lock()
			delete(adminConns, ac)
			adminMu.Unlock()
			ac.close()
		}()

		// 单写入者：管理通道的响应都从这个 goroutine 出去，
		// 免得和心跳 ping 撞车（同 botStreamSub 的理由）
		send := make(chan []byte, 32)
		done := make(chan struct{})
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case m := <-send:
					conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := conn.WriteMessage(websocket.TextMessage, m); err != nil {
						return
					}
				case <-ticker.C:
					if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
						return
					}
				case <-done:
					return
				}
			}
		}()
		defer close(done)

		// 管理员的收件箱（ac.send）和命令回复（send）都汇到这一条连接上写。
		// gorilla 不允许多 goroutine 并发写，所以必须有且只有一个写者。
		go func() {
			for {
				select {
				case m := <-ac.send:
					conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := conn.WriteMessage(websocket.TextMessage, m); err != nil {
						return
					}
				case <-done:
					return
				}
			}
		}()

		reply := func(v interface{}) {
			b, err := json.Marshal(v)
			if err != nil {
				return
			}
			select {
			case send <- b:
			case <-done:
			}
		}

		// ── 第三层：命令循环 ──
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(120 * time.Second))
			return nil
		})

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[ADMIN] 管理员断开: uid=%s", uid)
				return
			}
			conn.SetReadDeadline(time.Now().Add(120 * time.Second))

			var req struct {
				Action string `json:"action"`
				UID    string `json:"uid"`
				Target string `json:"target"`
				Delta  *int64 `json:"delta"`
				Quota  *int64 `json:"quota"`
				Reason string `json:"reason"`
				Ban    *bool  `json:"ban"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				reply(map[string]interface{}{"code": 400, "message": "invalid json"})
				continue
			}

			reply(handleAdminAction(uid, req.Action, req.UID, req.Target, req.Delta, req.Quota, req.Reason, req.Ban))
		}
	})
}

// ════════════════════════════════════════════════════════════════
// 管理员连接的注册表 —— 用于主动推送（全局地图）
// ════════════════════════════════════════════════════════════════

var (
	adminMu    sync.RWMutex
	adminConns = map[*adminConn]bool{}
)

type adminConn struct {
	uid  string
	send chan []byte
	once sync.Once
	done chan struct{}
}

func (a *adminConn) close() {
	a.once.Do(func() { close(a.done) })
}

// push 非阻塞投递。管理员客户端太慢就丢帧，不拖住推送循环。
func (a *adminConn) push(msg []byte) {
	select {
	case a.send <- msg:
	case <-a.done:
	default:
	}
}

// adminBroadcast 把一帧数据推给所有在线管理员。
// 推的是同一份字节切片，接收方只读，不需要各自拷贝。
func adminBroadcast(msg []byte) {
	adminMu.RLock()
	targets := make([]*adminConn, 0, len(adminConns))
	for a := range adminConns {
		targets = append(targets, a)
	}
	adminMu.RUnlock()
	for _, a := range targets {
		a.push(msg)
	}
}

// adminMapLoop 周期性把所有机器人的实时位置推给在线管理员。
//
// 只推给管理员，所以普通用户的 /ws/api/stream 完全不受影响 ——
// 他们那条流里的 botStreamSub.bots 白名单照旧只含自己名下的机器人。
func adminMapLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		adminMu.RLock()
		n := len(adminConns)
		adminMu.RUnlock()
		if n == 0 {
			continue // 没人在看就不查库
		}
		res := adminListBots()
		payload, err := json.Marshal(map[string]interface{}{
			"code": 200,
			"type": "map_snapshot",
			"bots": res["bots"],
		})
		if err != nil {
			continue
		}
		adminBroadcast(payload)
	}
}

// handleAdminAction 分发一条管理指令。
//
// 每个 action 都独立返回一个 map，里面必定有 code 字段：
//
//	200 成功 / 400 参数错 / 403 无权限 / 404 对象不存在 / 500 服务异常
func handleAdminAction(adminUID, action, uid, target string, delta, quota *int64, reason string, ban *bool) map[string]interface{} {
	switch action {

	// ── 列出全部机器人（跨用户）──
	case "list_bots":
		return adminListBots()

	// ── 列出全部用户 ──
	case "list_users":
		return adminListUsers()

	// ── 改配额：quota 是设成多少，delta 是加减多少，二选一 ──
	case "set_quota":
		if uid == "" {
			return adminErr(400, "uid required")
		}
		if quota == nil && delta == nil {
			return adminErr(400, "quota 或 delta 必须给一个")
		}
		u, err := findUser(globalDB, uid)
		if err != nil {
			return adminErr(500, "db error: "+err.Error())
		}
		if u == nil {
			return adminErr(404, "user not found: "+uid)
		}
		old := u.RemainingBotCreationQuantity
		if quota != nil {
			u.RemainingBotCreationQuantity = *quota
		} else {
			u.RemainingBotCreationQuantity = old + *delta
		}
		if u.RemainingBotCreationQuantity < 0 {
			u.RemainingBotCreationQuantity = 0
		}
		if err := upsertUser(globalDB, u); err != nil {
			return adminErr(500, "db error: "+err.Error())
		}
		log.Printf("[ADMIN] %s 把 %s 的配额 %d → %d", adminUID, uid, old, u.RemainingBotCreationQuantity)
		return map[string]interface{}{
			"code": 200, "uid": uid,
			"old": old, "quota": u.RemainingBotCreationQuantity,
			"message": fmt.Sprintf("配额 %d → %d", old, u.RemainingBotCreationQuantity),
		}

	// ── 封禁 / 解封 ──
	case "set_ban":
		if uid == "" {
			return adminErr(400, "uid required")
		}
		if ban == nil {
			return adminErr(400, "ban required (true=封禁, false=解封)")
		}
		u, err := findUser(globalDB, uid)
		if err != nil {
			return adminErr(500, "db error: "+err.Error())
		}
		if u == nil {
			return adminErr(404, "user not found: "+uid)
		}
		if *ban {
			u.Status = "ban"
			u.StatusInfo = reason
		} else {
			u.Status = "ok"
			u.StatusInfo = reason
		}
		if err := upsertUser(globalDB, u); err != nil {
			return adminErr(500, "db error: "+err.Error())
		}
		// 内存 map 才是真正的执法点，必须同步改。
		// 落库是为了重启后 loadBannedUsers 能重新加载 —— 两者缺一不可。
		if *ban {
			bannedUsers.Store(uid, true)
		} else {
			bannedUsers.Delete(uid)
		}
		word := "解封"
		if *ban {
			word = "封禁"
		}
		log.Printf("[ADMIN] %s %s 了 %s（%s）", adminUID, word, uid, reason)
		return map[string]interface{}{
			"code": 200, "uid": uid, "banned": *ban,
			"message": word + "成功",
		}

	default:
		return adminErr(400, "unknown action: "+action)
	}
}

func adminErr(code int, msg string) map[string]interface{} {
	return map[string]interface{}{"code": code, "message": msg}
}

// ────────────────────────────────────────────────────────────────
// 查询类
// ────────────────────────────────────────────────────────────────

// adminListBots 跨用户列出全部机器人，附带实时状态与所在维度。
// 与 listBotsByUser 的区别只有一处：没有 WHERE belong = ?。
func adminListBots() map[string]interface{} {
	rows, err := globalDB.Query(`
		SELECT b.username, b.belong,
		       COALESCE(u.tag,''), COALESCE(u.status,'ok'),
		       COALESCE(b.status,'no'),
		       COALESCE(b.status_json,''), COALESCE(b.status_time,''),
		       COALESCE(b.last_exit_reason,''), COALESCE(b.last_exit_time,'')
		  FROM bots b
		  LEFT JOIN userdata u ON u.jht_uid = b.belong
		 ORDER BY b.belong, b.id`)
	if err != nil {
		return adminErr(500, "db error: "+err.Error())
	}
	defer rows.Close()

	type botRow struct {
		Username    string `json:"username"`
		Belong      string `json:"belong"`
		OwnerTag    string `json:"owner_tag"`
		OwnerStatus string `json:"owner_status"`
		// 库里的归属验证状态（confirmed / no），不是在线状态
		VerifyStatus string `json:"verify_status"`
		StatusTime   string `json:"status_time"`
		ExitReason   string `json:"last_exit_reason"`
		ExitTime     string `json:"last_exit_time"`
		// 实时状态（位置/维度/邦国），由 JS 节点上报后落库的那一份
		Status json.RawMessage `json:"status,omitempty"`
	}

	out := []botRow{}
	for rows.Next() {
		var r botRow
		var sj string
		if err := rows.Scan(&r.Username, &r.Belong, &r.OwnerTag, &r.OwnerStatus,
			&r.VerifyStatus, &sj, &r.StatusTime, &r.ExitReason, &r.ExitTime); err != nil {
			continue
		}
		if sj != "" && json.Valid([]byte(sj)) {
			r.Status = json.RawMessage(sj)
		}
		out = append(out, r)
	}
	return map[string]interface{}{"code": 200, "bots": out, "count": len(out)}
}

// adminListUsers 列出全部用户及其机器人数量。
func adminListUsers() map[string]interface{} {
	rows, err := globalDB.Query(`
		SELECT u.jht_uid, COALESCE(u.tag,''), COALESCE(u.level_id,0),
		       COALESCE(u.fanverify_uid,0), COALESCE(u.remaining_bot_creation_quantity,0),
		       COALESCE(u.status,'ok'), COALESCE(u.status_info,''),
		       COALESCE(u.last_login_time,''),
		       (SELECT COUNT(*) FROM bots b WHERE b.belong = u.jht_uid)
		  FROM userdata u
		 ORDER BY u.fanverify_uid, u.jht_uid`)
	if err != nil {
		return adminErr(500, "db error: "+err.Error())
	}
	defer rows.Close()

	type userRow struct {
		UID       string `json:"uid"`
		Tag       string `json:"tag"`
		LevelID   int64  `json:"level_id"`
		FanUID    int64  `json:"fanverify_uid"`
		Quota     int64  `json:"quota"`
		Status    string `json:"status"`
		StatusMsg string `json:"status_info"`
		LastLogin string `json:"last_login_time"`
		BotCount  int64  `json:"bot_count"`
		IsAdmin   bool   `json:"is_admin"`
	}

	out := []userRow{}
	for rows.Next() {
		var r userRow
		if err := rows.Scan(&r.UID, &r.Tag, &r.LevelID, &r.FanUID, &r.Quota,
			&r.Status, &r.StatusMsg, &r.LastLogin, &r.BotCount); err != nil {
			continue
		}
		r.IsAdmin = r.FanUID == 0
		out = append(out, r)
	}
	return map[string]interface{}{"code": 200, "users": out, "count": len(out)}
}
