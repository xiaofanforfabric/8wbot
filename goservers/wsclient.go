package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// BotSession holds a persistent WS connection to a started bot for event monitoring.
type BotSession struct {
	conn    *websocket.Conn
	botName string
	mu      sync.Mutex
	closed  bool
}

// Close safely closes the session's WS connection.
func (s *BotSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.conn != nil {
		s.closed = true
		s.conn.Close()
	}
}

var activeSessions sync.Map // botName -> *BotSession

// InitWSClient is kept for backward compatibility.
func InitWSClient() *WSClient {
	return &WSClient{}
}

// WSClient placeholder for backward compatibility.
type WSClient struct{}

// wsPoller sets up ping/pong heartbeat on an outgoing WS connection to JS.
// Starts a goroutine that sends pings periodically.
func wsPoller(conn *websocket.Conn) func() {
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

// StartBotAndDetect spawns a dedicated WS connection, starts a bot, monitors
// events, and returns when we see "正在生成动态二维码，请稍候..." or timeout.
// Returns the matched chat text and the bot's simpass UID (from chat or bot name).
func (c *WSClient) StartBotAndDetect(username string, timeout time.Duration) (chat string, uid string, err error) {
	u := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/startbot"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("ws dial: %w", err)
	}
	stopPoller := wsPoller(conn)
	defer stopPoller()

	// Consume welcome message
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})

	// Send start command
	req, _ := json.Marshal(map[string]string{"username": username})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		conn.Close()
		return "", "", fmt.Errorf("write start: %w", err)
	}

	// Read start response
	_, resp, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return "", "", fmt.Errorf("read start resp: %w", err)
	}
	var startResp map[string]interface{}
	json.Unmarshal(resp, &startResp)
	if code, _ := startResp["code"].(float64); code != 200 {
		msg, _ := startResp["message"].(string)
		conn.Close()
		return "", "", fmt.Errorf("start failed: %s", msg)
	}

	// Monitor events
	deadline := time.Now().Add(timeout)
	conn.SetReadDeadline(deadline)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			return "", "", fmt.Errorf("event read: %w", err)
		}
		// Parse event array: [{ botname, data: [{ chat }] }]
		var events []struct {
			BotName string `json:"botname"`
			Data    []struct {
				Chat string `json:"chat"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg, &events); err != nil {
			log.Printf("[WS] unmarshal fail (non-event msg?): %s", string(msg))
			continue
		}
		for _, ev := range events {
			for _, d := range ev.Data {
				log.Printf("[WS] event from %s: %s", ev.BotName, d.Chat)

				// Found the welcome message with UID
				if strings.Contains(d.Chat, "您绑定的简幻通UID是") {
					conn.SetReadDeadline(time.Time{})
					session := &BotSession{conn: conn, botName: username}
					activeSessions.Store(username, session)
					uid := extractSimpassUID(d.Chat)
					if uid == "" {
						uid = username
					}
					log.Printf("[WS] verify session created for %s: %s", username, d.Chat)
					return d.Chat, uid, nil
				}
			}
		}
	}
}

// StopBot sends a stop command via WS to terminate a bot.
func (c *WSClient) StopBot(username string) error {
	u := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/stopbot"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("stopbot dial: %w", err)
	}
	defer conn.Close()

	// Consume welcome
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})

	// Send stop command
	req, _ := json.Marshal(map[string]string{"username": username})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		return fmt.Errorf("stopbot write: %w", err)
	}

	// Read response
	_, resp, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("stopbot read resp: %w", err)
	}
	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	if code, _ := result["code"].(float64); code != 200 {
		msg, _ := result["message"].(string)
		return fmt.Errorf("stopbot failed: %s", msg)
	}
	return nil
}

// GetBotStatus queries the JS launcher for bot online status.
// Returns online bool.
func (c *WSClient) GetBotStatus(username string) (online bool, err error) {
	u := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/botstatus"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("botstatus dial: %w", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})

	req, _ := json.Marshal(map[string]string{"username": username})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		return false, fmt.Errorf("botstatus write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		return false, fmt.Errorf("botstatus read resp: %w", err)
	}
	var result struct {
		Code   float64 `json:"code"`
		Online bool    `json:"online"`
	}
	json.Unmarshal(resp, &result)
	return result.Online, nil
}

// BotLogs connects to the botlogs WS and returns the connection for reading events.
// Caller must close the connection.
func (c *WSClient) BotLogs(username string) (*websocket.Conn, error) {
	u := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/botlogs"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("botlogs dial: %w", err)
	}

	// Set up pong handler for heartbeat from JS server
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})

	req, _ := json.Marshal(map[string]string{"username": username})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("botlogs write: %w", err)
	}

	// Read the 200 monitoring started response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, resp, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("botlogs read resp: %w", err)
	}
	var result struct {
		Code float64 `json:"code"`
	}
	json.Unmarshal(resp, &result)
	if result.Code != 200 {
		conn.Close()
		return nil, fmt.Errorf("botlogs: bot not running")
	}
	return conn, nil
}

// SendCommandAndDetect sends a chat command via sendinfo WS, then reads events
// from the stored BotSession (startbot WS) to detect verification result.
func (c *WSClient) SendCommandAndDetect(botname string, chatText string, timeout time.Duration) (string, error) {
	// Look up the active session
	raw, ok := activeSessions.Load(botname)
	if !ok {
		return "", fmt.Errorf("no active verify session for %s, start verify first", botname)
	}
	session := raw.(*BotSession)
	defer func() {
		session.Close()
		activeSessions.Delete(botname)
	}()

	// Send code via sendinfo
	u := url.URL{Scheme: "ws", Host: "127.0.0.1:8889", Path: "/ws/api/sendinfo"}
	sinfo, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("sendinfo dial: %w", err)
	}
	defer sinfo.Close()

	// Consume welcome
	sinfo.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = sinfo.ReadMessage()
	sinfo.SetReadDeadline(time.Time{})

	// Send command
	req, _ := json.Marshal(map[string]interface{}{
		"botname": botname,
		"data":    []map[string]string{{"chat": chatText}},
	})
	if err := sinfo.WriteMessage(websocket.TextMessage, req); err != nil {
		return "", fmt.Errorf("write cmd: %w", err)
	}

	// Read command response
	_, cmdResp, err := sinfo.ReadMessage()
	if err != nil {
		return "", fmt.Errorf("read cmd resp: %w", err)
	}
	var cmdResult map[string]interface{}
	json.Unmarshal(cmdResp, &cmdResult)
	if code, _ := cmdResult["code"].(float64); code != 200 {
		msg, _ := cmdResult["message"].(string)
		return "", fmt.Errorf("cmd failed: %s", msg)
	}

	// Now read events from the stored startbot connection for verification result
	deadline := time.Now().Add(timeout)
	session.conn.SetReadDeadline(deadline)

	for {
		_, msg, err := session.conn.ReadMessage()
		if err != nil {
			return "", fmt.Errorf("verify event read: %w", err)
		}
		var events []struct {
			BotName string `json:"botname"`
			Data    []struct {
				Chat string `json:"chat"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg, &events); err != nil {
			continue
		}
		for _, ev := range events {
			for _, d := range ev.Data {
				chat := d.Chat
				if strings.Contains(chat, "验证成功") {
					return chat, nil
				}
				if strings.Contains(chat, "验证失败") || strings.Contains(chat, "ID或验证码错误") {
					return chat, fmt.Errorf("verify_failed")
				}
			}
		}
	}
}
