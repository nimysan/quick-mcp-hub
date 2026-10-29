package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// 这组用例把 napcat 声明放到**真的 MCP 协议**上跑一遍：tools/list 看得见
// 什么、tools/call 真的打到哪里。目录里那份声明直接拿来用，所以它同时在
// 验 napcat.json 经由注册层之后的样子。
//
// 刻意不碰系统钥匙串：Runtime 自带 Secret 函数，所以这里不需要为一次验证
// 在用户的钥匙串里留条目。

// fakeOneBot 是一个最小的 OneBot 11 网关：明文 http + 回环地址。
func fakeOneBot(t *testing.T, token string, seen *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, r *http.Request, data any) {
		*seen = append(*seen, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "retcode": 0, "data": data, "message": "",
		})
	}
	mux.HandleFunc("/get_login_info", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, map[string]any{"user_id": 10000, "nickname": "叶工"})
	})
	mux.HandleFunc("/get_group_list", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, []any{map[string]any{
			"group_id": 70001, "group_name": "技术交流群", "member_count": 231,
		}})
	})
	mux.HandleFunc("/send_group_msg", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, map[string]any{"message_id": 6001})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func napcatRuntime(t *testing.T, endpoint, token, account string, allowWrite bool) httpchan.Runtime {
	t.Helper()
	spec, ok := httpchan.ByID("napcat")
	if !ok {
		t.Fatal("目录里没有 napcat 声明")
	}
	settings := map[string]string{"endpoint": endpoint}
	if allowWrite {
		settings[httpchan.AllowWriteField] = "true"
	}
	return httpchan.Runtime{
		Spec: spec, Account: account, Settings: settings,
		Secret: func() (string, error) { return token, nil },
	}
}

// 写开关关着：7 个工具里只注册 5 个只读的，两个发消息的根本不出现在
// 模型眼前 —— 不是调用时被拒，是看不到。
func TestNapCatWriteToolsWithheldFromMCP(t *testing.T) {
	var seen []string
	srv := fakeOneBot(t, "s3cr3t", &seen)
	server := newServer()

	n := RegisterHTTP(server, []httpchan.Runtime{
		napcatRuntime(t, srv.URL, "s3cr3t", "10000", false),
	})
	if n != 5 {
		t.Errorf("注册了 %d 个工具，期望 5 个（写开关关着）", n)
	}
	names := listNames(t, connect(t, server))
	for _, name := range names {
		if strings.HasSuffix(name, "_send_group_msg") || strings.HasSuffix(name, "_send_private_msg") {
			t.Errorf("写开关关着却注册了 %s", name)
		}
	}
	for _, want := range []string{"napcat_whoami", "napcat_list_groups", "napcat_group_history"} {
		if !contains(names, want) {
			t.Errorf("缺少只读工具 %s，实际：%v", want, names)
		}
	}
}

// 写开关打开：7 个都注册，而且 tools/call 真的打到了本地网关。
func TestNapCatToolsReachMCP(t *testing.T) {
	var seen []string
	srv := fakeOneBot(t, "s3cr3t", &seen)
	server := newServer()

	if n := RegisterHTTP(server, []httpchan.Runtime{
		napcatRuntime(t, srv.URL, "s3cr3t", "10000", true),
	}); n != 7 {
		t.Fatalf("注册了 %d 个工具，期望 7 个", n)
	}
	cs := connect(t, server)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "napcat_list_groups",
	})
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("调用回了错误: %+v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), "技术交流群") {
		t.Errorf("结果里没有群名: %s", raw)
	}

	// 发消息也走通，并且确实是 POST。
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "napcat_send_group_msg",
		Arguments: map[string]any{"group_id": "70001", "message": "明天的会改到十点"},
	}); err != nil {
		t.Fatalf("发群消息失败: %v", err)
	}
	if !contains(seen, "GET /get_group_list") {
		t.Errorf("取群列表不是 GET：%v", seen)
	}
	if !contains(seen, "POST /send_group_msg") {
		t.Errorf("发消息不是 POST：%v", seen)
	}
}

// 地址被手改成外网主机时，工具仍然注册（声明没变），但调用必须失败 ——
// 失败点在发请求之前，所以那台主机连一个字节都收不到。
func TestNapCatTamperedEndpointFailsAtCall(t *testing.T) {
	server := newServer()
	if n := RegisterHTTP(server, []httpchan.Runtime{
		napcatRuntime(t, "http://evil.example.com:3000", "s3cr3t", "10000", false),
	}); n != 5 {
		t.Fatalf("注册了 %d 个工具，期望 5 个", n)
	}
	res, err := connect(t, server).CallTool(context.Background(), &mcp.CallToolParams{
		Name: "napcat_whoami",
	})
	if err == nil && !res.IsError {
		t.Fatal("地址指向外网主机时调用应当失败")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
