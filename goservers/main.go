package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	JhtUID      string `json:"jht_uid"`
	AccessToken string `json:"accesstoken"`
	BotName     string `json:"bot_name"`
	LevelID     int64  `json:"level_id"`
	SimpassUID  int64  `json:"simpass_uid"`
	CreateTime  string `json:"create_time"`
	Level       int64  `json:"level"`
	Risky       bool   `json:"risky"`
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
		capSecret = "sk-mRQIS4lQqqMICYvAaEg3P3rScMYhMt0hbRs22bMuAPI"
		log.Printf("using default CAP_SECRET")
	} else {
		log.Printf("CAP_SECRET loaded from .env")
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
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

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
				JhtUID:     uidStr,
				BotName:    mcUsername,
				LevelID:    10001,
				SimpassUID: simpassResp.UserInfo.SimpassUID,
				CreateTime: simpassResp.UserInfo.CreateTime,
				Level:      simpassResp.UserInfo.Level,
				Risky:      simpassResp.UserInfo.Risky,
			}
		} else {
			u.SimpassUID = simpassResp.UserInfo.SimpassUID
			u.CreateTime = simpassResp.UserInfo.CreateTime
			u.Level = simpassResp.UserInfo.Level
			u.Risky = simpassResp.UserInfo.Risky
			if mcUsername != "" {
				u.BotName = mcUsername
			}
		}

		// Issue JWT
		now := time.Now()
		exp := now.Add(tokenValidity)
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"jht_uid": u.JhtUID,
			"bot":     u.BotName,
			"level":   u.LevelID,
			"sim_uid": simpassResp.UserInfo.SimpassUID,
			"sim_lv":  simpassResp.UserInfo.Level,
			"risky":   simpassResp.UserInfo.Risky,
			"exp":     exp.Unix(),
			"iat":     now.Unix(),
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
						JhtUID:     uidStr,
						LevelID:    10001,
						SimpassUID: otpStatus.UserInfo.SimpassUID,
						CreateTime: otpStatus.UserInfo.CreateTime,
						Level:      otpStatus.UserInfo.Level,
						Risky:      otpStatus.UserInfo.Risky,
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
			capResp, err := http.Post("https://api.xiaofanshop.cn/siteverify", "application/json", bytes.NewReader(capBody))
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
		capResp, err := http.Post("https://api.xiaofanshop.cn/siteverify", "application/json", bytes.NewReader(capBody))
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
				JhtUID:     uidStr,
				LevelID:    10001,
				SimpassUID: simpassResp.UserInfo.SimpassUID,
				CreateTime: simpassResp.UserInfo.CreateTime,
				Level:      simpassResp.UserInfo.Level,
				Risky:      simpassResp.UserInfo.Risky,
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

	// --- Static file serving ---
	staticDir := filepath.Join(baseDir, "static")
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

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

func migrate(db *sql.DB) error {
	sqlStmt := `CREATE TABLE IF NOT EXISTS userdata (
		jht_uid TEXT PRIMARY KEY,
		accesstoken TEXT,
		bot_name TEXT,
		level_id INTEGER,
		simpass_uid INTEGER,
		create_time TEXT,
		sim_level INTEGER,
		risky INTEGER
	);`
	_, err := db.Exec(sqlStmt)
	return err
}

func findUser(db *sql.DB, uid string) (*UserData, error) {
	row := db.QueryRow("SELECT jht_uid, accesstoken, bot_name, level_id, simpass_uid, create_time, sim_level, risky FROM userdata WHERE jht_uid = ?", uid)
	var u UserData
	var riskyInt int64
	err := row.Scan(&u.JhtUID, &u.AccessToken, &u.BotName, &u.LevelID, &u.SimpassUID, &u.CreateTime, &u.Level, &riskyInt)
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
	_, err := db.Exec(`INSERT INTO userdata(jht_uid, accesstoken, bot_name, level_id, simpass_uid, create_time, sim_level, risky)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(jht_uid) DO UPDATE SET
			accesstoken=excluded.accesstoken,
			bot_name=excluded.bot_name,
			level_id=excluded.level_id,
			simpass_uid=excluded.simpass_uid,
			create_time=excluded.create_time,
			sim_level=excluded.sim_level,
			risky=excluded.risky;`,
		u.JhtUID, u.AccessToken, u.BotName, u.LevelID, u.SimpassUID, u.CreateTime, u.Level, riskyInt)
	return err
}
