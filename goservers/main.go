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
	"mime/multipart"
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
	listenAddr     = ":8888"
	dbFile         = "data.sqlite"
	tokenValidity  = 7 * 24 * time.Hour
	simpassAPIBase = "https://pass.simpfun.cn"
	defaultDevUUID = "48cdd5e7-7c12-45e2-8699-5bc6ed5cdf63"
)

type UserData struct {
	JhtUID                       string `json:"jht_uid"`
	AccessToken                  string `json:"accesstoken"`
	RemainingBotCreationQuantity int64  `json:"remaining_bot_creation_quantity"`
	LevelID                      int64  `json:"level_id"`
	SimpassUID                   int64  `json:"simpass_uid"`
	CreateTime                   string `json:"create_time"`
	Level                        int64  `json:"level"`
	Risky                        bool   `json:"risky"`
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
			env[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return env
}

var jwtSecret []byte
var devUUID string
var capSecret string
var capAPIEndpoint string
var capSiteverifyURL string

// OTP session store (in-memory)
type otpSession struct {
	OtpID      string    `json:"otp_id"`
	Status     string    `json:"status"` // "wait" | "ok"
	ExpiresAt  time.Time `json:"expires_at"`
	SimpassUID int64     `json:"simpass_uid,omitempty"`
	CreateTime string    `json:"create_time,omitempty"`
	Level      int64     `json:"level,omitempty"`
	Risky      bool      `json:"risky,omitempty"`
	Token      string    `json:"token,omitempty"`
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
	devUUID = envVars["DEV_UUID"]
	if devUUID == "" {
		devUUID = defaultDevUUID
		log.Printf("using default dev UUID (set DEV_UUID in .env to override)")
	} else {
		log.Printf("dev UUID loaded from .env")
	}
	capSecret = envVars["CAP_SECRET"]
	if capSecret == "" {
		log.Fatalf("FATAL: CAP_SECRET not set in .env! cap.js verification will not work.")
	}
	log.Printf("CAP_SECRET loaded from .env")

	capAPIEndpoint = envVars["CAP_API_ENDPOINT"]
	if capAPIEndpoint == "" {
		log.Fatalf("FATAL: CAP_API_ENDPOINT not set in .env! cap.js widget will not work.")
	}
	log.Printf("CAP_API_ENDPOINT loaded from .env")

	capSiteverifyURL = envVars["CAP_SITEVERIFY_URL"]
	if capSiteverifyURL == "" {
		log.Fatalf("FATAL: CAP_SITEVERIFY_URL not set in .env! cap.js verification will not work.")
	}
	log.Printf("CAP_SITEVERIFY_URL loaded from .env")

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
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	globalDB = db
	defer db.Close()

	if err := migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// Start SSH server (non-blocking)
	StartSSHServer(baseDir)

	// Start WS client to JS bot launcher (non-blocking)
	InitWSClient()
	log.Println("[WS] client connecting to JS bot launcher at ws://127.0.0.1:8889")

	// Global auto-restore: every 5 minutes, send restore command for all bots with auto_restore enabled
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			rows, err := db.Query("SELECT username FROM bots WHERE auto_restore = 1")
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
				su := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/sendinfo"}
				sc, _, serr := (&websocket.Dialer{}).Dial(su.String(), nil)
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

	mux := http.NewServeMux()

	// --- Logging middleware ---
	loggedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			disp = strings.ReplaceAll(disp, capSecret, "***")
			logLine += " | body=" + disp
		}

		// Wrap ResponseWriter to capture status
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: 200}
		mux.ServeHTTP(lrw, r)
		logLine += " | status=" + strconv.Itoa(lrw.statusCode)
		log.Println(logLine)
	})

	// --- POST /api/dev/auth : 简欢通动态验证码登录 ---
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

		userIDStr := r.FormValue("user_id")
		verifyCode := r.FormValue("verify_code")
		mcUsername := r.FormValue("mc_username")
		mcUUID := r.FormValue("mc_uuid")
		playerIP := r.FormValue("player_ip")

		if userIDStr == "" || verifyCode == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "msg": "user_id and verify_code required"})
			return
		}

		// Build multipart request to simpass API
		bodyBuf := &bytes.Buffer{}
		writer := multipart.NewWriter(bodyBuf)
		writer.WriteField("uuid", devUUID)
		writer.WriteField("user_id", userIDStr)
		writer.WriteField("verify_code", verifyCode)
		if mcUsername != "" {
			writer.WriteField("mc_username", mcUsername)
		}
		if mcUUID != "" {
			writer.WriteField("mc_uuid", mcUUID)
		}
		if playerIP != "" {
			writer.WriteField("player_ip", playerIP)
		}
		writer.Close()

		resp, err := http.Post(simpassAPIBase+"/api/dev/auth", writer.FormDataContentType(), bodyBuf)
		if err != nil {
			log.Printf("simpass api error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "upstream request failed: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		// Parse simpass response
		var simpassResp struct {
			Code     int    `json:"code"`
			Msg      string `json:"msg"`
			UserInfo *struct {
				SimpassUID int64  `json:"simpass_uid"`
				CreateTime string `json:"create_time"`
				Level      int64  `json:"level"`
				Risky      bool   `json:"risky"`
			} `json:"user_info"`
		}
		if err := json.Unmarshal(respBody, &simpassResp); err != nil {
			log.Printf("simpass response parse error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "bad upstream response"})
			return
		}

		if simpassResp.Code != 200 || simpassResp.UserInfo == nil {
			// Forward the upstream error directly
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
			return
		}

		// Auth success — find or create user in our DB
		uidStr := strconv.FormatInt(simpassResp.UserInfo.SimpassUID, 10)
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
				SimpassUID:                   simpassResp.UserInfo.SimpassUID,
				CreateTime:                   simpassResp.UserInfo.CreateTime,
				Level:                        simpassResp.UserInfo.Level,
				Risky:                        simpassResp.UserInfo.Risky,
			}
		} else {
			u.SimpassUID = simpassResp.UserInfo.SimpassUID
			u.CreateTime = simpassResp.UserInfo.CreateTime
			u.Level = simpassResp.UserInfo.Level
			u.Risky = simpassResp.UserInfo.Risky
		}

		// Issue JWT
		now := time.Now()
		exp := now.Add(tokenValidity)
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"jht_uid":        u.JhtUID,
			"remaining_bots": u.RemainingBotCreationQuantity,
			"level":          u.LevelID,
			"sim_uid":        simpassResp.UserInfo.SimpassUID,
			"sim_lv":         simpassResp.UserInfo.Level,
			"risky":          simpassResp.UserInfo.Risky,
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
				"simpass_uid": simpassResp.UserInfo.SimpassUID,
				"create_time": simpassResp.UserInfo.CreateTime,
				"level":       simpassResp.UserInfo.Level,
				"risky":       simpassResp.UserInfo.Risky,
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

			// Query simpass OTP status (only otp_id, NO uuid — uuid triggers generate)
			otpQueryURL := simpassAPIBase + "/api/dev/otp?otp_id=" + sess.OtpID
			log.Printf("[SESSION] poll token=%s... querying simpass: %s", sessionToken[:16], otpQueryURL)
			otpResp, err := http.Get(otpQueryURL)
			if err != nil {
				log.Printf("[SESSION] simpass otp query error: %v", err)
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "wait"})
				return
			}
			defer otpResp.Body.Close()
			otpBody, _ := io.ReadAll(otpResp.Body)
			log.Printf("[SESSION] simpass otp query raw response (http %d): %s", otpResp.StatusCode, string(otpBody))

			var otpStatus struct {
				Status   string `json:"status"`
				UserID   int64  `json:"user_id"`
				UserInfo *struct {
					SimpassUID int64  `json:"simpass_uid"`
					CreateTime string `json:"create_time"`
					Level      int64  `json:"level"`
					Risky      bool   `json:"risky"`
				} `json:"user_info"`
			}
			json.Unmarshal(otpBody, &otpStatus)

			if otpStatus.Status == "verified" && otpStatus.UserInfo != nil {
				log.Printf("[SESSION] poll token=%s... simpass VERIFIED! uid=%d", sessionToken[:16], otpStatus.UserID)

				// Find or create user in DB
				uidStr := strconv.FormatInt(otpStatus.UserInfo.SimpassUID, 10)
				u, _ := findUser(db, uidStr)
				if u == nil {
					u = &UserData{
						JhtUID:                       uidStr,
						LevelID:                      10001,
						RemainingBotCreationQuantity: 1,
						SimpassUID:                   otpStatus.UserInfo.SimpassUID,
						CreateTime:                   otpStatus.UserInfo.CreateTime,
						Level:                        otpStatus.UserInfo.Level,
						Risky:                        otpStatus.UserInfo.Risky,
					}
				} else {
					u.SimpassUID = otpStatus.UserInfo.SimpassUID
					u.CreateTime = otpStatus.UserInfo.CreateTime
					u.Level = otpStatus.UserInfo.Level
					u.Risky = otpStatus.UserInfo.Risky
				}

				// Issue JWT
				now := time.Now()
				exp := now.Add(tokenValidity)
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
					"jht_uid": u.JhtUID, "level": u.LevelID,
					"sim_uid": otpStatus.UserInfo.SimpassUID,
					"sim_lv":  otpStatus.UserInfo.Level,
					"risky":   otpStatus.UserInfo.Risky,
					"exp":     exp.Unix(), "iat": now.Unix(),
				})
				tokStr, _ := token.SignedString(jwtSecret)
				u.AccessToken = tokStr
				upsertUser(db, u)

				// Cache in session for subsequent polls, then return
				sess.Status = "ok"
				sess.Token = tokStr
				sess.SimpassUID = otpStatus.UserInfo.SimpassUID
				sess.CreateTime = otpStatus.UserInfo.CreateTime
				sess.Level = otpStatus.UserInfo.Level
				sess.Risky = otpStatus.UserInfo.Risky

				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":       "ok",
					"access_token": tokStr,
				})
				otpSessions.Delete(sessionToken)
				return
			}

			log.Printf("[SESSION] poll token=%s... simpass status=%s", sessionToken[:16], otpStatus.Status)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "wait"})
			return
		}

		// ────────── GET: 生成二维码 (cap_token) ──────────
		if r.Method == http.MethodGet && r.URL.Query().Get("cap_token") != "" {
			capTok := r.URL.Query().Get("cap_token")

			// verify cap
			capBody, _ := json.Marshal(map[string]string{"secret": capSecret, "response": capTok})
			capResp, err := http.Post(capSiteverifyURL, "application/json", bytes.NewReader(capBody))
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "verification service unavailable"})
				return
			}
			defer capResp.Body.Close()
			var capResult struct {
				Success bool `json:"success"`
			}
			json.NewDecoder(capResp.Body).Decode(&capResult)
			if !capResult.Success {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "human verification failed"})
				return
			}

			// Call simpass /api/dev/otp to get OTP ID
			otpURL := simpassAPIBase + "/api/dev/otp?uuid=" + devUUID
			log.Printf("[OTP] requesting from simpass: %s", otpURL)
			otpResp, err := http.Get(otpURL)
			if err != nil {
				log.Printf("[OTP] http error: %v", err)
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "otp service unavailable"})
				return
			}
			defer otpResp.Body.Close()
			otpBody, _ := io.ReadAll(otpResp.Body)
			log.Printf("[OTP] simpass raw response: %s", string(otpBody))
			var otpData struct {
				OtpID     string `json:"otp_id"`
				ExpiresIn int    `json:"expires_in"`
			}
			if err := json.Unmarshal(otpBody, &otpData); err != nil || otpData.OtpID == "" {
				log.Printf("[OTP] parse error: %v | body=%s", err, string(otpBody))
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "bad otp response"})
				return
			}
			log.Printf("[OTP] success: otp_id=%s expires_in=%d", otpData.OtpID, otpData.ExpiresIn)

			// Generate session token (128 hex chars = 64 bytes)
			sessionToken := generateToken(64)
			qrURL := simpassAPIBase + "/api/otp?otp_id=" + otpData.OtpID

			expiresIn := otpData.ExpiresIn
			if expiresIn <= 0 {
				expiresIn = 120
			}
			otpSessions.Store(sessionToken, &otpSession{
				OtpID:     otpData.OtpID,
				Status:    "wait",
				ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
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

		// cap.js human verification
		if req.CapToken == "" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "human verification required"})
			return
		}
		capBody, _ := json.Marshal(map[string]string{"secret": capSecret, "response": req.CapToken})
		capResp, err := http.Post(capSiteverifyURL, "application/json", bytes.NewReader(capBody))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "verification service unavailable"})
			return
		}
		defer capResp.Body.Close()
		var capResult struct {
			Success bool `json:"success"`
		}
		json.NewDecoder(capResp.Body).Decode(&capResult)
		if !capResult.Success {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 403, "msg": "human verification failed"})
			return
		}

		// Proxy to simpass
		bodyBuf := &bytes.Buffer{}
		writer := multipart.NewWriter(bodyBuf)
		writer.WriteField("uuid", devUUID)
		writer.WriteField("user_id", req.UserID)
		writer.WriteField("verify_code", req.VerifyCode)
		writer.Close()

		resp, err := http.Post(simpassAPIBase+"/api/dev/auth", writer.FormDataContentType(), bodyBuf)
		if err != nil {
			log.Printf("[AUTH] simpass request error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "msg": "upstream request failed"})
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[AUTH] simpass response for user_id=%s: code=%d body=%s", req.UserID, resp.StatusCode, string(respBody))

		var simpassResp struct {
			Code     int    `json:"code"`
			Msg      string `json:"msg"`
			UserInfo *struct {
				SimpassUID int64  `json:"simpass_uid"`
				CreateTime string `json:"create_time"`
				Level      int64  `json:"level"`
				Risky      bool   `json:"risky"`
			} `json:"user_info"`
		}
		json.Unmarshal(respBody, &simpassResp)

		if simpassResp.Code != 200 || simpassResp.UserInfo == nil {
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
			return
		}

		uidStr := strconv.FormatInt(simpassResp.UserInfo.SimpassUID, 10)
		u, _ := findUser(db, uidStr)
		if u == nil {
			u = &UserData{
				JhtUID:                       uidStr,
				LevelID:                      10001,
				RemainingBotCreationQuantity: 1,
				SimpassUID:                   simpassResp.UserInfo.SimpassUID,
				CreateTime:                   simpassResp.UserInfo.CreateTime,
				Level:                        simpassResp.UserInfo.Level,
				Risky:                        simpassResp.UserInfo.Risky,
			}
		} else {
			u.SimpassUID = simpassResp.UserInfo.SimpassUID
			u.CreateTime = simpassResp.UserInfo.CreateTime
			u.Level = simpassResp.UserInfo.Level
			u.Risky = simpassResp.UserInfo.Risky
		}

		now := time.Now()
		exp := now.Add(tokenValidity)
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"jht_uid": u.JhtUID, "level": u.LevelID,
			"sim_uid": simpassResp.UserInfo.SimpassUID,
			"sim_lv":  simpassResp.UserInfo.Level,
			"risky":   simpassResp.UserInfo.Risky,
			"exp":     exp.Unix(), "iat": now.Unix(),
		})
		tokStr, _ := token.SignedString(jwtSecret)
		u.AccessToken = tokStr
		upsertUser(db, u)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200, "msg": "Authentication successful",
			"user_info": map[string]interface{}{
				"simpass_uid": simpassResp.UserInfo.SimpassUID,
				"create_time": simpassResp.UserInfo.CreateTime,
				"level":       simpassResp.UserInfo.Level,
				"risky":       simpassResp.UserInfo.Risky,
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" {
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
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" || req.BotName == "" {
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" {
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
			BotName     string `json:"bot_name"`
			CreateTime  string `json:"create_time"`
			DSL         bool   `json:"DSL"`
			Status      string `json:"status"`
			AutoRestore bool   `json:"auto_restore"`
		}
		var items []botItem
		for _, b := range bots {
			items = append(items, botItem{
				BotName:     b.Username,
				CreateTime:  b.CreationTime,
				DSL:         b.DSL,
				Status:      b.Status,
				AutoRestore: b.AutoRestore,
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" || req.BotName == "" {
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
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
		updateBotStatus(globalDB, req.BotName, "confirmed")
		json.NewEncoder(w).Encode(map[string]interface{}{"code": "200", "message": "验证成功，机器人已确认归属", "chat": chat})

		// Stop the bot after 5s (async)
		go func() {
			time.Sleep(5 * time.Second)
			if stopErr := client.StopBot(req.BotName); stopErr != nil {
				log.Printf("[WS] stopbot after verify success: %v", stopErr)
			}
		}()
	})

	// --- POST /api/getbotstatus : 查询机器人状态 ---
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" || req.BotName == "" {
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
		online, err := client.GetBotStatus(req.BotName)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 502, "message": "bad gateway!!!机器人服务异常，请联系管理员!"})
			return
		}

		server := "main"
		// If online, we could determine server from context; default to "main"
		if !online {
			server = ""
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":           "200",
			"online":         online,
			"DSL":            bot.DSL,
			"auto_restore":   bot.AutoRestore,
			"auto_reconnect": bot.AutoReconnect,
			"server":         server,
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" || req.BotName == "" {
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" || req.BotName == "" {
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

		// Send via sendinfo WS
		u := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/sendinfo"}
		wsConn, _, err := (&websocket.Dialer{}).Dial(u.String(), nil)
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

	// --- GET /api/config : 前端配置 ---
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"cap_api_endpoint": capAPIEndpoint,
		})
	})

	// --- WebSocket: /ws/api/connectbot : 机器人日志流 ---
	var wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// wsKeepAlive sets up ping/pong heartbeat on a WebSocket connection.
	// Returns a stop function to clean up the goroutine.
	wsKeepAlive := func(conn *websocket.Conn) func() {
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
					conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
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
		defer conn.Close()
		stopHeartbeat := wsKeepAlive(conn)
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
			BotName     string `json:"botname"`
		}
		if err := json.Unmarshal(authMsg, &auth); err != nil || auth.AccessToken == "" || auth.BotName == "" {
			conn.WriteJSON(map[string]interface{}{"code": 400, "message": "accesstoken and botname required"})
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
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, auth.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			conn.WriteJSON(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Build history file path
		logsDir := filepath.Join(baseDir, "botlogs")
		os.MkdirAll(logsDir, 0755)
		logFile := filepath.Join(logsDir, auth.BotName+".log")

		// Load last 100 lines as base64 history
		var historyLines []string
		if f, err := os.Open(logFile); err == nil {
			defer f.Close()
			data, _ := io.ReadAll(f)
			allLines := strings.Split(string(data), "\n")
			start := 0
			if len(allLines) > 100 {
				start = len(allLines) - 100
			}
			for i := start; i < len(allLines); i++ {
				if allLines[i] != "" {
					historyLines = append(historyLines, allLines[i])
				}
			}
		}
		historyB64 := base64.StdEncoding.EncodeToString([]byte(strings.Join(historyLines, "\n")))

		// Send history to browser
		conn.WriteJSON(map[string]interface{}{
			"code": "200",
			"data": []map[string]string{{"log": historyB64}},
		})

		// Connect to JS launcher botlogs WS
		client := InitWSClient()
		jsConn, err := client.BotLogs(auth.BotName)
		if err != nil {
			// Bot offline — notify browser and keep connection open
			log.Printf("[WS] connectbot botlogs (bot offline): %v", err)
			conn.WriteJSON(map[string]interface{}{
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
									conn.WriteJSON(map[string]interface{}{
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
									// Try to restart: connect to JS startbot WS
									restartURL := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/startbot"}
									rc, _, rerr := (&websocket.Dialer{}).Dial(restartURL.String(), nil)
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
										conn.WriteJSON(map[string]interface{}{
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
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
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
					}
				}
			}
		}()
		<-done
	})

	// --- WebSocket: /ws/api/startbot : 启动机器人 ---
	mux.HandleFunc("/ws/api/startbot", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] startbot upgrade error: %v", err)
			return
		}
		defer conn.Close()
		stopHeartbeat := wsKeepAlive(conn)
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
		if err := json.Unmarshal(authMsg, &req); err != nil || req.AccessToken == "" || req.BotName == "" {
			conn.WriteJSON(map[string]interface{}{"code": 400, "message": "access_token and botname required"})
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
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			conn.WriteJSON(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Connect to JS launcher and start the bot
		jsURL := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/startbot"}
		jsConn, _, err := (&websocket.Dialer{}).Dial(jsURL.String(), nil)
		if err != nil {
			conn.WriteJSON(map[string]interface{}{"code": 502, "message": "bad gateway!!!无法连接到机器人服务"})
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
			conn.WriteJSON(map[string]interface{}{"code": 502, "message": "发送启动命令失败"})
			return
		}

		// Read start response from JS
		jsConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, jsResp, err := jsConn.ReadMessage()
		if err != nil {
			conn.WriteJSON(map[string]interface{}{"code": 502, "message": "启动超时，未收到机器人响应"})
			return
		}
		var jsResult map[string]interface{}
		json.Unmarshal(jsResp, &jsResult)

		// Forward JS response to browser
		conn.WriteJSON(jsResult)

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
					if err := conn.WriteMessage(websocket.TextMessage, evtMsg); err != nil {
						return
					}
				}
			}()
			<-done
		}
	})

	// --- WebSocket: /ws/api/stopbot : 关闭机器人 ---
	mux.HandleFunc("/ws/api/stopbot", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] stopbot upgrade error: %v", err)
			return
		}
		defer conn.Close()
		stopHeartbeat := wsKeepAlive(conn)
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
		if err := json.Unmarshal(authMsg, &req); err != nil || req.AccessToken == "" || req.BotName == "" {
			conn.WriteJSON(map[string]interface{}{"code": 400, "message": "access_token and botname required"})
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
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}
		claims, _ := token.Claims.(jwt.MapClaims)
		jhtUID, _ := claims["jht_uid"].(string)
		if jhtUID == "" {
			conn.WriteJSON(map[string]interface{}{"code": 401, "message": "无效的access_token"})
			return
		}

		// Check bot belongs to user
		bot, err := findBotByUsername(globalDB, req.BotName)
		if err != nil || bot == nil || bot.Belong != jhtUID {
			conn.WriteJSON(map[string]interface{}{"code": 403, "message": "此机器人不属于你，你无权控制"})
			return
		}

		// Connect to JS launcher stopbot
		client := InitWSClient()
		if err := client.StopBot(req.BotName); err != nil {
			log.Printf("[WS] stopbot error: %v", err)
			conn.WriteJSON(map[string]interface{}{"code": 502, "message": "关闭失败: " + err.Error()})
			return
		}

		conn.WriteJSON(map[string]interface{}{"code": 200, "message": "机器人已断开"})
	})

	// --- Static file serving with config injection ---
	staticDir := filepath.Join(baseDir, "static")
	fileServer := http.FileServer(http.Dir(staticDir))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inject cap endpoint into login.html on the fly
		if r.URL.Path == "/login.html" || r.URL.Path == "/login" {
			loginPath := filepath.Join(staticDir, "login.html")
			if data, err := os.ReadFile(loginPath); err == nil {
				content := strings.ReplaceAll(string(data), "{{CAP_API_ENDPOINT}}", capAPIEndpoint)
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
		simpass_uid INTEGER,
		create_time TEXT,
		sim_level INTEGER,
		risky INTEGER,
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

	// Bots table
	botsStmt := `CREATE TABLE IF NOT EXISTS bots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		belong TEXT NOT NULL,
		creation_time TEXT NOT NULL,
		username TEXT NOT NULL,
		dsl INTEGER DEFAULT 0,
		status TEXT DEFAULT 'no',
		auto_restore INTEGER DEFAULT 1,
		auto_reconnect INTEGER DEFAULT 1
	);`
	if _, err := db.Exec(botsStmt); err != nil {
		return err
	}
	db.Exec("ALTER TABLE bots ADD COLUMN status TEXT DEFAULT 'no'")
	db.Exec("ALTER TABLE bots ADD COLUMN auto_restore INTEGER DEFAULT 1")
	db.Exec("ALTER TABLE bots ADD COLUMN auto_reconnect INTEGER DEFAULT 1")
	return nil
}

func findUser(db *sql.DB, uid string) (*UserData, error) {
	row := db.QueryRow("SELECT jht_uid, accesstoken, COALESCE(remaining_bot_creation_quantity,1), level_id, simpass_uid, create_time, sim_level, risky, COALESCE(last_login_time,''), COALESCE(status,'ok'), COALESCE(status_info,'') FROM userdata WHERE jht_uid = ?", uid)
	var u UserData
	var riskyInt int64
	err := row.Scan(&u.JhtUID, &u.AccessToken, &u.RemainingBotCreationQuantity, &u.LevelID, &u.SimpassUID, &u.CreateTime, &u.Level, &riskyInt, &u.LastLoginTime, &u.Status, &u.StatusInfo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Risky = riskyInt != 0
	return &u, nil
}

func upsertUser(db *sql.DB, u *UserData) error {
	riskyInt := int64(0)
	if u.Risky {
		riskyInt = 1
	}
	_, err := db.Exec(`INSERT INTO userdata(jht_uid, accesstoken, remaining_bot_creation_quantity, level_id, simpass_uid, create_time, sim_level, risky, last_login_time, status, status_info)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(jht_uid) DO UPDATE SET
			accesstoken=excluded.accesstoken,
			remaining_bot_creation_quantity=excluded.remaining_bot_creation_quantity,
			level_id=excluded.level_id,
			simpass_uid=excluded.simpass_uid,
			create_time=excluded.create_time,
			sim_level=excluded.sim_level,
			risky=excluded.risky,
			last_login_time=excluded.last_login_time,
			status=excluded.status,
			status_info=excluded.status_info;`,
		u.JhtUID, u.AccessToken, u.RemainingBotCreationQuantity, u.LevelID, u.SimpassUID, u.CreateTime, u.Level, riskyInt, u.LastLoginTime, u.Status, u.StatusInfo)
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

func extractSimpassUID(chat string) string {
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
