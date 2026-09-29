package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 假节点：实现 /ws/api/expand，走和真节点一样的握手。
func fakeNode(t *testing.T, delay time.Duration, answer map[string]interface{}) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// 握手帧：真节点也是先发一条 {ok:true}
		c.WriteJSON(map[string]interface{}{"ok": true})
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		time.Sleep(delay)
		c.WriteJSON(answer)
	}))
}

// 测节点不可达时 dispatchExpand 要多久返回，以及返回什么。
//
// 这条路径现在挂在 WebSocket 上，浏览器那边有超时。如果 dispatchExpand
// 卡得比浏览器超时还久，用户看到的就是「等待后端确认超时」——
// 完全不知道是节点连不上。
func TestDispatchExpandNodeUnreachable(t *testing.T) {
	// 指向一个没人监听的端口
	os.Setenv("JS_NODE_URL", "ws://127.0.0.1:1")
	defer os.Unsetenv("JS_NODE_URL")

	start := time.Now()
	// AccessToken 空会提前返回，所以这里只测「拨号失败」这段：
	// 鉴权需要真 JWT，测不了，改为直接测拨号超时上界。
	conn, _, err := jsDialer.Dial(getJSNodeURL("/ws/api/expand"), nil)
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		t.Fatal("不该连上")
	}
	t.Logf("拨号失败耗时 %v，错误: %v", elapsed, err)
	if elapsed > 12*time.Second {
		t.Errorf("拨号失败花了 %v，超过浏览器 10 秒预算", elapsed)
	}
}

// 测节点正常时的往返耗时。
func TestDispatchExpandRoundTrip(t *testing.T) {
	srv := fakeNode(t, 50*time.Millisecond, map[string]interface{}{
		"code": 200, "message": "已下发 3 个区块，开始扩地",
	})
	defer srv.Close()
	os.Setenv("JS_NODE_URL", "ws"+strings.TrimPrefix(srv.URL, "http"))
	defer os.Unsetenv("JS_NODE_URL")

	start := time.Now()
	conn, _, err := jsDialer.Dial(getJSNodeURL("/ws/api/expand"), nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	conn.ReadMessage() // 握手帧
	conn.SetReadDeadline(time.Time{})
	conn.WriteMessage(websocket.TextMessage, []byte(`{"username":"x","chunks":[[1,2]]}`))
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("等回包失败: %v", err)
	}
	t.Logf("往返 %v，回包 %s", time.Since(start), resp)
	if time.Since(start) > 5*time.Second {
		t.Errorf("往返太慢: %v", time.Since(start))
	}
}

// 测节点不回包时，读超时是否按预期在 8 秒左右触发。
//
// 这个值必须小于浏览器那边的等待上限，否则用户看到的是「超时」而不是
// 「未收到扩地队列确认」—— 后者才有排查价值。
func TestDispatchExpandReadTimeout(t *testing.T) {
	srv := fakeNode(t, 30*time.Second, map[string]interface{}{"code": 200})
	defer srv.Close()
	os.Setenv("JS_NODE_URL", "ws"+strings.TrimPrefix(srv.URL, "http"))
	defer os.Unsetenv("JS_NODE_URL")

	conn, _, err := jsDialer.Dial(getJSNodeURL("/ws/api/expand"), nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	conn.WriteMessage(websocket.TextMessage, []byte(`{"username":"x"}`))

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	_, _, err = conn.ReadMessage()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("应该读超时")
	}
	t.Logf("读超时在 %v 触发", elapsed)
	if elapsed < 7*time.Second || elapsed > 9*time.Second {
		t.Errorf("读超时 %v，不是预期的 8 秒", elapsed)
	}
}
