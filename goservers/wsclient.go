package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
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

// Replace 用新的 startbot 连接替换会话里的旧连接。
// 重新拉起掉线的机器人后使用：后续要从新连接读取验证结果。
func (s *BotSession) Replace(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn != conn {
		s.conn.Close()
	}
	s.conn = conn
	s.closed = false
}

// Conn 返回当前会话使用的连接。
func (s *BotSession) Conn() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

var activeSessions sync.Map // botName -> *BotSession

// jsDialer 用于连接内地 JS 节点。
//
// 必须显式设置超时：默认的 websocket.DefaultDialer 握手超时为 0（永不超时），
// 当节点不可达时拨号会一直挂着，前端表现为长时间转圈直至 axios 超时。
var jsDialer = &websocket.Dialer{
	HandshakeTimeout: 10 * time.Second,
	NetDial:          (&net.Dialer{Timeout: 8 * time.Second}).Dial,
}

// getJSNodeURL 构建连接内地 E5 机器 JS 节点的完整 URL (自动附加鉴权 secret)
func getJSNodeURL(apiPath string) string {
	rawBase := os.Getenv("JS_NODE_URL")
	if rawBase == "" {
		// 默认走 443 的 wss：JS 节点位于 Cloudflare 代理之后，
		// Cloudflare 不代理 8889 端口，只有 443 才可达。
		rawBase = "wss://js.xiaofanai.uk"
	}
	secret := os.Getenv("INTERNAL_NODE_SECRET")

	u, err := url.Parse(rawBase)
	if err != nil {
		u = &url.URL{Scheme: "wss", Host: "js.xiaofanai.uk"}
	}
	u.Path = apiPath

	if secret != "" {
		q := u.Query()
		q.Set("secret", secret)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// InitWSClient is kept for backward compatibility.
func InitWSClient() *WSClient {
	return &WSClient{}
}

// WSClient placeholder for backward compatibility.
type WSClient struct{}

func wsPoller(conn *websocket.Conn) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
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
// events, and returns when we see "您绑定的简幻通UID是" or timeout.
// startBotDial 拨通 JS 节点的 /ws/api/startbot 并下发启动指令，
// 返回仍然打开的连接与启动响应（code=200 表示已受理）。
// 失败时由本函数负责关闭连接；成功时由调用方负责。
func startBotDial(username string) (*websocket.Conn, map[string]interface{}, error) {
	targetURL := getJSNodeURL("/ws/api/startbot")
	conn, _, err := jsDialer.Dial(targetURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ws dial: %w", err)
	}

	// Consume welcome message
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})

	// Send start command
	req, _ := json.Marshal(map[string]string{"username": username})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write start: %w", err)
	}

	// Read start response
	_, resp, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read start resp: %w", err)
	}
	var startResp map[string]interface{}
	json.Unmarshal(resp, &startResp)
	return conn, startResp, nil
}

// startBotReplacingStale 启动机器人；若 JS 节点回报 409（已有同名实例在运行），
// 先把它停掉再重新启动一次。
//
// 为什么必须这样处理：未完成归属验证的机器人在界面上「上线/下线」按钮是禁用的
// （UserPage 的 :disabled 里带了 status !== 'confirmed'），用户没有任何办法自己
// 清理上一次验证遗留下来的实例。如果这里直接以 409 失败，用户就会陷入
// 「验证不了 → 上不了线 → 也停不掉 → 永远验证不了」的死锁。
func (c *WSClient) startBotReplacingStale(username string) (*websocket.Conn, error) {
	conn, startResp, err := startBotDial(username)
	if err != nil {
		return nil, err
	}
	code, _ := startResp["code"].(float64)
	if code == 409 {
		msg, _ := startResp["message"].(string)
		log.Printf("[WS] %s 已有实例在运行（%s），先停止再重新启动", username, msg)
		conn.Close()
		if serr := c.StopBot(username); serr != nil {
			log.Printf("[WS] 清理残留实例失败 %s: %v", username, serr)
		}
		// 给 JS 节点一点时间真正断开连接，否则紧接着的启动可能仍被判重复
		time.Sleep(2 * time.Second)
		conn, startResp, err = startBotDial(username)
		if err != nil {
			return nil, err
		}
		code, _ = startResp["code"].(float64)
	}
	if code != 200 {
		msg, _ := startResp["message"].(string)
		conn.Close()
		return nil, fmt.Errorf("start failed: %s", msg)
	}
	return conn, nil
}

