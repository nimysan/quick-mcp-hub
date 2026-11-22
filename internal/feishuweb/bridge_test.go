package feishuweb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testToken = "fw-test-token"

func newBridgeServer(t *testing.T) (*Bridge, *httptest.Server) {
	t.Helper()
	b := &Bridge{Token: func() (string, error) { return testToken, nil }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ext/ws", b.HandleWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return b, srv
}

func dial(ctx context.Context, srv *httptest.Server, origin string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ext/ws", &websocket.DialOptions{HTTPHeader: h})
}

func TestBridgeRejectsWrongOrigin(t *testing.T) {
	_, srv := newBridgeServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, o := range []string{"", "https://evil.example", "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		c, resp, err := dial(ctx, srv, o)
		if err == nil {
			c.CloseNow()
			t.Errorf("Origin %q 不该连得上", o)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: 期望 403，得到 %v", o, resp)
		}
	}
}

func TestBridgeRejectsWrongToken(t *testing.T) {
	b, srv := newBridgeServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := dial(ctx, srv, ExtensionOrigin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","token":"wrong"}`))
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("期望 1008 关闭，得到 %v", err)
	}
	if b.Connected() {
		t.Error("错 token 不该算连上")
	}
}

// 完整走一遍：扩展连上 -> 交任务 -> 扩展回块树 -> 拿到 Markdown 和自检数据。
func TestBridgeRoundTrip(t *testing.T) {
	b, srv := newBridgeServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := b.Do(ctx, JobReadDoc, "https://x.feishu.cn/docx/AbCdEf123456"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("没连上时应当报 ErrNotConnected，得到 %v", err)
	}

	c, _, err := dial(ctx, srv, ExtensionOrigin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","token":"`+testToken+`","version":"0.1.0"}`))
	if _, data, err := c.Read(ctx); err != nil || !strings.Contains(string(data), "welcome") {
		t.Fatalf("没收到 welcome: %s %v", data, err)
	}

	// 假扩展：收到任务就回一份块树。
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m wireMsg
			_ = json.Unmarshal(data, &m)
			if m.Type != "job" {
				continue
			}
			res := RawResult{
				Status: StatusOK, Title: "标题\u200b",
				Flags: &Flags{Copyable: bp(true), Exportable: bp(true), IsForceSetSecurity: bp(false)},
				Diag:  &SelfCheck{PageMain: true, TopChildren: 1, ScrollRounds: 2},
				Root:  &Block{Type: "page", Children: []*Block{{Type: "text", Ops: []Op{{Insert: "正文"}}}}},
			}
			out, _ := json.Marshal(wireMsg{Type: "result", ID: m.ID, Result: &res})
			_ = c.Write(ctx, websocket.MessageText, out)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !b.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	res, err := b.Do(ctx, JobReadDoc, "https://x.feishu.cn/docx/AbCdEf123456?from=x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || res.Markdown != "正文" || res.Title != "标题" || res.Notice == "" {
		t.Fatalf("结果不对：%+v", res)
	}
	st := b.Status()
	if !st.Connected || st.FirstCheck == nil || st.FirstCheck.ScrollRounds != 2 {
		t.Errorf("自检没记下来：%+v", st)
	}
}
