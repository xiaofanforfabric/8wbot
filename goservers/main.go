package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

const (
	defaultListenAddr    = ":8888"
	dbFile               = "data.sqlite"
	tokenValidity        = 7 * 24 * time.Hour
	defaultFanverifyBase = "https://api.fanverify.cn"
	simpassAPIBase       = "https://simpass.simpfun.cn"
)

var (
	listenAddr           string
	fanverifyAPIBase     string
	fanverifyAccessToken string
	defaultDevUUID       string
	jwtSecret            []byte
	devUUID              string
	capSiteKey           string
	capSecretKey         string
	capServerURL         string
	corsRawOrigins       string
	corsAllowAll         bool
	corsOriginList       []string
)

// extractAccessToken 从请求中取出访问令牌。
//
// 优先使用 body 中的 access_token 字段；若为空则回退到
// Authorization: Bearer <token> 请求头 —— 前端 axios 拦截器统一注入的正是后者。
// 早期实现只认 body 字段，导致前端所有机器人管理接口都因取不到令牌而失败。
func extractAccessToken(r *http.Request, bodyToken string) string {
	if bodyToken != "" {
		return bodyToken
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return h
	}
	// 浏览器 WebSocket 无法自定义请求头，允许用查询参数传递令牌
	if r != nil {
		if q := r.URL.Query(); q != nil {
			if t := q.Get("token"); t != "" {
				return t
			}
			if t := q.Get("access_token"); t != "" {
				return t
			}
		}
	}
	return ""
}

// upstreamHTTPClient 用于所有对外部服务的调用。
//
// 必须显式设置超时：默认的 http.Get / http.Post 没有任何超时，一旦上游不可达
// （例如已下线的 simpass.simpfun.cn）就会一直挂起，最终被 Cloudflare 判为超时
// 并返回不带 CORS 头的错误页，浏览器只能看到难以排查的 "Network Error"。
var upstreamHTTPClient = &http.Client{Timeout: 15 * time.Second}

type UserData struct {
	JhtUID                       string `json:"jht_uid"`
	AccessToken                  string `json:"accesstoken"`
	RemainingBotCreationQuantity int64  `json:"remaining_bot_creation_quantity"`
	LevelID                      int64  `json:"level_id"`
	FanverifyUID                 int64  `json:"fanverify_uid"`
	CreateTime                   string `json:"create_time"`
	Level                        int64  `json:"level"`
	Tag                          string `json:"tag"`
	LastLoginTime                string `json:"last_login_time"`
	Status                       string `json:"status"`      // "ok" or "ban"
	StatusInfo                   string `json:"status_info"` // reason
}

// BotData represents a Minecraft bot bound to a user.
type BotData struct {
	Belong        string `json:"belong"`         // 简欢通UID
	CreationTime  string `json:"creation_time"`  // 创建时间
	Username      string `json:"username"`       // 游戏内用户名
	DSL           bool   `json:"dsl"`            // 是否启用DSL脚本
	Status        string `json:"status"`         // no / confirmed
	AutoRestore   bool   `json:"auto_restore"`   // 是否自动恢复连接
	AutoReconnect bool   `json:"auto_reconnect"` // 是否自动重连

	// 上次退出原因（由 JS 节点上报后落库）
	LastExitReason string `json:"last_exit_reason"`
	LastExitType   string `json:"last_exit_type"`
	LastExitTime   string `json:"last_exit_time"`
}

// saveBotExitReason 把上次退出原因写回 bots 表
func saveBotExitReason(db *sql.DB, username, reason, exitType, exitTime string) error {
	_, err := db.Exec(
		"UPDATE bots SET last_exit_reason = ?, last_exit_type = ?, last_exit_time = ? WHERE username = ?",
		reason, exitType, exitTime, username)
	return err
}

func loadEnv(path string) map[string]string {
	env := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			env[key] = val

			// 同时导出到进程环境变量。
			// wsclient.go 等处的 os.Getenv() 依赖它；早期实现只把 .env 解析进
			// map 而没有导出，导致 JS_NODE_URL / INTERNAL_NODE_SECRET 永远取不到值，
			// 跨机连接始终退回默认地址且不带鉴权密钥。
			// 已存在的真实环境变量优先，便于容器/K8s 覆盖 .env。
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
	return env
}

// initCORS 解析 CORS_ALLOWED_ORIGINS 配置。
// 支持三种写法（逗号分隔混用）：
//   - "*"                    放行所有来源（默认；API 使用 Bearer Token 鉴权，不依赖跨域 Cookie）
//   - "https://a.example.com" 精确匹配来源
//   - "*.example.com"         匹配该域名及其所有子域（支持 ESA Pages 预览域名）
func initCORS(raw string) {
	corsRawOrigins = strings.TrimSpace(raw)
	if corsRawOrigins == "" {
		corsRawOrigins = "*"
	}
	corsAllowAll = false
	corsOriginList = nil
	for _, item := range strings.Split(corsRawOrigins, ",") {
		v := strings.ToLower(strings.TrimSpace(item))
		if v == "" {
			continue
		}
		if v == "*" {
			corsAllowAll = true
			continue
		}
		corsOriginList = append(corsOriginList, v)
	}
}

// corsOriginAllowed 判断请求来源是否被允许。
func corsOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	if corsAllowAll {
		return true
	}
	o := strings.ToLower(strings.TrimSpace(origin))
	for _, allow := range corsOriginList {
		if allow == o {
			return true
		}
		// 通配子域：*.example.com 同时匹配 example.com 本身
		if strings.HasPrefix(allow, "*.") {
			suffix := allow[1:] // ".example.com"
			if strings.HasSuffix(o, suffix) {
				return true
			}
			// 形如 https://example.com 时，主机部分正好等于后缀去掉点
			if strings.HasSuffix(o, suffix[1:]) {
				host := o
				if i := strings.Index(host, "://"); i >= 0 {
					host = host[i+3:]
				}
				if host == suffix[1:] {
					return true
				}
			}
		}
	}
	return false
}

