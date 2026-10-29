package httpchan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 仿真的 NapCat（OneBot 11）本地网关。这组用例跑的是目录里那份真声明
// （ByID("napcat")），所以它同时在验 napcat.json。
//
// 着重验的是这份声明和别的渠道不一样的几处：
//
//   - 它是 local 渠道：明文 http + 回环地址，而把地址改成外网必须被拦住
//   - 读操作走 GET（OneBot 11 的 /:action 同时接受 GET 和 POST），所以
//     不需要 postReads 那个「方法不能作证」的逃生口
//   - 无论成败都回 HTTP 200，失败藏在 status 字段里 —— 不靠 failWhen 认
//     出来，一次「群号不存在」会被当成成功，模型就会据一个空结果编结论
//   - 发消息钉死 auto_escape：否则正文里的方括号会被当成 CQ 码解析
type napcatMock struct {
	srv *httptest.Server

	mu       sync.Mutex
	token    string
	gotAuth  string
	gotCalls []string // "METHOD /path"
	gotQuery map[string]string
	sentBody map[string]any
}

// ok 按 OneBot 11 的包装回成功：status/retcode/data 三件套。
func ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "ok", "retcode": 0, "data": data, "message": "", "wording": "",
	})
}

func newNapCatMock(t *testing.T, token string) *napcatMock {
	m := &napcatMock{token: token, gotQuery: map[string]string{}}
	mux := http.NewServeMux()

	record := func(w http.ResponseWriter, r *http.Request) bool {
		m.mu.Lock()
		m.gotAuth = r.Header.Get("Authorization")
		m.gotCalls = append(m.gotCalls, r.Method+" "+r.URL.Path)
		for k := range r.URL.Query() {
			m.gotQuery[k] = r.URL.Query().Get(k)
		}
		m.mu.Unlock()
		// NapCat 的鉴权：Authorization: Bearer <token>，不符合回 403。
		if r.Header.Get("Authorization") != "Bearer "+m.token {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"token verify failed!"}`))
			return false
		}
		return true
	}
	body := func(r *http.Request) map[string]any {
		raw, _ := io.ReadAll(r.Body)
		var v map[string]any
		json.Unmarshal(raw, &v)
		m.mu.Lock()
		m.sentBody = v
		m.mu.Unlock()
		return v
	}

	mux.HandleFunc("/get_login_info", func(w http.ResponseWriter, r *http.Request) {
		if !record(w, r) {
			return
		}
		ok(w, map[string]any{"user_id": 10000, "nickname": "叶工"})
	})

	mux.HandleFunc("/get_group_list", func(w http.ResponseWriter, r *http.Request) {
		if !record(w, r) {
			return
		}
		ok(w, []any{
			map[string]any{"group_id": 70001, "group_name": "技术交流群", "member_count": 231,
				"group_all_shut": 0, "group_remark": "", "max_member_count": 500},
			map[string]any{"group_id": 70002, "group_name": "家庭群", "member_count": 7,
				"group_all_shut": 0, "group_remark": "", "max_member_count": 200},
		})
	})

	mux.HandleFunc("/get_friend_list", func(w http.ResponseWriter, r *http.Request) {
		if !record(w, r) {
			return
		}
		ok(w, []any{
			map[string]any{"user_id": 20001, "nickname": "zhang", "remark": "老张",
				"phone_num": "13800000000", "email": "noise@example.com"},
		})
	})

	mux.HandleFunc("/get_group_msg_history", func(w http.ResponseWriter, r *http.Request) {
		if !record(w, r) {
			return
		}
		// 群号不存在：HTTP 仍是 200，失败在 status 里。
		if r.URL.Query().Get("group_id") == "999" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": "failed", "retcode": 200, "data": nil,
				"message": "消息999不存在", "wording": "消息999不存在",
			})
			return
		}
		ok(w, map[string]any{"messages": []any{
			map[string]any{
				"message_id": 4001, "time": 1762000000, "raw_message": "明天的会改到十点",
				"sender":  map[string]any{"user_id": 20001, "nickname": "zhang", "card": "老张"},
				"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "明天的会改到十点"}}},
				"font":    0, "self_id": 10000, "post_type": "message",
			},
			map[string]any{
				"message_id": 4002, "time": 1762000060, "raw_message": "收到",
				"sender":  map[string]any{"user_id": 10000, "nickname": "叶工", "card": ""},
				"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "收到"}}},
				"font":    0, "self_id": 10000, "post_type": "message",
			},
		}})
	})

	mux.HandleFunc("/get_friend_msg_history", func(w http.ResponseWriter, r *http.Request) {
		if !record(w, r) {
			return
		}
		ok(w, map[string]any{"messages": []any{
			map[string]any{
				"message_id": 5001, "time": 1762000100, "raw_message": "在吗",
				"sender": map[string]any{"user_id": 20001, "nickname": "zhang"},
			},
		}})
	})

	for _, p := range []string{"/send_group_msg", "/send_private_msg"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if !record(w, r) {
				return
			}
			body(r)
			ok(w, map[string]any{"message_id": 6001})
		})
	}

	m.srv = httptest.NewServer(mux) // 明文 http + 127.0.0.1，正是 local 渠道的形状
	t.Cleanup(m.srv.Close)
	return m
}

// runtime 按给定设置装配一个运行态。
func (m *napcatMock) runtime(t *testing.T, allowWrite bool) Runtime {
	t.Helper()
	spec, found := ByID("napcat")
	if !found {
		t.Fatal("目录里没有 napcat 声明")
	}
	settings := map[string]string{"endpoint": m.srv.URL}
	if allowWrite {
		settings[AllowWriteField] = "true"
	}
	return Runtime{
		Spec:     spec,
		Account:  "10000",
		Settings: settings,
		Secret:   func() (string, error) { return m.token, nil },
	}
}

func (m *napcatMock) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.gotCalls...)
}

func TestNapCatSpecShape(t *testing.T) {
	s, found := ByID("napcat")
	if !found {
		t.Fatal("目录里没有 napcat 声明")
	}
	if !s.Local {
		t.Error("napcat 必须声明 local：它连的是本机程序，页面要据此做披露")
	}
	// 读操作必须是 GET。这不是风格问题：readOnly 决定工具能不能绕过写开关，
	// 而对 GET 来说方法本身就是证据。OneBot 11 的 /:action 接受 GET，所以
	// 这份声明不该用 postReads 那个逃生口。
	if s.PostReads {
		t.Error("napcat 不该用 postReads——OneBot 的 /:action 接受 GET")
	}
	for _, tool := range s.Tools {
		if tool.ReadOnly && tool.Method != "GET" {
			t.Errorf("%s 是只读工具但方法是 %s", tool.Name, tool.Method)
		}
	}
	var writes []string
	for _, tool := range s.Tools {
		if !tool.ReadOnly {
			writes = append(writes, tool.Name)
		}
	}
	if got, want := strings.Join(writes, ","), "napcat_send_group_msg,napcat_send_private_msg"; got != want {
		t.Errorf("写工具 = %q，期望 %q", got, want)
	}
	for _, name := range []string{"napcat_whoami", "napcat_list_groups", "napcat_list_friends",
		"napcat_group_history", "napcat_friend_history"} {
		if _, has := s.Tool(name); !has {
			t.Errorf("缺少工具 %s", name)
		}
	}
	// 发消息必须钉死 auto_escape，而且不能把它开成参数 —— 一旦模型能自己
	// 关掉它，正文里的 [CQ:...] 就会被 NapCat 当成结构化元素解析。
	for _, name := range []string{"napcat_send_group_msg", "napcat_send_private_msg"} {
		tool, _ := s.Tool(name)
		if tool.BodyConst["auto_escape"] != true {
			t.Errorf("%s 必须在 bodyConst 里钉死 auto_escape=true，现在是 %v",
				name, tool.BodyConst["auto_escape"])
		}
		for _, p := range tool.Params {
			if p.Name == "auto_escape" {
				t.Errorf("%s 不能把 auto_escape 开成参数", name)
			}
		}
	}
}

// 读操作走 GET、凭据按 Bearer 注入、响应按声明裁剪。
func TestNapCatReadsUseGET(t *testing.T) {
	m := newNapCatMock(t, "s3cr3t")
	rt := m.runtime(t, false)
	c := new(Client)
	ctx := context.Background()

	if _, err := c.Test(ctx, rt); err != nil {
		t.Fatalf("测试连通失败: %v", err)
	}
	if m.gotAuth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q，期望 Bearer s3cr3t", m.gotAuth)
	}

	groups, _, err := c.Call(ctx, rt, "napcat_list_groups", nil)
	if err != nil {
		t.Fatalf("取群列表失败: %v", err)
	}
	list, isArr := groups.([]any)
	if !isArr || len(list) != 2 {
		t.Fatalf("群列表 = %#v，期望 2 条", groups)
	}
	first, _ := list[0].(map[string]any)
	if first["group_name"] != "技术交流群" {
		t.Errorf("第一个群 = %#v", first)
	}
	// 裁剪要真的生效：group_all_shut 这类噪音字段不该进模型上下文。
	if _, leaked := first["group_all_shut"]; leaked {
		t.Errorf("未声明的字段漏进了结果: %#v", first)
	}

	msgs, _, err := c.Call(ctx, rt, "napcat_group_history",
		map[string]any{"group_id": "70001", "count": float64(2)})
	if err != nil {
		t.Fatalf("取群记录失败: %v", err)
	}
	rows, isArr := msgs.([]any)
	if !isArr || len(rows) != 2 {
		t.Fatalf("群记录 = %#v，期望 2 条", msgs)
	}
	// data.messages 这种点号路径要能取到，嵌套的 sender.* 也要被摊平。
	row, _ := rows[0].(map[string]any)
	if row["raw_message"] != "明天的会改到十点" {
		t.Errorf("第一条消息 = %#v", row)
	}
	if row["sender.nickname"] != "zhang" || row["sender.card"] != "老张" {
		t.Errorf("发言人字段没取到: %#v", row)
	}

	// 数字参数要以字符串形式进 query —— GET 的 query 全是字符串，NapCat
	// 那边靠 schema 的 Convert 转回 number。
	if got := m.gotQuery["count"]; got != "2" {
		t.Errorf("count 进 query = %q，期望 \"2\"", got)
	}
	if got := m.gotQuery["group_id"]; got != "70001" {
		t.Errorf("group_id 进 query = %q", got)
	}

	for _, call := range m.calls() {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("读操作走了非 GET: %s", call)
		}
	}
}

// HTTP 200 + status:failed 必须被认成失败，并把 message 带出来。
//
// 这是这份声明里最容易被漏掉的一条：不声明 failWhen，一次「群号不存在」
// 会变成一个空结果，模型会据它编出「那个群最近没人说话」。
func TestNapCatBusinessFailureIsReported(t *testing.T) {
	m := newNapCatMock(t, "s3cr3t")
	rt := m.runtime(t, false)

	_, _, err := new(Client).Call(context.Background(), rt, "napcat_group_history",
		map[string]any{"group_id": "999"})
	if err == nil {
		t.Fatal("群号不存在时应当报错，而不是回一个空结果")
	}
	if !strings.Contains(err.Error(), "消息999不存在") {
		t.Errorf("报错里没带上 NapCat 的说明: %v", err)
	}
}

// 写开关关着时，写工具在发出请求之前就被拒。
func TestNapCatWriteNeedsSwitch(t *testing.T) {
	m := newNapCatMock(t, "s3cr3t")
	c := new(Client)
	ctx := context.Background()

	_, _, err := c.Call(ctx, m.runtime(t, false), "napcat_send_group_msg",
		map[string]any{"group_id": "70001", "message": "hi"})
	if err == nil {
		t.Fatal("写开关关着时发消息应当被拒")
	}
	if len(m.calls()) != 0 {
		t.Errorf("被拒的调用不该发出请求，实际发了: %v", m.calls())
	}

	// 打开之后能发，而且 auto_escape 被钉在请求体里。
	res, _, err := c.Call(ctx, m.runtime(t, true), "napcat_send_group_msg",
		map[string]any{"group_id": "70001", "message": "明天的会改到十点 [CQ:at,qq=all]"})
	if err != nil {
		t.Fatalf("发群消息失败: %v", err)
	}
	if got, _ := res.(map[string]any); got["message_id"] != float64(6001) {
		t.Errorf("返回 = %#v，期望带 message_id", res)
	}
	m.mu.Lock()
	sent := m.sentBody
	m.mu.Unlock()
	if sent["auto_escape"] != true {
		t.Errorf("请求体里 auto_escape = %v，必须是 true（否则正文按 CQ 码解析）", sent["auto_escape"])
	}
	if sent["message"] != "明天的会改到十点 [CQ:at,qq=all]" {
		t.Errorf("正文被改动了: %#v", sent["message"])
	}
	if sent["group_id"] != "70001" {
		t.Errorf("group_id = %#v", sent["group_id"])
	}
}

// 地址被手改成外网主机时必须拒绝发请求。
//
// channels.json 是可手改的，而这个渠道走明文 http —— 校验只在保存时做一次
// 是不够的，否则改一行配置就能让令牌明文发到任意主机。
func TestNapCatEndpointMustStayLoopback(t *testing.T) {
	spec, found := ByID("napcat")
	if !found {
		t.Fatal("目录里没有 napcat 声明")
	}
	bad := []string{
		"http://evil.example.com:3000",
		"https://evil.example.com",
		"http://localhost:3000", // 域名要靠 DNS 解析，解析到哪里不由这里决定
		"http://10.0.0.5:3000",
		"http://127.0.0.1.evil.com:3000",
	}
	for _, raw := range bad {
		rt := Runtime{Spec: spec, Settings: map[string]string{"endpoint": raw}}
		if _, err := rt.BaseURL(); err == nil {
			t.Errorf("endpoint=%q 应当被拒", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:3000", "http://127.0.0.2:5700", "http://[::1]:3000"} {
		rt := Runtime{Spec: spec, Settings: map[string]string{"endpoint": raw}}
		if _, err := rt.BaseURL(); err != nil {
			t.Errorf("endpoint=%q 应当被接受: %v", raw, err)
		}
	}
}

// 令牌不对时 NapCat 回 403，报错要能让人据以行动。
func TestNapCatWrongTokenIsActionable(t *testing.T) {
	m := newNapCatMock(t, "s3cr3t")
	rt := m.runtime(t, false)
	rt.Secret = func() (string, error) { return "wrong", nil }

	_, err := new(Client).Test(context.Background(), rt)
	if err == nil {
		t.Fatal("令牌不对时应当报错")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("报错里应当带上状态码: %v", err)
	}
	// 错误信息里不能回显令牌本身。
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("报错里泄漏了令牌: %v", err)
	}
}