// StartBotSimple 只负责把机器人拉起来，不等任何聊天内容。
//
// 供全局自动重连巡护使用。拉起后立即关闭这条 WS：JS 节点的 startbot 分支
// 只在 bot 事件里往 ws 写数据，并没有注册 close 回调，所以连接关闭不会
// 影响已经跑起来的机器人本体。
func (c *WSClient) StartBotSimple(username string) error {
	conn, err := c.startBotReplacingStale(username)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (c *WSClient) StartBotAndDetect(username string, timeout time.Duration) (chat string, uid string, err error) {
	conn, err := c.startBotReplacingStale(username)
	if err != nil {
		return "", "", err
	}
	stopPoller := wsPoller(conn)
	defer stopPoller()

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

func extractSimpassUID(text string) string {
	idx := strings.Index(text, "您绑定的简幻通UID是")
	if idx == -1 {
		return ""
	}
	sub := text[idx+len("您绑定的简幻通UID是"):]
	sub = strings.TrimSpace(sub)
	fields := strings.Fields(sub)
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// StopBot sends a stop command via WS to terminate a bot.
func (c *WSClient) StopBot(username string) error {
	targetURL := getJSNodeURL("/ws/api/stopbot")
	conn, _, err := jsDialer.Dial(targetURL, nil)
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

// BotStatusInfo 是 JS 节点 /ws/api/botstatus 返回的状态信息
type BotStatusInfo struct {
	Online        bool
	LastExitReason string
	LastExitType   string
	LastExitTime   string
}

// GetBotStatus queries the JS launcher for bot online status.
// Returns online bool.
func (c *WSClient) GetBotStatus(username string) (online bool, err error) {
	info, err := c.GetBotStatusInfo(username)
	return info.Online, err
}

// GetBotStatusInfo 查询节点状态，含上次退出原因（供前端展示与落库）
func (c *WSClient) GetBotStatusInfo(username string) (BotStatusInfo, error) {
	var info BotStatusInfo
	targetURL := getJSNodeURL("/ws/api/botstatus")
	conn, _, err := jsDialer.Dial(targetURL, nil)
	if err != nil {
		return info, fmt.Errorf("botstatus dial: %w", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})

	req, _ := json.Marshal(map[string]string{"username": username})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		return info, fmt.Errorf("botstatus write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		return info, fmt.Errorf("botstatus read resp: %w", err)
	}
	var result struct {
		Code           float64 `json:"code"`
		Online         bool    `json:"online"`
		LastExitReason string  `json:"last_exit_reason"`
		LastExitType   string  `json:"last_exit_type"`
		LastExitTime   string  `json:"last_exit_time"`
	}
	json.Unmarshal(resp, &result)
	info.Online = result.Online
	info.LastExitReason = result.LastExitReason
	info.LastExitType = result.LastExitType
	info.LastExitTime = result.LastExitTime
	return info, nil
}

// BotLogs connects to the botlogs WS and returns the connection for reading events.
// Caller must close the connection.
func (c *WSClient) BotLogs(username string) (*websocket.Conn, error) {
	targetURL := getJSNodeURL("/ws/api/botlogs")
	conn, _, err := jsDialer.Dial(targetURL, nil)
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

// sendInfoOnce 通过 sendinfo 通道给指定机器人投递一条聊天，只负责投递与应答。
func (c *WSClient) sendInfoOnce(botname string, chatText string) error {
	targetURL := getJSNodeURL("/ws/api/sendinfo")
	sinfo, _, err := jsDialer.Dial(targetURL, nil)
	if err != nil {
		return fmt.Errorf("sendinfo dial: %w", err)
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
		return fmt.Errorf("write cmd: %w", err)
	}

	// Read command response
	_, cmdResp, err := sinfo.ReadMessage()
	if err != nil {
		return fmt.Errorf("read cmd resp: %w", err)
	}
	var cmdResult map[string]interface{}
	json.Unmarshal(cmdResp, &cmdResult)
	if code, _ := cmdResult["code"].(float64); code != 200 {
		msg, _ := cmdResult["message"].(string)
		return fmt.Errorf("cmd failed: %s", msg)
	}
	return nil
}

// isBotNotRunningErr 判断错误是否为「机器人当前不在运行」。
// JS 节点在 sendinfo 里查不到 bots[botname] 时回 code=404，消息为
// 「机器人没有运行，请先启动」/「机器人没有运行」。
func isBotNotRunningErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "机器人没有运行")
}

// restartBotForVerify 重新拉起一个已经掉线的机器人，等它真正进入游戏后
// 返回新的 startbot 连接（后续从这个连接读验证结果）。
func (c *WSClient) restartBotForVerify(username string) (*websocket.Conn, error) {
	conn, err := c.startBotReplacingStale(username)
	if err != nil {
		return nil, err
	}

	// JS 节点在 spawn 时会推 [系统] <name> 已进入游戏
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("等待机器人进入游戏失败: %w", err)
		}
		var events []struct {
			BotName string `json:"botname"`
			Data    []struct {
				Chat string `json:"chat"`
			} `json:"data"`
		}
		if json.Unmarshal(msg, &events) != nil {
			continue
		}
		ready := false
		for _, ev := range events {
			for _, d := range ev.Data {
				if strings.Contains(d.Chat, "已进入游戏") {
					ready = true
				}
			}
		}
		if ready {
			conn.SetReadDeadline(time.Time{})
			// 刚进游戏时服务器还没把验证提示发下来，稍等一下再发验证码，
			// 否则验证码可能早于提示到达而被服务器忽略
			time.Sleep(5 * time.Second)
			return conn, nil
		}
	}
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

	// 投递验证码。若机器人此时已经掉线，就地重新拉起再发一次。
	//
	// 典型掉线原因：SimpPass 要求 120 秒内完成验证，超时会把机器人踢出
	// （服务端日志: 验证超时！您需要在 120 秒内完成身份验证。）；或资源包
	// 卡在 configuration 阶段导致断线。若不这样兜底，用户只会看到
	// 「机器人没有运行，请先启动」—— 而未验证机器人的「上线」按钮是禁用的，
	// 等于彻底卡死，没有任何自救途径。
	if err := c.sendInfoOnce(botname, chatText); err != nil {
		if !isBotNotRunningErr(err) {
			return "", err
		}
		log.Printf("[WS] %s 已掉线，重新启动后再发送验证码（原错误: %v）", botname, err)
		newConn, rerr := c.restartBotForVerify(botname)
		if rerr != nil {
			return "", fmt.Errorf("机器人已掉线，重新启动也失败（可能是服务器验证超时把它踢了，请重新点「验证」再试）: %w", rerr)
		}
		session.Replace(newConn)
		if err2 := c.sendInfoOnce(botname, chatText); err2 != nil {
			return "", err2
		}
		log.Printf("[WS] %s 已重新启动，验证码重新投递成功", botname)
	}

	// Now read events from the stored startbot connection for verification result
	deadline := time.Now().Add(timeout)
	session.Conn().SetReadDeadline(deadline)

	for {
		_, msg, err := session.Conn().ReadMessage()
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