// verifyCapToken 调用 Cap 服务端校验人机验证 token。
//
// 同时发送 token 与 response 两个字段名以兼容不同版本的 Cap 实例：
//   - 新版 Cap 读取 token
//   - 部分自建/旧版实例（如 cap.fanverify.cn）仍读取 response
//
// 只发 token 时旧实例取不到该字段，会返回 500 Internal server error，
// 进而被误判为"人机验证失败"，表现为前端始终收到 403。
func verifyCapToken(capTok string) (bool, error) {
	if capTok == "" {
		return false, nil
	}
	verifyURL := capServerURL + "/siteverify"
	payload := map[string]string{
		"secret":   capSecretKey,
		"token":    capTok,
		"response": capTok,
	}
	capBody, _ := json.Marshal(payload)

	resp, err := upstreamHTTPClient.Post(verifyURL, "application/json", bytes.NewReader(capBody))
	if err != nil {
		return false, fmt.Errorf("siteverify request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(raw, &result)

	if !result.Success {
		// 保留原始响应，便于区分"token 失效"与"实例内部错误"
		log.Printf("[CAP] siteverify 校验未通过 (http %d): %s", resp.StatusCode, string(raw))
		return false, nil
	}
	return true, nil
}

// OTP session store (in-memory)
type otpSession struct {
	OtpID        string    `json:"otp_id"`
	Status       string    `json:"status"` // "wait" | "ok"
	ExpiresAt    time.Time `json:"expires_at"`
	FanverifyUID int64     `json:"fanverify_uid,omitempty"`
	CreateTime   string    `json:"create_time,omitempty"`
	Level        int64     `json:"level,omitempty"`
	Tag          string    `json:"tag,omitempty"`
	Token        string    `json:"token,omitempty"`
}

var otpSessions sync.Map
var bannedUsers sync.Map // jht_uid -> true
var globalDB *sql.DB     // accessible from ssh/webuser commands

func generateToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func main() {
	baseDir := exeDir()

	// Load .env
	envVars := loadEnv(filepath.Join(baseDir, ".env"))

	listenAddr = envVars["PORT"]
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	} else if !strings.HasPrefix(listenAddr, ":") {
		listenAddr = ":" + listenAddr
	}

	fanverifyAPIBase = envVars["FANVERIFY_API_BASE"]
	if fanverifyAPIBase == "" {
		fanverifyAPIBase = defaultFanverifyBase
	}

	fanverifyAccessToken = envVars["FANVERIFY_ACCESS_TOKEN"]
	if fanverifyAccessToken == "" {
		fanverifyAccessToken = os.Getenv("FANVERIFY_ACCESS_TOKEN")
	}
	if fanverifyAccessToken == "" {
		log.Fatalf("FATAL: FANVERIFY_ACCESS_TOKEN not set in .env or environment!")
	}
	log.Printf("FANVERIFY_ACCESS_TOKEN loaded successfully")

	devUUID = envVars["DEV_UUID"]
	if devUUID == "" {
		devUUID = os.Getenv("DEV_UUID")
	}
	if devUUID == "" {
		log.Printf("DEV_UUID not specified, skipping dev auth module")
	} else {
		log.Printf("DEV_UUID loaded")
	}
	capSiteKey = envVars["CAP_SITE_KEY"]
	if capSiteKey == "" {
		log.Fatalf("FATAL: CAP_SITE_KEY not set in .env! Cap widget will not work.")
	}
	log.Printf("CAP_SITE_KEY loaded from .env: %s", capSiteKey)

	capSecretKey = envVars["CAP_SECRET_KEY"]
	if capSecretKey == "" {
		log.Fatalf("FATAL: CAP_SECRET_KEY not set in .env! Cap verification will not work.")
	}
	log.Printf("CAP_SECRET_KEY loaded from .env")

	capServerURL = envVars["CAP_SERVER_URL"]
	if capServerURL == "" {
		log.Fatalf("FATAL: CAP_SERVER_URL not set in .env! Cap server will not work.")
	}
	log.Printf("CAP_SERVER_URL loaded from .env: %s", capServerURL)

	initCORS(envVars["CORS_ALLOWED_ORIGINS"])
	if corsAllowAll {
		log.Printf("CORS: 放行所有来源 (CORS_ALLOWED_ORIGINS=%s)", corsRawOrigins)
	} else {
		log.Printf("CORS: 白名单 %v", corsOriginList)
	}


	// load or create jwt secret
	secretPath := filepath.Join(baseDir, "jwt.secret")
	if b, err := os.ReadFile(secretPath); err == nil && len(b) > 0 {
		jwtSecret = b
	} else {
		jwtSecret = []byte("dev-secret-please-change")
		_ = os.WriteFile(secretPath, jwtSecret, 0600)
		log.Printf("generated jwt secret to %s (change in production)", secretPath)
	}

	dbPath := filepath.Join(baseDir, dbFile)

	// SQLite 并发参数：
	//   busy_timeout —— 遇到锁时等待而不是立刻失败（否则并发请求会随机报 "db error"）
	//   journal_mode(WAL) —— 允许读写并发，写入不再阻塞读取
	// 另外把连接池限制为 1，彻底串行化访问。本服务并发量很低，
	// 串行化的代价可以忽略，却能根除 SQLITE_BUSY。
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	globalDB = db
	defer db.Close()

	if err := migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// Start SSH server (non-blocking)
	StartSSHServer(baseDir)

	// Start WS client to JS bot launcher (non-blocking)
	InitWSClient()
	log.Printf("[WS] client connecting to JS bot launcher at %s", getJSNodeURL(""))

	// Global auto-restore: every 5 minutes, send restore command for all bots with auto_restore enabled
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			// 只对已完成归属验证、且不是用户主动下线的机器人做自动恢复，
			// 避免未验证机器人被此任务牵扯，也避免对已经关掉的机器人白发指令
			rows, err := db.Query("SELECT username FROM bots WHERE auto_restore = 1 AND status = 'confirmed' AND COALESCE(manual_stop,0) = 0")
			if err != nil {
				continue
			}
			var names []string
			for rows.Next() {
				var name string
				rows.Scan(&name)
				names = append(names, name)
			}
			rows.Close()
			if len(names) == 0 {
				continue
			}
			log.Printf("[AUTO-RESTORE] sending restore command to %d bot(s)", len(names))
			for _, name := range names {
				targetURL := getJSNodeURL("/ws/api/sendinfo")
				sc, _, serr := jsDialer.Dial(targetURL, nil)
				if serr != nil {
					continue
				}
				sc.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, _, _ = sc.ReadMessage()
				sc.SetReadDeadline(time.Time{})
				sreq, _ := json.Marshal(map[string]interface{}{
					"botname": name,
					"data":    []map[string]string{{"command": "u restore confirm"}},
				})
				sc.WriteMessage(websocket.TextMessage, sreq)
				sc.Close()
			}
		}
	}()

	// Global auto-reconnect: 常驻巡护，把意外掉线的机器人重新拉起来。
	//
	// 原先 auto_reconnect 的实现寄生在 /ws/api/connectbot 处理器里，靠读取
	// 浏览器那条 JS 连接上的 bot_offline 事件触发。后果是这个功能只在
	// 「有人开着控制台页面」时才有效：控制台一关，连接断开、协程退出，
	// 机器人掉线后再也没有人管它 —— 而「自动重连」本来应该是常驻能力。
	go autoReconnectLoop(db, envVars["AUTO_RECONNECT_INTERVAL"])

	// 常驻订阅 JS 节点的全局事件流，把位置/维度/服务器/邦国信息落库并
	// 推给浏览器。前端只开一条 /ws/api/stream，切页面不断开。
	go jsEventsLoop(db)


	mux := http.NewServeMux()

	// --- Logging middleware ---
	loggedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// --- CORS: 必须在业务处理前完成，且 OPTIONS 预检必须短路 ---
		origin := r.Header.Get("Origin")
		if corsOriginAllowed(origin) {
			if corsAllowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				// 回显具体来源，兼容后续可能开启的凭证模式
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With, Accept, Origin")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Type, Content-Length, X-Squaremap-Base")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			// 预检请求直接返回，避免被各 handler 的 method 检查判成 405
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Read body for POST/PUT (limit to 4KB)
		var bodyDump string
		if r.Method == "POST" || r.Method == "PUT" {
			if r.Body != nil {
				bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				bodyDump = string(bodyBytes)
			}
		}
		// Build log line
		query := r.URL.RawQuery
		logLine := r.Method + " " + r.URL.Path
		if query != "" {
			logLine += "?" + query
		}
		if bodyDump != "" {
			// Truncate long bodies and mask secrets
			disp := bodyDump
			if len(disp) > 200 {
				disp = disp[:200] + "..."
			}
			disp = strings.ReplaceAll(disp, capSecretKey, "***")
			logLine += " | body=" + disp
		}

		// Wrap ResponseWriter to capture status
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: 200}
		mux.ServeHTTP(lrw, r)
		logLine += " | status=" + strconv.Itoa(lrw.statusCode)
		log.Println(logLine)
	})

	// --- POST /api/dev/auth : FanVerify 动态验证码登录 ---
	mux.HandleFunc("/api/dev/auth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := r.ParseMultipartForm(10 << 20); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid form"})
			return
		}

		uidStr := r.FormValue("uid")         // FanVerify: uid (对应原 user_id)
		passCode := r.FormValue("pass_code") // FanVerify: pass_code (对应原 verify_code)

		if uidStr == "" || passCode == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "uid and pass_code required"})
			return
		}

		// Call FanVerify API (GET request with query params)
		apiURL := fmt.Sprintf("%s/openapi/user_verify?accesstoken=%s&uid=%s&pass_code=%s",
			fanverifyAPIBase, fanverifyAccessToken, uidStr, passCode)
		
		resp, err := upstreamHTTPClient.Get(apiURL)
		if err != nil {
			log.Printf("fanverify api error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "upstream request failed: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		// Parse FanVerify response
		var fanverifyResp struct {
			Status string `json:"status"`
			Error  string `json:"error"` // 401 时返回
			Data   []struct {
				UID     int64  `json:"uid"`
				RegTime string `json:"reg_time"`
				Level   string `json:"level"` // FanVerify 返回 string
				Tag     string `json:"tag"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBody, &fanverifyResp); err != nil {
			log.Printf("fanverify response parse error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "bad upstream response"})
			return
		}

		// Check for auth failure
		if resp.StatusCode == 401 || fanverifyResp.Status != "ok" || len(fanverifyResp.Data) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 401,
				"msg":  "Authentication failed: " + fanverifyResp.Error,
			})
			return
		}

		userInfo := fanverifyResp.Data[0]

		// Convert level from string to int64
		levelInt, err := strconv.ParseInt(userInfo.Level, 10, 64)
		if err != nil {
			levelInt = 1 // fallback
		}

		// Auth success — find or create user in our DB
		u, err := findUser(db, uidStr)
		if err != nil {
			http.Error(w, `{"code":500,"msg":"db error"}`, http.StatusInternalServerError)
			return
		}
		if u == nil {
			u = &UserData{
				JhtUID:                       uidStr,
				RemainingBotCreationQuantity: 1,
				LevelID:                      10001,
				FanverifyUID:                 userInfo.UID,
				CreateTime:                   userInfo.RegTime,
				Level:                        levelInt,
				Tag:                          userInfo.Tag,
			}
		} else {
			u.FanverifyUID = userInfo.UID
			u.CreateTime = userInfo.RegTime
			u.Level = levelInt
			u.Tag = userInfo.Tag
		}

		// Issue JWT
		now := time.Now()
		exp := now.Add(tokenValidity)
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"jht_uid":        u.JhtUID,
			"remaining_bots": u.RemainingBotCreationQuantity,
			"level":          u.LevelID,
			"fan_uid":        userInfo.UID,
			"fan_lv":         levelInt,
			"tag":            userInfo.Tag,
			"exp":            exp.Unix(),
			"iat":            now.Unix(),
		})
		tokStr, err := token.SignedString(jwtSecret)
		if err != nil {
			http.Error(w, `{"code":500,"msg":"token error"}`, http.StatusInternalServerError)
			return
		}
		u.AccessToken = tokStr

		if err := upsertUser(db, u); err != nil {
			http.Error(w, `{"code":500,"msg":"db save error"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200,
			"msg":  "Authentication successful",
			"user_info": map[string]interface{}{
				"fanverify_uid": userInfo.UID,
				"create_time":   userInfo.RegTime,
				"level":         levelInt,
				"tag":           userInfo.Tag,
			},
			"accesstoken": tokStr,
			"expires_at":  exp.Unix(),
		})
	})

	// --- POST /api/login : 前端登录 (验证码/二维码轮询) ---
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// ────────── GET: 二维码轮询 (session_token) ──────────
		if r.Method == http.MethodGet && r.URL.Query().Get("session_token") != "" {
			sessionToken := r.URL.Query().Get("session_token")
			val, ok := otpSessions.Load(sessionToken)
			if !ok {
				log.Printf("[SESSION] poll token=%s... not found", sessionToken[:min(16, len(sessionToken))])
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "expired"})
				return
			}
			sess := val.(*otpSession)
			if time.Now().After(sess.ExpiresAt) {
				log.Printf("[SESSION] poll token=%s... expired (otp=%s)", sessionToken[:16], sess.OtpID)
				otpSessions.Delete(sessionToken)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "expired"})
				return
			}
			// Already verified in a previous poll? Return cached token.
			if sess.Status == "ok" {
				log.Printf("[SESSION] poll token=%s... cached VERIFIED", sessionToken[:16])
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":       "ok",
					"access_token": sess.Token,
				})
				otpSessions.Delete(sessionToken)
				return
			}

			// 调用 FanVerify OTP 轮询接口 (旧版 simpass 接口已下线)
			otpQueryURL := fmt.Sprintf("%s/openapi/seeotp?accesstoken=%s&otp=%s",
				fanverifyAPIBase, url.QueryEscape(fanverifyAccessToken), url.QueryEscape(sess.OtpID))
			log.Printf("[SESSION] poll token=%s... querying fanverify", sessionToken[:16])
			otpResp, err := upstreamHTTPClient.Get(otpQueryURL)
			if err != nil {
				log.Printf("[SESSION] fanverify otp query error: %v", err)
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "wait"})
				return
			}
			defer otpResp.Body.Close()
			otpBody, _ := io.ReadAll(otpResp.Body)

			var otpStatus struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Data   []struct {
					UID     int64  `json:"uid"`
					RegTime string `json:"reg_time"`
					Level   string `json:"level"`
					Tag     string `json:"tag"`
				} `json:"data"`
			}
			json.Unmarshal(otpBody, &otpStatus)

			// 429 rate_limit: FanVerify 要求同一 OTP 至少间隔 5 秒才能再次查询，
			// 属正常节流，按"等待中"处理即可，不应当作错误。
			if otpResp.StatusCode == http.StatusTooManyRequests || otpStatus.Status == "rate_limit" {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "wait"})
				return
			}

			if otpStatus.Status == "ok" && len(otpStatus.Data) > 0 {
				userInfo := otpStatus.Data[0]
				log.Printf("[SESSION] poll token=%s... fanverify VERIFIED! uid=%d", sessionToken[:16], userInfo.UID)

				levelInt, perr := strconv.ParseInt(userInfo.Level, 10, 64)
				if perr != nil {
					levelInt = 1
				}
				uidStr := strconv.FormatInt(userInfo.UID, 10)

				u, _ := findUser(db, uidStr)
				if u == nil {
					u = &UserData{
						JhtUID:                       uidStr,
						LevelID:                      10001,
						RemainingBotCreationQuantity: 1,
						FanverifyUID:                 userInfo.UID,
						CreateTime:                   userInfo.RegTime,
						Level:                        levelInt,
						Tag:                          userInfo.Tag,
					}
				} else {
					u.FanverifyUID = userInfo.UID
					u.CreateTime = userInfo.RegTime
					u.Level = levelInt
					u.Tag = userInfo.Tag
				}

				// Issue JWT
				now := time.Now()
				exp := now.Add(tokenValidity)
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
					"jht_uid": u.JhtUID, "level": u.LevelID,
					"fan_uid": userInfo.UID,
					"fan_lv":  levelInt,
					"tag":     userInfo.Tag,
					"exp":     exp.Unix(), "iat": now.Unix(),
				})
				tokStr, _ := token.SignedString(jwtSecret)
				u.AccessToken = tokStr
				upsertUser(db, u)

				// Cache in session for subsequent polls, then return
				sess.Status = "ok"
				sess.Token = tokStr
				sess.FanverifyUID = userInfo.UID
				sess.CreateTime = userInfo.RegTime
				sess.Level = levelInt
				sess.Tag = userInfo.Tag

				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":       "ok",
					"access_token": tokStr,
				})
				otpSessions.Delete(sessionToken)
				return
			}

			log.Printf("[SESSION] poll token=%s... fanverify status=%s", sessionToken[:16], otpStatus.Status)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "wait"})
			return
		}

		// ────────── GET: 生成二维码 (cap_token) ──────────
		if r.Method == http.MethodGet && r.URL.Query().Get("cap_token") != "" {
			capTok := r.URL.Query().Get("cap_token")

			// verify cap token using Cap siteverify API
			capOK, capErr := verifyCapToken(capTok)
			if capErr != nil {
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "人机验证服务暂时不可用，请稍后重试"})
				return
			}
			if !capOK {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "人机验证未通过，请重新完成验证"})
				return
			}

			// 调用 FanVerify OTP 申请接口 (旧版 simpass 接口已下线)
			otpURL := fmt.Sprintf("%s/openapi/otp?accesstoken=%s",
				fanverifyAPIBase, url.QueryEscape(fanverifyAccessToken))
			log.Printf("[OTP] requesting from fanverify: %s/openapi/otp", fanverifyAPIBase)
			otpResp, err := upstreamHTTPClient.Get(otpURL)
			if err != nil {
				log.Printf("[OTP] http error: %v", err)
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "扫码凭据服务连接失败，请稍后重试"})
				return
			}
			defer otpResp.Body.Close()
			otpBody, _ := io.ReadAll(otpResp.Body)
			log.Printf("[OTP] fanverify raw response (http %d): %s", otpResp.StatusCode, string(otpBody))

			var otpData struct {
				Success bool `json:"success"`
				Error   string `json:"error"`
				Data    struct {
					Otp string `json:"otp"`
				} `json:"data"`
			}
			if err := json.Unmarshal(otpBody, &otpData); err != nil {
				log.Printf("[OTP] parse error: %v | body=%s", err, string(otpBody))
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "扫码凭据服务返回异常"})
				return
			}
			if !otpData.Success || otpData.Data.Otp == "" {
				reason := otpData.Error
				if reason == "" {
					reason = "未知错误"
				}
				log.Printf("[OTP] fanverify rejected: %s", reason)
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "扫码登录暂不可用：" + reason})
				return
			}
			otpValue := otpData.Data.Otp
			log.Printf("[OTP] success: otp=%s", otpValue)

			// Generate session token (128 hex chars = 64 bytes)
			sessionToken := generateToken(64)

			// 二维码图片不直接把 FanVerify 的 genqrcode 地址交给前端：
			// 该接口要求把 accesstoken 放在 URL 里，直接下发会把开发者密钥
			// 暴露给每一个访问者。改为经本方 /api/otp/qrcode 代理转发。
			proto := r.Header.Get("X-Forwarded-Proto")
			if proto == "" {
				if r.TLS != nil {
					proto = "https"
				} else {
					proto = "http"
				}
			}
			qrURL := fmt.Sprintf("%s://%s/api/otp/qrcode?session_token=%s", proto, r.Host, sessionToken)

			otpSessions.Store(sessionToken, &otpSession{
				OtpID:     otpValue,
				Status:    "wait",
				ExpiresAt: time.Now().Add(120 * time.Second),
			})

			json.NewEncoder(w).Encode(map[string]interface{}{
				"qr_coder":      qrURL,
				"session_token": sessionToken,
			})
			return
		}

		// ────────── POST: 验证码登录 (user_id + verify_code + cap_token) ──────────
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			UserID     string `json:"user_id"`
			VerifyCode string `json:"verify_code"`
			CapToken   string `json:"cap_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}

		if req.UserID == "" || req.VerifyCode == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "user_id and verify_code required"})
			return
		}

		// Cap human verification
		if req.CapToken == "" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "请先完成人机验证"})
			return
		}
		capOK, capErr := verifyCapToken(req.CapToken)
		if capErr != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "人机验证服务暂时不可用，请稍后重试"})
			return
		}
		if !capOK {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "人机验证未通过，请重新完成验证"})
			return
		}

		// 调用 FanVerify 开放平台校验动态通行码
		// （旧版简幻通 simpass 接口已废弃，继续调用会直接连接失败返回 502）
		apiURL := fmt.Sprintf("%s/openapi/user_verify?accesstoken=%s&uid=%s&pass_code=%s",
			fanverifyAPIBase, fanverifyAccessToken, url.QueryEscape(req.UserID), url.QueryEscape(req.VerifyCode))

		resp, err := upstreamHTTPClient.Get(apiURL)
		if err != nil {
			log.Printf("[AUTH] fanverify request error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "账号验证服务连接失败，请稍后重试"})
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[AUTH] fanverify response for uid=%s: code=%d body=%s", req.UserID, resp.StatusCode, string(respBody))

		var fanverifyResp struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Data   []struct {
				UID     int64  `json:"uid"`
				RegTime string `json:"reg_time"`
				Level   string `json:"level"`
				Tag     string `json:"tag"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBody, &fanverifyResp); err != nil {
			log.Printf("[AUTH] fanverify parse error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "账号验证服务返回异常"})
			return
		}

		if fanverifyResp.Status != "ok" || len(fanverifyResp.Data) == 0 {
			reason := fanverifyResp.Error
			if reason == "" {
				reason = "UID 或动态通行码错误"
			}
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "msg": "验证失败：" + reason})
			return
		}

		userInfo := fanverifyResp.Data[0]
		uidStr := strconv.FormatInt(userInfo.UID, 10)

		levelInt, err := strconv.ParseInt(userInfo.Level, 10, 64)
		if err != nil {
			levelInt = 1
		}

		u, _ := findUser(db, uidStr)
		if u == nil {
			u = &UserData{
				JhtUID:                       uidStr,
				LevelID:                      10001,
				RemainingBotCreationQuantity: 1,
				FanverifyUID:                 userInfo.UID,
				CreateTime:                   userInfo.RegTime,
				Level:                        levelInt,
				Tag:                          userInfo.Tag,
			}
		} else {
			u.FanverifyUID = userInfo.UID
			u.CreateTime = userInfo.RegTime
			u.Level = levelInt
			u.Tag = userInfo.Tag
		}

		now := time.Now()
		exp := now.Add(tokenValidity)
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"jht_uid": u.JhtUID, "level": u.LevelID,
			"fan_uid": userInfo.UID,
			"fan_lv":  levelInt,
			"tag":     userInfo.Tag,
			"exp":     exp.Unix(), "iat": now.Unix(),
		})
		tokStr, _ := token.SignedString(jwtSecret)
		u.AccessToken = tokStr
		upsertUser(db, u)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200, "msg": "Authentication successful",
			"user_info": map[string]interface{}{
				"fanverify_uid": userInfo.UID,
				"create_time":   userInfo.RegTime,
				"level":         levelInt,
				"tag":           userInfo.Tag,
			},
			"accesstoken": tokStr, "expires_at": exp.Unix(),
		})
	})

	// --- POST /api/userdata : 获取用户数据 ---
	mux.HandleFunc("/api/userdata", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token required"})
			return
		}

		// Parse & validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "access_token error, invalid or expired"})
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "access_token error, invalid or expired"})
			return
		}

		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "access_token error, invalid or expired"})
			return
		}

		// Check ban list (in memory for now)
		if _, banned := bannedUsers.Load(jhtUID); banned {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "you are banned this server:attack server"})
			return
		}

		u, err := findUser(db, jhtUID)
		if err != nil || u == nil {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "user not found"})
			return
		}

		// Update last login time
		u.LastLoginTime = time.Now().Format("2006-01-02 15:04:05")
		upsertUser(db, u)

		// Real bot count
		botCount, _ := countBotsByUser(db, jhtUID)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":            "200",
			"user_id":         u.JhtUID,
			"level_uid":       u.LevelID,
			"last_login_time": u.LastLoginTime,
			"reg_time":        u.CreateTime,
			"bots":            botCount,
		})
	})

	// --- POST /api/createbot : 创建机器人 ---
	mux.HandleFunc("/api/createbot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"bot_name"`
			Username    string `json:"username"` // 字段别名，兼容前端既有写法
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.BotName == "" {
			req.BotName = strings.TrimSpace(req.Username)
		}
		if req.AccessToken == "" || req.BotName == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token and bot_name required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check remaining creation quota
		user, _ := findUser(db, jhtUID)
		if user == nil || user.RemainingBotCreationQuantity <= 0 {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    403,
				"message": "你没有创建bot的次数了，如需创建更多bot请向管理员提交申请！",
			})
			return
		}

		// Check if bot username already exists
		existing, err := findBotByUsername(db, req.BotName)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "msg": "db error"})
			return
		}

		if existing != nil && existing.Status == "confirmed" {
			// 409: username already registered by someone else and confirmed
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    409,
				"message": "此bot用户名已被其他用户注册并通过认证，请确认账户所有权，必要时请联系管理员！",
			})
			return
		}

		if existing != nil && existing.Status == "no" {
			// Reassign to new owner
			if err := updateBotOwner(db, req.BotName, jhtUID); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "msg": "db error"})
				return
			}
		} else {
			// Create new bot
			bot := &BotData{
				Belong:       jhtUID,
				CreationTime: time.Now().Format("2006-01-02 15:04:05"),
				Username:     req.BotName,
				DSL:          false,
				Status:       "no",
				AutoRestore:  true,
			}
			if err := createBot(db, bot); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "msg": "db error"})
				return
			}
		}

		// Decrement remaining creation quota
		user.RemainingBotCreationQuantity--
		upsertUser(db, user)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":     "200",
			"status":   "To be confirmed",
			"bot_name": req.BotName,
			"message":  "机器人创建成功，但需要验证用户所有权后才能正常使用",
		})
	})

	// --- POST /api/getmybotslist : 获取我的机器人列表 ---
	mux.HandleFunc("/api/getmybotslist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		bots, err := getBotsByUser(db, jhtUID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "msg": "db error"})
			return
		}

		type botItem struct {
			BotName        string `json:"bot_name"`
			CreateTime     string `json:"create_time"`
			DSL            bool   `json:"DSL"`
			Status         string `json:"status"`
			AutoRestore    bool   `json:"auto_restore"`
			AutoReconnect  bool   `json:"auto_reconnect"`
			// 实时状态缓存（位置/维度/所在服务器/邦国信息）
			StatusJSON     string `json:"status_json"`
			StatusTime     string `json:"status_time"`
			LastExitReason string `json:"last_exit_reason"`
			LastExitType   string `json:"last_exit_type"`
			LastExitTime   string `json:"last_exit_time"`
		}
		var items []botItem
		for _, b := range bots {
			items = append(items, botItem{
				BotName:        b.Username,
				CreateTime:     b.CreationTime,
				DSL:            b.DSL,
				Status:         b.Status,
				AutoRestore:    b.AutoRestore,
				AutoReconnect:  b.AutoReconnect,
				LastExitReason: b.LastExitReason,
				LastExitType:   b.LastExitType,
				LastExitTime:   b.LastExitTime,
				StatusJSON:     mustBotStatusJSON(b.Username),
				StatusTime:     mustBotStatusTime(b.Username),
			})
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": "200",
			"bots": len(items),
			"data": items,
		})
	})

	// --- POST /api/verifybot : 验证机器人 ---
	mux.HandleFunc("/api/verifybot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"bot_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token and bot_name required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// 归属校验：只能验证属于自己的机器人
		// 否则任何人都能启动/确认别人的机器人（配合 createbot 的重新认领
		// 机制，还会出现「A 的验证努力帮 B 坐实归属」的劫持）
		vbot, verr := findBotByUsername(globalDB, req.BotName)
		if verr != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "message": "db error"})
			return
		}
		if vbot == nil || vbot.Belong != jhtUID {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权验证"})
			return
		}

		// Start bot via WS and monitor events
		client := InitWSClient()
		chat, uid, err := client.StartBotAndDetect(req.BotName, 120*time.Second)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "启动或监控失败: " + err.Error()})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":            "200",
			"bot_simpass_uid": uid,
			"server":          "auth",
			"chat":            chat,
		})
	})

	// --- POST /api/verifycode : 提交验证码 ---
	mux.HandleFunc("/api/verifycode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"bot_name"`
			Code        string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.Code == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "code required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// 归属校验：只能确认属于自己的机器人
		// 关键：验证可能持续 120 秒，期间机器人有可能被他人重新认领，
		// 若不校验，A 提交验证码会把他人的机器人改成 confirmed
		vbot, verr := findBotByUsername(globalDB, req.BotName)
		if verr != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "message": "db error"})
			return
		}
		if vbot == nil || vbot.Belong != jhtUID {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "message": "此机器人已不属于你（可能已被其他用户重新认领），请回到列表重新确认归属"})
			return
		}

		// Send code to bot via WS and monitor result
		client := InitWSClient()
		chat, err := client.SendCommandAndDetect(req.BotName, req.Code, 120*time.Second)
		if err != nil {
			errMsg := err.Error()
			if errMsg == "verify_failed" {
				// Stop the bot immediately on failure
				if stopErr := client.StopBot(req.BotName); stopErr != nil {
					log.Printf("[WS] stopbot error: %v", stopErr)
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "message": "验证失败：ID或验证码错误"})
			} else {
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "发送验证码失败: " + errMsg})
			}
			return
		}

		// Success — "验证成功" found in chat
		setBotDSL(globalDB, jhtUID, req.BotName, false)

		// 带 belong 条件写入，并检查是否真的改中:
		// 验证持续期间机器人可能已被他人重新认领，此时返回 0 行，
		// 绝不能把归属已变更的机器人标成 confirmed
		affected, uerr := updateBotStatus(globalDB, req.BotName, jhtUID, "confirmed")
		if uerr != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "message": "验证通过但保存失败: " + uerr.Error()})
			return
		}
		if affected == 0 {
			log.Printf("[verify] %s 验证通过，但归属已变更，未标记 confirmed", req.BotName)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    409,
				"message": "游戏内验证已通过，但此机器人刚刚被其他用户重新认领，归属已变更。请回到列表确认当前归属后重新操作。",
				"chat":    chat,
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{"code": "200", "message": "验证成功，机器人已确认归属", "chat": chat})

		// Stop the bot after 5s (async)
		go func() {
			time.Sleep(5 * time.Second)
			if stopErr := client.StopBot(req.BotName); stopErr != nil {
				log.Printf("[WS] stopbot after verify success: %v", stopErr)
				return
			}
			// 验证通过后按设计要把机器人停掉，等用户自己去点「上线」。
			// 这里必须一并打上主动停止标记，否则全局自动重连巡护会发现
			// 「confirmed + 离线 + 未标记」并立刻把它重新拉起来，
			// 机器人会在验证刚通过时自己上线，与设计意图相反。
			if merr := setBotManualStop(globalDB, req.BotName, true); merr != nil {
				log.Printf("[verify] 记录验证后停止标记失败 %s: %v", req.BotName, merr)
			}
		}()
	})

	// --- POST /api/getbotstatus : 查询机器人状态 ---
	// --- API: /api/mapsettings : squaremap 的世界列表 ---
	//
	// 后端只做代理，不做解析。解析（点在多边形等）全部在浏览器里做，
	// 后端 CPU 开销归零。按需懒加载：没人请求就不抓。
	mux.HandleFunc("/api/mapsettings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}
		var req struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		if !checkJWT(extractAccessToken(r, req.AccessToken)) {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		// 把地图基址告诉前端，供它拼「在地图上查看」的链接。
		// 走响应头而不是塞进 JSON，这样正文仍然是 squaremap 的原始内容，
		// 基址也只有一个来源（后端），不会和前端配置漂移。
		w.Header().Set("X-Squaremap-Base", squaremapBase())

		body, err := fetchSquaremapRaw("/tiles/settings.json")
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "地图数据获取失败"})
			return
		}
		w.Write(body)
	})

	// --- API: /api/mapmarkers : 某个世界的原始 markers.json ---
	//
	// 原样转发，不做任何解析。世界名走白名单校验 —— 它会被拼进 URL 路径，
	// 不校验的话 ../ 就能打到 squaremap 的任意路径。
	mux.HandleFunc("/api/mapmarkers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}
		var req struct {
			AccessToken string `json:"access_token"`
			World       string `json:"world"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		if !checkJWT(extractAccessToken(r, req.AccessToken)) {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		if !squaremapWorldRe.MatchString(req.World) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid world"})
			return
		}
		body, err := fetchSquaremapRaw("/tiles/" + req.World + "/markers.json")
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "地图数据获取失败"})
			return
		}
		w.Write(body)
	})

	mux.HandleFunc("/api/getbotstatus", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"botname"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token and botname required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot exists and belongs to user
		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "bad gateway!!!机器人服务异常，请联系管理员!"})
			return
		}
		if bot == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 404, "message": "不存在此机器人，请确认用户名"})
			return
		}
		if bot.Belong != jhtUID {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Query online status from JS launcher
		client := InitWSClient()
		info, err := client.GetBotStatusInfo(req.BotName)
		if err != nil {
			// 节点不可达时不再直接报错，退回数据库缓存的上次退出原因，
			// 至少让前端能显示机器人为什么不在线
			log.Printf("[status] botstatus 查询失败 %s: %v", req.BotName, err)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":             "200",
				"online":           false,
				"DSL":              bot.DSL,
				"auto_restore":     bot.AutoRestore,
				"auto_reconnect":   bot.AutoReconnect,
				"server":           "",
				"last_exit_reason": bot.LastExitReason,
				"last_exit_type":   bot.LastExitType,
				"last_exit_time":   bot.LastExitTime,
				"node_reachable":   false,
			})
			return
		}

		// 从节点拿到了退出原因就落库，保证节点重启/记录丢失后仍可追溯
		if info.LastExitReason != "" && info.LastExitReason != bot.LastExitReason {
			if err := saveBotExitReason(db, req.BotName, info.LastExitReason, info.LastExitType, info.LastExitTime); err != nil {
				log.Printf("[status] 保存退出原因失败 %s: %v", req.BotName, err)
			}
			bot.LastExitReason = info.LastExitReason
			bot.LastExitType = info.LastExitType
			bot.LastExitTime = info.LastExitTime
		}

		server := "main"
		// If online, we could determine server from context; default to "main"
		if !info.Online {
			server = ""
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":             "200",
			"online":           info.Online,
			"DSL":              bot.DSL,
			"auto_restore":     bot.AutoRestore,
			"auto_reconnect":   bot.AutoReconnect,
			"server":           server,
			"last_exit_reason": info.LastExitReason,
			"last_exit_type":   info.LastExitType,
			"last_exit_time":   info.LastExitTime,
			"node_reachable":   true,
		})
	})

	// --- POST /api/updatebotconfig : 更新机器人配置 ---
	mux.HandleFunc("/api/updatebotconfig", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken   string `json:"access_token"`
			BotName       string `json:"botname"`
			AutoReconnect *bool  `json:"auto_reconnect"`
			AutoRestore   *bool  `json:"auto_restore"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token and botname required"})
			return
		}

		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		if req.AutoReconnect != nil {
			if err := setBotAutoReconnect(globalDB, req.BotName, *req.AutoReconnect); err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "msg": "db error"})
				return
			}
			log.Printf("[API] auto_reconnect for %s set to %v", req.BotName, *req.AutoReconnect)
		}
		if req.AutoRestore != nil {
			if err := setBotAutoRestore(globalDB, req.BotName, *req.AutoRestore); err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 500, "msg": "db error"})
				return
			}
			log.Printf("[API] auto_restore for %s set to %v", req.BotName, *req.AutoRestore)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "message": "配置已更新"})
	})

	// --- POST /api/sendinfo : 发送命令到机器人 ---
	mux.HandleFunc("/api/sendinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"bot_name"`
			Data        []struct {
				Chat    string `json:"chat"`
				Command string `json:"command"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token, bot_name and data required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Send via sendinfo WS (目标为内地 JS 节点，自动附带内部鉴权 secret)
		targetURL := getJSNodeURL("/ws/api/sendinfo")
		wsConn, _, err := jsDialer.Dial(targetURL, nil)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "bad gateway!!!机器人服务异常，请联系管理员!"})
			return
		}
		defer wsConn.Close()

		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = wsConn.ReadMessage()
		wsConn.SetReadDeadline(time.Time{})

		sendReq, _ := json.Marshal(map[string]interface{}{
			"botname": req.BotName,
			"data":    req.Data,
		})
		if err := wsConn.WriteMessage(websocket.TextMessage, sendReq); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "发送失败"})
			return
		}

		wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, resp, err := wsConn.ReadMessage()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "未收到确认"})
			return
		}
		var result map[string]interface{}
		json.Unmarshal(resp, &result)
		json.NewEncoder(w).Encode(result)
	})

	// --- POST /api/expand : 网页操控扩地 ---
	//
	// 网页框选区块后调这里，Go 转发给内地 JS 节点的 /ws/api/expand。
	// 鉴权与归属校验跟 /api/sendinfo 一致：必须持有有效 JWT 且机器人属于自己。
	// 进度不从这里回，走 /ws/api/stream（JS 节点 → Go 事件流 → 浏览器）。
	mux.HandleFunc("/api/expand", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 405, "msg": "method not allowed"})
			return
		}

		var req struct {
			AccessToken string  `json:"access_token"`
			BotName     string  `json:"bot_name"`
			Action      string  `json:"action"`   // "start"（默认）或 "stop"
			Chunks      [][]int `json:"chunks"`   // [[cx,cz],...]
			Occupied    [][]int `json:"occupied"` // 服务器已知已占区块，供接壤排序
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "invalid json"})
			return
		}
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "access_token and bot_name required"})
			return
		}

		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		isStop := req.Action == "stop"
		if !isStop && len(req.Chunks) == 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "message": "选区为空，请先在地图上框选区块"})
			return
		}

		wsConn, _, err := jsDialer.Dial(getJSNodeURL("/ws/api/expand"), nil)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "机器人服务异常，请联系管理员"})
			return
		}
		defer wsConn.Close()

		// 握手后 JS 节点先发一条 {ok:true,...}，读掉它再发正文
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = wsConn.ReadMessage()
		wsConn.SetReadDeadline(time.Time{})

		payload := map[string]interface{}{"username": req.BotName}
		if isStop {
			payload["action"] = "stop"
		} else {
			payload["chunks"] = req.Chunks
			payload["occupied"] = req.Occupied
		}
		sendReq, _ := json.Marshal(payload)
		if err := wsConn.WriteMessage(websocket.TextMessage, sendReq); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "扩地指令发送失败"})
			return
		}

		wsConn.SetReadDeadline(time.Now().Add(8 * time.Second))
		_, resp, err := wsConn.ReadMessage()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "未收到扩地队列确认"})
			return
		}
		var result map[string]interface{}
		json.Unmarshal(resp, &result)
		json.NewEncoder(w).Encode(result)
	})

	// --- GET /api/otp/qrcode : 扫码登录二维码代理 ---
	//
	// FanVerify 的 /openapi/genqrcode 要求把 accesstoken 放在 URL 查询参数里。
	// 若把该地址直接下发给前端，等于把开发者密钥公开给所有访问者，
	// 因此这里做一次服务端代理，密钥只保留在后端。
	mux.HandleFunc("/api/otp/qrcode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sessionToken := r.URL.Query().Get("session_token")
		if sessionToken == "" {
			http.Error(w, "session_token required", http.StatusBadRequest)
			return
		}
		val, ok := otpSessions.Load(sessionToken)
		if !ok {
			http.Error(w, "session expired", http.StatusNotFound)
			return
		}
		sess := val.(*otpSession)
		if time.Now().After(sess.ExpiresAt) {
			otpSessions.Delete(sessionToken)
			http.Error(w, "session expired", http.StatusNotFound)
			return
		}

		qcURL := fmt.Sprintf("%s/openapi/genqrcode?accesstoken=%s&otp=%s",
			fanverifyAPIBase, url.QueryEscape(fanverifyAccessToken), url.QueryEscape(sess.OtpID))
		resp, err := upstreamHTTPClient.Get(qcURL)
		if err != nil {
			log.Printf("[QRCODE] upstream error: %v", err)
			http.Error(w, "二维码服务不可用", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			log.Printf("[QRCODE] upstream http %d: %s", resp.StatusCode, string(body))
			http.Error(w, "二维码生成失败", http.StatusBadGateway)
			return
		}

		png, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			http.Error(w, "二维码读取失败", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	})

	// --- GET /api/config : 前端配置 ---
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"cap_site_key": capSiteKey,
			"cap_server":   capServerURL,
		})
	})

	// --- WebSocket: /ws/api/connectbot : 机器人日志流 ---
	var wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// wsKeepAlive sets up ping/pong heartbeat on a WebSocket connection.
	// Returns a stop function to clean up the goroutine.
	wsKeepAlive := func(w *wsWriter) func() {
		conn := w.conn
		const (
			pongWait   = 60 * time.Second
			pingPeriod = 30 * time.Second
		)
		conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})
		stop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(pingPeriod)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					// 必须走串行化写入：与 handler 的写并发会触发
					// gorilla/websocket 的 panic，客户端表现为莫名其妙的 1006
					if err := w.Ping(); err != nil {
						return
					}
				case <-stop:
					return
				}
			}
		}()
		return func() { close(stop) }
	}

	mux.HandleFunc("/ws/api/connectbot", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] upgrade error: %v", err)
			return
		}
		// 所有写操作都经过 wsWriter 串行化，避免与心跳 goroutine 并发写
		bw := newWSWriter(conn)
		// 断开时发一个正常的关闭帧。原先直接 conn.Close() 不发帧，
		// 浏览器只能看到 1006（异常关闭），拿不到任何原因。
		defer bw.CloseWith(websocket.CloseNormalClosure, "")
		stopHeartbeat := wsKeepAlive(bw)
		defer stopHeartbeat()

		// Read auth message
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, authMsg, err := conn.ReadMessage()
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			log.Printf("[WS] connectbot auth read error: %v", err)
			return
		}
		var auth struct {
			AccessToken string `json:"accesstoken"`
			// 别名：其它接口统一用 access_token，这里一并兼容避免踩坑
			AccessTokenAlt string `json:"access_token"`
			BotName        string `json:"botname"`
			BotNameAlt     string `json:"bot_name"`
		}
		if err := json.Unmarshal(authMsg, &auth); err != nil {
			bw.WriteJSON(map[string]interface{}{"code": 400, "message": "invalid json"})
			return
		}
		if auth.AccessToken == "" {
			auth.AccessToken = auth.AccessTokenAlt
		}
		if auth.BotName == "" {
			auth.BotName = auth.BotNameAlt
		}
		auth.AccessToken = extractAccessToken(r, auth.AccessToken)
		if auth.AccessToken == "" || auth.BotName == "" {
			bw.WriteJSON(map[string]interface{}{"code": 400, "message": "accesstoken and botname required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(auth.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			bw.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			bw.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, auth.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			bw.WriteJSON(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Build history file path
		logsDir := filepath.Join(baseDir, "botlogs")
		os.MkdirAll(logsDir, 0755)
		logFile := filepath.Join(logsDir, auth.BotName+".log")

		// 只取最后 100 行作为历史。只读文件末尾，不整读 ——
		// 日志是只追加的，跑几个月后可能上百 MB，整读会在每次连控制台时
		// 造成一次明显的内存尖峰。
		historyLines := tailLogLines(logFile, historyLogLines)
		historyB64 := base64.StdEncoding.EncodeToString([]byte(strings.Join(historyLines, "\n")))

		// Send history to browser
		bw.WriteJSON(map[string]interface{}{
			"code": "200",
			"data": []map[string]string{{"log": historyB64}},
		})

		// Connect to JS launcher botlogs WS
		client := InitWSClient()
		jsConn, err := client.BotLogs(auth.BotName)
		if err != nil {
			// Bot offline — notify browser and keep connection open
			log.Printf("[WS] connectbot botlogs (bot offline): %v", err)
			bw.WriteJSON(map[string]interface{}{
				"code":   "200",
				"online": false,
			})
			// Stay connected until browser disconnects
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		defer jsConn.Close()

		// Forward events from JS to browser + save to log file
		autoReconnect := bot.AutoReconnect
		botName := auth.BotName
		done := make(chan struct{})

		go func() {
			defer close(done)
			for {
				_, msg, err := jsConn.ReadMessage()
				if err != nil {
					return
				}
				// Check for mapdata before forwarding
				var mapEvents []struct {
					BotName string `json:"botname"`
					Data    []struct {
						MapData    string `json:"mapdata"`
						Width      int    `json:"width"`
						BotOffline bool   `json:"bot_offline"`
						Reason     string `json:"reason"`
					} `json:"data"`
				}
				if json.Unmarshal(msg, &mapEvents) == nil {
					for _, ev := range mapEvents {
						for _, d := range ev.Data {
							if d.MapData != "" && d.Width > 0 {
								pngB64 := mapDataToPNG(d.MapData, d.Width)
								if pngB64 != "" {
									bw.WriteJSON(map[string]interface{}{
										"type":    "map_image",
										"botname": ev.BotName,
										"image":   pngB64,
									})
								}
							}
							// Auto-reconnect on bot offline
							if d.BotOffline && autoReconnect && ev.BotName == botName {
								log.Printf("[WS] bot %s offline (reason: %s), auto-reconnect in 10s", ev.BotName, d.Reason)
								go func(name string) {
									time.Sleep(10 * time.Second)
									// 自动重连是一处绕过 HTTP 闸门的直达路径
									// （直接拨 JS 节点），这里重新确认归属状态，
									// 保证未验证的机器人不会被自动拉起
									if fb, ferr := findBotByUsername(globalDB, name); ferr != nil || fb == nil || fb.Status != "confirmed" {
										log.Printf("[WS] auto-reconnect skipped for %s: 归属状态未确认", name)
										return
									}
									// Try to restart: connect to JS startbot WS (内地节点 + 内部鉴权)
									restartURL := getJSNodeURL("/ws/api/startbot")
									rc, _, rerr := jsDialer.Dial(restartURL, nil)
									if rerr != nil {
										log.Printf("[WS] auto-reconnect dial failed for %s: %v", name, rerr)
										return
									}
									defer rc.Close()
									rc.SetReadDeadline(time.Now().Add(3 * time.Second))
									_, _, _ = rc.ReadMessage()
									rc.SetReadDeadline(time.Time{})
									rreq, _ := json.Marshal(map[string]string{"username": name})
									rc.WriteMessage(websocket.TextMessage, rreq)
									rc.SetReadDeadline(time.Now().Add(10 * time.Second))
									_, rresp, rerr := rc.ReadMessage()
									if rerr != nil {
										log.Printf("[WS] auto-reconnect start failed for %s: %v", name, rerr)
										return
									}
									var rresult map[string]interface{}
									json.Unmarshal(rresp, &rresult)
									if code, _ := rresult["code"].(float64); code == 200 {
										log.Printf("[WS] auto-reconnect success for %s", name)
										// Notify browser
										bw.WriteJSON(map[string]interface{}{
											"type": "auto_reconnect",
											"msg":  "机器人已自动重连",
										})
									}
								}(ev.BotName)
							}
						}
					}
				}
				// Forward to browser
				if err := bw.WriteMessage(websocket.TextMessage, msg); err != nil {
					return
				}
				// Save to log file
				var events []struct {
					BotName string `json:"botname"`
					Data    []struct {
						Chat string `json:"chat"`
					} `json:"data"`
				}
				if json.Unmarshal(msg, &events) == nil {
					f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
					if err == nil {
						for _, ev := range events {
							for _, d := range ev.Data {
								f.WriteString(time.Now().Format("2006-01-02 15:04:05") + " " + d.Chat + "\n")
							}
						}
						f.Close()
						// 日志只追加、不轮转的话会无限涨下去，最终把磁盘写满，
						// 而且每次读历史都要回溯一个巨大的文件。超过上限就裁剪，
						// 只保留最近若干行。
						trimLogFile(logFile)
					}
				}
			}
		}()

		// JS 链路断了（机器人掉线、内地节点重启等）。
		// 原先这里直接返回，defer 关连接时不给任何原因，
		// 用户在控制台只看到「通道已断开」，完全不知道发生了什么。
		<-done
		log.Printf("[WS] connectbot %s: JS 节点链路已断开", botName)
		bw.CloseWith(websocket.CloseGoingAway, "机器人数据链路已断开（可能是机器人掉线或内地节点重启），请点「重连通道」")
	})

	// --- WebSocket: /ws/api/startbot : 启动机器人 ---
	mux.HandleFunc("/ws/api/startbot", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] startbot upgrade error: %v", err)
			return
		}
		// 与心跳共用同一个串行化写入器：各自 newWSWriter 的话锁不互斥，
		// 心跳和 handler 的写仍会并发，照样 panic
		bw := newWSWriter(conn)
		defer bw.CloseWith(websocket.CloseNormalClosure, "")
		stopHeartbeat := wsKeepAlive(bw)
		defer stopHeartbeat()

		// Read auth message
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, authMsg, err := conn.ReadMessage()
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			log.Printf("[WS] startbot auth read error: %v", err)
			return
		}
		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"botname"`
		}
		if err := json.Unmarshal(authMsg, &req); err != nil {
			bw.WriteJSON(map[string]interface{}{"code": 400, "message": "invalid json"})
			return
		}
		// 浏览器 WebSocket 无法自定义请求头，令牌通常由前端放在首包里；
		// 同时兼容 Authorization 头与 ?token= 查询参数，方便脚本/调试调用。
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			bw.WriteJSON(map[string]interface{}{"code": 400, "message": "access_token and botname required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			bw.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			bw.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			bw.WriteJSON(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// 归属验证闸门：未完成验证的机器人不允许上线
		if bot.Status != "confirmed" {
			bw.WriteJSON(map[string]interface{}{
				"code":    403,
				"message": "此机器人尚未完成归属验证，请先在机器人列表中点击「验证」完成归属确认",
			})
			return
		}

		// Connect to JS launcher and start the bot (内地节点 + 内部鉴权)
		jsURL := getJSNodeURL("/ws/api/startbot")
		jsConn, _, err := jsDialer.Dial(jsURL, nil)
		if err != nil {
			bw.WriteJSON(map[string]interface{}{"code": 502, "message": "bad gateway!!!无法连接到机器人服务"})
			return
		}
		defer jsConn.Close()

		// Consume welcome
		jsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = jsConn.ReadMessage()
		jsConn.SetReadDeadline(time.Time{})

		// Send start command
		startReq, _ := json.Marshal(map[string]string{"username": req.BotName})
		if err := jsConn.WriteMessage(websocket.TextMessage, startReq); err != nil {
			bw.WriteJSON(map[string]interface{}{"code": 502, "message": "发送启动命令失败"})
			return
		}

		// Read start response from JS
		jsConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, jsResp, err := jsConn.ReadMessage()
		if err != nil {
			bw.WriteJSON(map[string]interface{}{"code": 502, "message": "启动超时，未收到机器人响应"})
			return
		}
		var jsResult map[string]interface{}
		json.Unmarshal(jsResp, &jsResult)

		// Forward JS response to browser
		bw.WriteJSON(jsResult)

		// 用户主动上线：清掉「主动下线」标记，让自动重连巡护重新接管这个机器人。
		// 409 也一并清除 —— 那表示机器人本来就在跑，同样不该被巡护当成下线处理。
		if code, _ := jsResult["code"].(float64); code == 200 || code == 409 {
			if err := setBotManualStop(globalDB, req.BotName, false); err != nil {
				log.Printf("[WS] 清除主动下线标记失败 %s: %v", req.BotName, err)
			}
		}

		// If bot started successfully, forward events to browser in real-time
		if code, _ := jsResult["code"].(float64); code == 200 {
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					_, evtMsg, err := jsConn.ReadMessage()
					if err != nil {
						return
					}
					if err := bw.WriteMessage(websocket.TextMessage, evtMsg); err != nil {
						return
					}
				}
			}()
			<-done
		}
	})

	// --- WebSocket: /ws/api/stream : 全量机器人实时状态流 ---
	//
	// 与 /ws/api/connectbot 的区别：connectbot 是「单个机器人的控制台」，
	// 带历史日志和地图，页面一关就断；stream 是「我名下所有机器人的状态」，
	// 前端全程只开这一条，切页面、离开控制台都不影响数据的持续更新。
	//
	// 只推 status 与 bot_offline —— 聊天和地图仍走 connectbot，避免重复占带宽。
	mux.HandleFunc("/ws/api/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] stream upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// 认证：浏览器 WebSocket 无法自定义请求头，令牌走首包
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, authMsg, err := conn.ReadMessage()
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			return
		}
		var req struct {
			AccessToken    string `json:"access_token"`
			AccessTokenAlt string `json:"accesstoken"`
		}
		if err := json.Unmarshal(authMsg, &req); err != nil {
			conn.WriteJSON(map[string]interface{}{"code": 400, "message": "invalid json"})
			return
		}
		tok := req.AccessToken
		if tok == "" {
			tok = req.AccessTokenAlt
		}
		tok = extractAccessToken(r, tok)
		if tok == "" {
			conn.WriteJSON(map[string]interface{}{"code": 400, "message": "access_token required"})
			return
		}

		token, err := jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// 该用户名下的机器人构成转发白名单。
		// 这是安全边界：别人的机器人状态绝不能流到这条连接上。
		owned, err := getBotsByUser(globalDB, jhtUID)
		if err != nil {
			conn.WriteJSON(map[string]interface{}{"code": 502, "message": "读取机器人列表失败"})
			return
		}
		allowed := make(map[string]bool, len(owned))
		for _, b := range owned {
			allowed[b.Username] = true
		}

		sub := &botStreamSub{
			conn: conn,
			bots: allowed,
			send: make(chan []byte, 128),
			done: make(chan struct{}),
		}
		addBotStreamSub(sub)
		defer removeBotStreamSub(sub)

		// 所有写出都走 channel，由 writeLoop 独占写这条连接。
		// gorilla/websocket 不允许多 goroutine 并发写，直接 conn.WriteJSON
		// 会和 writeLoop 里的心跳 ping 撞车。
		if ack, e := json.Marshal(map[string]interface{}{
			"code": 200, "message": "subscribed", "bots": len(allowed),
		}); e == nil {
			sub.push(ack)
		}
		// 先补一遍缓存状态，页面不必干等下一个采集周期
		for _, b := range owned {
			js, ts := botStatusRow(globalDB, b.Username)
			if js == "" || !json.Valid([]byte(js)) {
				continue
			}
			out, e := json.Marshal(map[string]interface{}{
				"botname": b.Username,
				"data":    []map[string]interface{}{{"status": json.RawMessage(js), "status_time": ts}},
			})
			if e == nil {
				sub.push(out)
			}
		}

		go sub.writeLoop()

		// 读循环只用于感知断开；pong 会延长读超时
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	// --- WebSocket: /ws/api/stopbot : 关闭机器人 ---
	mux.HandleFunc("/ws/api/stopbot", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] stopbot upgrade error: %v", err)
			return
		}
		// 与心跳共用同一个串行化写入器：各自 newWSWriter 的话锁不互斥，
		// 心跳和 handler 的写仍会并发，照样 panic
		bw := newWSWriter(conn)
		defer bw.CloseWith(websocket.CloseNormalClosure, "")
		stopHeartbeat := wsKeepAlive(bw)
		defer stopHeartbeat()

		// Read auth message
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, authMsg, err := conn.ReadMessage()
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			log.Printf("[WS] stopbot auth read error: %v", err)
			return
		}
		var req struct {
			AccessToken string `json:"access_token"`
			BotName     string `json:"botname"`
		}
		if err := json.Unmarshal(authMsg, &req); err != nil {
			bw.WriteJSON(map[string]interface{}{"code": 400, "message": "invalid json"})
			return
		}
		// 浏览器 WebSocket 无法自定义请求头，令牌通常由前端放在首包里；
		// 同时兼容 Authorization 头与 ?token= 查询参数，方便脚本/调试调用。
		req.AccessToken = extractAccessToken(r, req.AccessToken)
		if req.AccessToken == "" || req.BotName == "" {
			bw.WriteJSON(map[string]interface{}{"code": 400, "message": "access_token and botname required"})
			return
		}

		// Validate JWT
		token, err := jwt.Parse(req.AccessToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			bw.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			bw.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			bw.WriteJSON(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Connect to JS launcher stopbot
		client := InitWSClient()
		if err := client.StopBot(req.BotName); err != nil {
			log.Printf("[WS] stopbot error: %v", err)
			bw.WriteJSON(map[string]interface{}{"code": 502, "message": "关闭失败: " + err.Error()})
			return
		}

		// 记下「这是用户主动下线」。全局自动重连巡护会跳过带此标记的机器人，
		// 否则用户刚点完下线就会被巡护立刻拉回来，下线按钮形同虚设。
		if err := setBotManualStop(globalDB, req.BotName, true); err != nil {
			log.Printf("[WS] 记录主动下线标记失败 %s: %v", req.BotName, err)
		}

		bw.WriteJSON(map[string]interface{}{"code": 200, "message": "机器人已断开"})
	})

	// --- Static file serving with config injection ---
	staticDir := filepath.Join(baseDir, "static")
	fileServer := http.FileServer(http.Dir(staticDir))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inject cap endpoint into login.html on the fly
		if r.URL.Path == "/login.html" || r.URL.Path == "/login" {
			loginPath := filepath.Join(staticDir, "login.html")
			if data, err := os.ReadFile(loginPath); err == nil {
				content := strings.ReplaceAll(string(data), "{{CAP_SITE_KEY}}", capSiteKey)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write([]byte(content))
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	}))

	srv := &http.Server{Addr: listenAddr, Handler: loggedMux}
	log.Printf("listening %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

// loggingResponseWriter captures the status code for logging
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker so gorilla/websocket can upgrade connections.
func (lrw *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := lrw.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("loggingResponseWriter: underlying ResponseWriter does not implement http.Hijacker")
}

func migrate(db *sql.DB) error {
	sqlStmt := `CREATE TABLE IF NOT EXISTS userdata (
		jht_uid TEXT PRIMARY KEY,
		accesstoken TEXT,
		bot_name TEXT,
		remaining_bot_creation_quantity INTEGER DEFAULT 1,
		level_id INTEGER,
		fanverify_uid INTEGER,
		create_time TEXT,
		sim_level INTEGER,
		tag TEXT DEFAULT '',
		last_login_time TEXT,
		status TEXT DEFAULT 'ok',
		status_info TEXT DEFAULT ''
	);`
	if _, err := db.Exec(sqlStmt); err != nil {
		return err
	}
	// Add columns if table already existed without them (ignore errors)
	db.Exec("ALTER TABLE userdata ADD COLUMN last_login_time TEXT")
	db.Exec("ALTER TABLE userdata ADD COLUMN status TEXT DEFAULT 'ok'")
	db.Exec("ALTER TABLE userdata ADD COLUMN status_info TEXT DEFAULT ''")
	db.Exec("ALTER TABLE userdata ADD COLUMN remaining_bot_creation_quantity INTEGER DEFAULT 1")
	db.Exec("ALTER TABLE userdata ADD COLUMN tag TEXT DEFAULT ''")  // Add tag column for existing tables

	// Bots table
	botsStmt := `CREATE TABLE IF NOT EXISTS bots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		belong TEXT NOT NULL,
		creation_time TEXT NOT NULL,
		username TEXT NOT NULL,
		dsl INTEGER DEFAULT 0,
		status TEXT DEFAULT 'no',
		auto_restore INTEGER DEFAULT 1,
		auto_reconnect INTEGER DEFAULT 1,
		manual_stop INTEGER DEFAULT 0,
		status_json TEXT DEFAULT '',
		status_time TEXT DEFAULT '',
		last_exit_reason TEXT DEFAULT '',
		last_exit_type TEXT DEFAULT '',
		last_exit_time TEXT DEFAULT ''
	);`
	if _, err := db.Exec(botsStmt); err != nil {
		return err
	}
	db.Exec("ALTER TABLE bots ADD COLUMN status TEXT DEFAULT 'no'")
	db.Exec("ALTER TABLE bots ADD COLUMN auto_restore INTEGER DEFAULT 1")
	db.Exec("ALTER TABLE bots ADD COLUMN auto_reconnect INTEGER DEFAULT 1")
	// manual_stop: 1 表示用户主动下线过，自动重连巡护要放过它
	db.Exec("ALTER TABLE bots ADD COLUMN manual_stop INTEGER DEFAULT 0")
	// status_json: JS 节点上报的实时状态（位置/维度/所在服务器/邦国信息）
	// 落库后即使没人开着控制台、或用户换了页面，卡片也能显示最近一次状态
	db.Exec("ALTER TABLE bots ADD COLUMN status_json TEXT DEFAULT ''")
	db.Exec("ALTER TABLE bots ADD COLUMN status_time TEXT DEFAULT ''")
	// 上次退出原因（老库升级用，列已存在时 ALTER 会报错，忽略即可）
	db.Exec("ALTER TABLE bots ADD COLUMN last_exit_reason TEXT DEFAULT ''")
	db.Exec("ALTER TABLE bots ADD COLUMN last_exit_type TEXT DEFAULT ''")
	db.Exec("ALTER TABLE bots ADD COLUMN last_exit_time TEXT DEFAULT ''")
	return nil
}

// autoReconnectLoop 常驻巡护：周期性检查哪些机器人掉线了，把该拉起来的拉起来。
//
// 设计取舍：
//   - 用轮询 /ws/api/botstatus 而不是给每个机器人挂一条常驻 WS，避免机器
//     人数量增长时堆积 N 条长连接和 N 个协程。
//   - 只处理 auto_reconnect=1、status='confirmed'、manual_stop=0 的机器人。
//     manual_stop 是关键：用户主动点「下线」的机器人必须保持关闭，否则巡护
//     会立刻把它拉回来，下线按钮等于失效。
//   - 连续失败时指数退避（30s→60s→120s→240s），避免对一个坏账号无限猛敲；
//     节点不可达不计入失败次数，那是节点的问题不是机器人的问题。
func autoReconnectLoop(db *sql.DB, intervalEnv string) {
	interval := 30 * time.Second
	if v := strings.TrimSpace(intervalEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 5*time.Second {
			interval = d
		}
	}

	type botState struct {
		lastAttempt time.Time
		fails       int
	}
	states := make(map[string]*botState)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[AUTO-RECONNECT] 巡护已启动，检查间隔 %s", interval)

	for range ticker.C {
		names, err := autoReconnectCandidates(db)
		if err != nil {
			log.Printf("[AUTO-RECONNECT] 查询待巡护机器人失败: %v", err)
			continue
		}
		if len(names) == 0 {
			continue
		}
		client := InitWSClient()
		for _, name := range names {
			st := states[name]
			if st == nil {
				st = &botState{}
				states[name] = st
			}
			// 退避：失败次数越多，重试间隔越长（最多 8 倍）
			backoff := interval
			for i := 0; i < st.fails && i < 3; i++ {
				backoff *= 2
			}
			if !st.lastAttempt.IsZero() && time.Since(st.lastAttempt) < backoff {
				continue
			}

			online, err := client.GetBotStatus(name)
			if err != nil {
				// 节点不可达：跳过，不计失败
				continue
			}
			if online {
				st.fails = 0
				continue
			}

			st.lastAttempt = time.Now()
			log.Printf("[AUTO-RECONNECT] %s 已掉线，尝试重新拉起", name)
			if err := client.StartBotSimple(name); err != nil {
				st.fails++
				log.Printf("[AUTO-RECONNECT] %s 拉起失败（连续第 %d 次）: %v", name, st.fails, err)
				continue
			}
			st.fails = 0
			log.Printf("[AUTO-RECONNECT] %s 已重新拉起", name)
		}
	}
}

func findUser(db *sql.DB, uid string) (*UserData, error) {
	row := db.QueryRow("SELECT jht_uid, accesstoken, COALESCE(remaining_bot_creation_quantity,1), level_id, fanverify_uid, create_time, sim_level, COALESCE(tag,''), COALESCE(last_login_time,''), COALESCE(status,'ok'), COALESCE(status_info,'') FROM userdata WHERE jht_uid = ?", uid)
	var u UserData
	err := row.Scan(&u.JhtUID, &u.AccessToken, &u.RemainingBotCreationQuantity, &u.LevelID, &u.FanverifyUID, &u.CreateTime, &u.Level, &u.Tag, &u.LastLoginTime, &u.Status, &u.StatusInfo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func upsertUser(db *sql.DB, u *UserData) error {
	_, err := db.Exec(`INSERT INTO userdata(jht_uid, accesstoken, remaining_bot_creation_quantity, level_id, fanverify_uid, create_time, sim_level, tag, last_login_time, status, status_info)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(jht_uid) DO UPDATE SET
			accesstoken=excluded.accesstoken,
			remaining_bot_creation_quantity=excluded.remaining_bot_creation_quantity,
			level_id=excluded.level_id,
			fanverify_uid=excluded.fanverify_uid,
			create_time=excluded.create_time,
			sim_level=excluded.sim_level,
			tag=excluded.tag,
			last_login_time=excluded.last_login_time,
			status=excluded.status,
			status_info=excluded.status_info;`,
		u.JhtUID, u.AccessToken, u.RemainingBotCreationQuantity, u.LevelID, u.FanverifyUID, u.CreateTime, u.Level, u.Tag, u.LastLoginTime, u.Status, u.StatusInfo)
	return err
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && s != "" && sub != "" && len(s)-len(sub) >= 0 && containsStr(s, sub)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func extractFanverifyUID(chat string) string {
	// Try to extract UID from patterns like "UID：XXXXXX", "UID: XXXXXX" or "UID是：XXXXXX"
	for _, prefix := range []string{"UID是：", "UID是:", "UID：", "UID:", "uid是：", "uid是:", "uid：", "uid:"} {
		idx := strings.Index(chat, prefix)
		if idx >= 0 {
			rest := chat[idx+len(prefix):]
			// Trim leading spaces/colons
			rest = strings.TrimLeft(rest, " :：")
			end := strings.IndexAny(rest, " \n\r")
			if end > 0 {
				return rest[:end]
			}
			return rest
		}
	}
	return ""
}

// Minecraft map color palette (post-1.16), indexed by color ID (0-255).
// Source: Minecraft's MapColor.COLORS array.
var mapColors = []color.RGBA{
	{0, 0, 0, 0},         // 0
	{127, 178, 56, 255},  // 1
	{247, 233, 163, 255}, // 2
	{199, 199, 199, 255}, // 3
	{255, 255, 255, 255}, // 4
	{160, 160, 255, 255}, // 5
	{167, 167, 167, 255}, // 6
	{0, 124, 0, 255},     // 7
	{255, 255, 255, 255}, // 8
	{164, 168, 184, 255}, // 9
	{151, 109, 77, 255},  // 10
	{112, 112, 112, 255}, // 11
	{64, 64, 255, 255},   // 12
	{143, 119, 72, 255},  // 13
	{255, 252, 245, 255}, // 14
	{216, 127, 51, 255},  // 15
	{178, 76, 216, 255},  // 16
	{102, 153, 216, 255}, // 17
	{229, 229, 51, 255},  // 18
	{127, 204, 25, 255},  // 19
	{242, 127, 165, 255}, // 20
	{76, 76, 76, 255},    // 21
	{153, 153, 153, 255}, // 22
	{76, 127, 153, 255},  // 23
	{127, 63, 178, 255},  // 24
	{51, 76, 178, 255},   // 25
	{102, 76, 51, 255},   // 26
	{102, 127, 51, 255},  // 27
	{153, 51, 51, 255},   // 28
	{25, 25, 25, 255},    // 29
	{250, 238, 77, 255},  // 30
	{92, 219, 213, 255},  // 31
	{74, 128, 255, 255},  // 32
	{0, 217, 58, 255},    // 33
	{129, 86, 49, 255},   // 34
	{112, 2, 0, 255},     // 35
	{209, 177, 161, 255}, // 36
	{159, 144, 104, 255}, // 37
	{104, 96, 80, 255},   // 38
	{255, 255, 255, 255}, // 39
	{255, 255, 255, 255}, // 40
	{255, 255, 255, 255}, // 41
	{255, 255, 255, 255}, // 42
	{255, 255, 255, 255}, // 43
	{255, 255, 255, 255}, // 44
	{255, 255, 255, 255}, // 45
	{255, 255, 255, 255}, // 46
	{255, 255, 255, 255}, // 47
	{255, 255, 255, 255}, // 48
	{255, 255, 255, 255}, // 49
	{255, 255, 255, 255}, // 50
	{255, 255, 255, 255}, // 51
	{255, 255, 255, 255}, // 52
	{255, 255, 255, 255}, // 53
	{255, 255, 255, 255}, // 54
	{255, 255, 255, 255}, // 55
	{255, 255, 255, 255}, // 56
	{255, 255, 255, 255}, // 57
	{255, 255, 255, 255}, // 58
	{255, 255, 255, 255}, // 59
	{255, 255, 255, 255}, // 60
	{255, 255, 255, 255}, // 61
	{255, 255, 255, 255}, // 62
	{255, 255, 255, 255}, // 63
	// Indices 64-143 from Minecraft's full palette (biome colors, etc.)
	{0, 0, 0, 0},         // 64
	{127, 178, 56, 255},  // 65
	{247, 233, 163, 255}, // 66
	{167, 167, 167, 255}, // 67
	{255, 255, 255, 255}, // 68
	{160, 160, 255, 255}, // 69
	{167, 167, 167, 255}, // 70
	{0, 124, 0, 255},     // 71
	{255, 255, 255, 255}, // 72
	{164, 168, 184, 255}, // 73
	{151, 109, 77, 255},  // 74
	{112, 112, 112, 255}, // 75
	{64, 64, 255, 255},   // 76
	{143, 119, 72, 255},  // 77
	{255, 252, 245, 255}, // 78
	{216, 127, 51, 255},  // 79
	{178, 76, 216, 255},  // 80
	{102, 153, 216, 255}, // 81
	{229, 229, 51, 255},  // 82
	{127, 204, 25, 255},  // 83
	{242, 127, 165, 255}, // 84
	{76, 76, 76, 255},    // 85
	{153, 153, 153, 255}, // 86
	{76, 127, 153, 255},  // 87
	{127, 63, 178, 255},  // 88
	{51, 76, 178, 255},   // 89
	{102, 76, 51, 255},   // 90
	{102, 127, 51, 255},  // 91
	{153, 51, 51, 255},   // 92
	{25, 25, 25, 255},    // 93
	{250, 238, 77, 255},  // 94
	{92, 219, 213, 255},  // 95
	{74, 128, 255, 255},  // 96
	{0, 217, 58, 255},    // 97
	{129, 86, 49, 255},   // 98
	{112, 2, 0, 255},     // 99
	{209, 177, 161, 255}, // 100
	{159, 144, 104, 255}, // 101
	{104, 96, 80, 255},   // 102
	{255, 255, 255, 255}, // 103
	{255, 255, 255, 255}, // 104
	{255, 255, 255, 255}, // 105
	{255, 255, 255, 255}, // 106
	{255, 255, 255, 255}, // 107
	{255, 255, 255, 255}, // 108
	{255, 255, 255, 255}, // 109
	{255, 255, 255, 255}, // 110
	{255, 255, 255, 255}, // 111
	{255, 255, 255, 255}, // 112
	{255, 255, 255, 255}, // 113
	{255, 255, 255, 255}, // 114
	{255, 255, 255, 255}, // 115
	{255, 255, 255, 255}, // 116
	{255, 255, 255, 255}, // 117
	{255, 255, 255, 255}, // 118
	{255, 255, 255, 255}, // 119
	{255, 255, 255, 255}, // 120
	{255, 255, 255, 255}, // 121
	{255, 255, 255, 255}, // 122
	{255, 255, 255, 255}, // 123
	{255, 255, 255, 255}, // 124
	{255, 255, 255, 255}, // 125
	{255, 255, 255, 255}, // 126
	{255, 255, 255, 255}, // 127
	// Extended biome colors (indices 128-255)
	{0, 0, 0, 0}, // 128+
	{127, 178, 56, 255},
	{247, 233, 163, 255},
	{199, 199, 199, 255},
	{255, 255, 255, 255},
	{160, 160, 255, 255},
	{167, 167, 167, 255},
	{0, 124, 0, 255},
	{255, 255, 255, 255},
	{164, 168, 184, 255},
	{151, 109, 77, 255},
	{112, 112, 112, 255},
	{64, 64, 255, 255},
	{143, 119, 72, 255},
	{255, 252, 245, 255},
	{216, 127, 51, 255},
	{178, 76, 216, 255},
	{102, 153, 216, 255},
	{229, 229, 51, 255},
	{127, 204, 25, 255},
	{242, 127, 165, 255},
	{76, 76, 76, 255},
	{153, 153, 153, 255},
	{76, 127, 153, 255},
	{127, 63, 178, 255},
	{51, 76, 178, 255},
	{102, 76, 51, 255},
	{102, 127, 51, 255},
	{153, 51, 51, 255},
	{25, 25, 25, 255},
	{250, 238, 77, 255},
	{92, 219, 213, 255},
	{74, 128, 255, 255},
	{0, 217, 58, 255},
	{129, 86, 49, 255},
	{112, 2, 0, 255},
	{209, 177, 161, 255},
	{159, 144, 104, 255},
	{104, 96, 80, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{255, 255, 255, 255},
	{0, 0, 0, 0},
	{0, 0, 0, 0},
	{0, 0, 0, 0},
	{0, 0, 0, 0},
}

// getMapColor returns the RGBA color for a Minecraft map color ID (0-255).
// Out-of-range IDs return transparent black.
func getMapColor(id int) color.RGBA {
	if id >= 0 && id < len(mapColors) {
		return mapColors[id]
	}
	return color.RGBA{0, 0, 0, 0}
}

// mapDataToPNG converts Minecraft map pixel data (base64) to a PNG image
// encoded as base64. width is the pixel dimension (128 or 256).
func mapDataToPNG(b64Data string, width int) string {
	raw, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil || len(raw) == 0 {
		return ""
	}

	height := len(raw) / width
	if height == 0 {
		return ""
	}

	// Create RGBA image at 4x scale for better visibility
	scale := 4
	img := image.NewRGBA(image.Rect(0, 0, width*scale, height*scale))

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			if idx >= len(raw) {
				continue
			}
			c := getMapColor(int(raw[idx]))
			// Fill a scale×scale block with the same color
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetRGBA(x*scale+dx, y*scale+dy, c)
				}
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}


// ════════════════════════════════════════════════════════════════
// 控制台历史日志
//
// 上限必须前后端都有：后端限制「发多少」，前端限制「留多少」。
// 只做一边的话，任何一条路径漏了都会把浏览器 DOM 撑爆。
// ════════════════════════════════════════════════════════════════

const (
	// 连上控制台时最多回放多少条历史
	historyLogLines = 100
	// 日志文件超过这个大小就裁剪
	maxLogFileBytes = 1 << 20 // 1 MiB
	// 裁剪后保留多少行
	maxLogFileLines = 5000
	// tailLogLines 单次最多读多少字节。
	// 这个值必须 >= maxLogFileBytes，否则裁剪时读不全 5000 行，
	// 会出现「裁完还是那么大」的情况。
	tailMaxRead = 1 << 20 // 1 MiB
)

// tailLogLines 读文件末尾的最后 n 行，不把整个文件读进内存。
// 读取量本身也有上限，避免一个异常大的旧文件把内存吃掉。
func tailLogLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	start := int64(0)
	truncated := false
	if size > tailMaxRead {
		start = size - tailMaxRead
		truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, size-start)
	if _, err := io.ReadFull(f, buf); err != nil && err != io.ErrUnexpectedEOF {
		return nil
	}

	lines := strings.Split(string(buf), "\n")
	// 从中间截断的话第一行是半行，丢掉
	if truncated && len(lines) > 0 {
		lines = lines[1:]
	}
	// 文件末尾通常有个换行，Split 会多出一个空串。不去掉的话
	// 「取最后 100 行」实际只会拿到 99 行。
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// trimLogFile 在日志文件超过 maxLogFileBytes 时只保留最近 maxLogFileLines 行。
// 用「写临时文件 + rename」而不是原地截断，避免中途失败留下半个文件。
func trimLogFile(path string) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= maxLogFileBytes {
		return
	}
	lines := tailLogLines(path, maxLogFileLines)
	if len(lines) == 0 {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}
