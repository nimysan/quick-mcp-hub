package httpchan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 用本地 httptest 跑执行器：不碰网络，也能把凭据注入、参数分派、
// 响应裁剪、写开关这几件事都验到。
func TestCallBuildsRequestAndShapesResult(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotAccept, gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 用 EscapedPath：r.URL.Path 是解码后的，看不出转义有没有做对。
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		b := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			r.Body.Read(b)
		}
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[
			{"id":1,"title":"一","user":{"login":"a"},"noise":"丢掉"},
			{"id":2,"title":"二","user":{"login":"b"},"noise":"丢掉"},
			{"id":3,"title":"三","user":{"login":"c"},"noise":"丢掉"}
		]}`))
	}))
	defer srv.Close()

	spec := base()
	spec.BaseURL = "https://api.example.com" // 校验用；实际请求打到 httptest
	spec.Headers = map[string]string{"X-Api-Version": "2026-01-01"}
	spec.Tools = []Tool{{
		Name: "demo_list", Description: "列一下", ReadOnly: true,
		Method: "GET", Path: "/repos/{owner}/items",
		Params: []Param{
			{Name: "owner", In: InPath, Required: true},
			{Name: "state", In: InQuery, Default: "open"},
			{Name: "per_page", In: InQuery, Type: "number", Default: "20"},
		},
		Result: Result{Path: "items", Limit: 2, Fields: []string{"id", "title", "user.login"}},
	}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("声明不合法: %v", err)
	}

	rt := Runtime{
		Spec: spec, Account: "me",
		Settings: map[string]string{},
		Secret:   func() (string, error) { return "tok-123456", nil },
	}
	rt.Spec.BaseURL = srv.URL // httptest 是 https，符合 https 约束
	c := &Client{HTTP: srv.Client()}

	out, _, err := c.Call(context.Background(), rt, "demo_list", map[string]any{"owner": "oct o"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	if gotPath != "/repos/oct%20o/items" {
		t.Errorf("路径参数未正确转义: %q", gotPath)
	}
	if !strings.Contains(gotQuery, "state=open") || !strings.Contains(gotQuery, "per_page=20") {
		t.Errorf("默认值未进 query: %q", gotQuery)
	}
	if gotAuth != "Bearer tok-123456" {
		t.Errorf("凭据未按 bearer 注入: %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept 头不对: %q", gotAccept)
	}
	if gotBody != "" {
		t.Errorf("GET 不该带 body: %q", gotBody)
	}

	items, ok := out.([]any)
	if !ok {
		t.Fatalf("结果应当是数组，实际 %T", out)
	}
	if len(items) != 2 {
		t.Fatalf("Limit=2 应当只留 2 条，实际 %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["user.login"] != "a" || first["title"] != "一" {
		t.Errorf("字段裁剪结果不对: %v", first)
	}
	if _, leaked := first["noise"]; leaked {
		t.Error("未声明的字段漏了出来")
	}
}

func TestCallWriteToolNeedsSwitch(t *testing.T) {
	spec := base()
	spec.Tools = append(spec.Tools, Tool{
		Name: "demo_create", Description: "建一条",
		Method: "POST", Path: "/items",
		Params: []Param{{Name: "title", In: InBody, Required: true}},
	})
	if err := spec.Validate(); err != nil {
		t.Fatalf("声明不合法: %v", err)
	}

	called := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":7,"title":"新"}`))
	}))
	defer srv.Close()
	spec.BaseURL = srv.URL

	rt := Runtime{Spec: spec, Account: "me", Secret: func() (string, error) { return "tok-123456", nil }}
	c := &Client{HTTP: srv.Client()}

	// 写开关关着：必须在发请求之前就拒绝。
	_, _, err := c.Call(context.Background(), rt, "demo_create", map[string]any{"title": "新"})
	if err == nil || !strings.Contains(err.Error(), "写开关") {
		t.Fatalf("写开关关着时应当拒绝，实际: %v", err)
	}
	if called {
		t.Fatal("被拒绝的调用不该真的发出请求")
	}

	rt.Settings = map[string]string{AllowWriteField: "true"}
	if _, _, err := c.Call(context.Background(), rt, "demo_create", map[string]any{"title": "新"}); err != nil {
		t.Fatalf("打开写开关后应当放行: %v", err)
	}
	if !called {
		t.Fatal("打开写开关后请求没发出去")
	}
}

func TestCallRequiredParamAndErrorBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	spec := base()
	spec.BaseURL = srv.URL
	rt := Runtime{Spec: spec, Account: "me", Secret: func() (string, error) { return "tok-123456", nil }}
	c := &Client{HTTP: srv.Client()}

	if _, _, err := c.Call(context.Background(), rt, "demo_get", nil); err == nil ||
		!strings.Contains(err.Error(), "必填") {
		t.Fatalf("缺必填参数应当报错，实际: %v", err)
	}

	_, _, err := c.Call(context.Background(), rt, "demo_get", map[string]any{"id": 5})
	if err == nil {
		t.Fatal("401 应当是错误")
	}
	// 错误信息要带上对方的说法，否则用户只知道「失败了」。
	if !strings.Contains(err.Error(), "Bad credentials") || !strings.Contains(err.Error(), "401") {
		t.Errorf("错误信息不够具体: %v", err)
	}
	if strings.Contains(err.Error(), "tok-123456") {
		t.Error("错误信息里泄漏了凭据")
	}
}

// 用户自己填的站点地址同样必须是 https —— 配置文件可手改，不能信任落盘时校过。
func TestBaseURLFieldMustBeHTTPS(t *testing.T) {
	spec := base()
	spec.BaseURL = ""
	spec.BaseURLField = "site"
	spec.Fields = []Field{{Name: "site", Label: "站点地址", Required: true}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("声明应当合法: %v", err)
	}

	rt := Runtime{Spec: spec, Settings: map[string]string{"site": "http://intranet.example.com"}}
	if _, err := rt.BaseURL(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("明文 http 站点应当被拒，实际: %v", err)
	}

	rt.Settings["site"] = "https://jira.example.com/"
	got, err := rt.BaseURL()
	if err != nil {
		t.Fatalf("合法站点被拒: %v", err)
	}
	if got != "https://jira.example.com" {
		t.Errorf("结尾斜杠没去掉: %q", got)
	}
}

// 路径参数里的 / 必须被转义：否则模型给一个 "../../user" 就能把请求
// 打到声明之外的接口上。
func TestPathParamCannotTraverse(t *testing.T) {
	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	spec := base()
	spec.BaseURL = srv.URL
	rt := Runtime{Spec: spec, Account: "me", Secret: func() (string, error) { return "tok-123456", nil }}
	c := &Client{HTTP: srv.Client()}

	if _, _, err := c.Call(context.Background(), rt, "demo_get", map[string]any{"id": "../../user"}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 关键是斜杠被编码成 %2F：路径层级没有多出来，".." 只是个普通段。
	if strings.Count(gotPath, "/") != 2 || !strings.HasPrefix(gotPath, "/items/") {
		t.Fatalf("路径参数逃出了声明的路径: %q", gotPath)
	}
}

func TestShapeKeepsScalarAndObject(t *testing.T) {
	var v any
	json.Unmarshal([]byte(`{"login":"nimysan","name":"叶","noise":1}`), &v)
	out, _, err := shape(v, Result{Fields: []string{"login", "name"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["login"] != "nimysan" || m["name"] != "叶" {
		t.Errorf("对象裁剪不对: %v", m)
	}
	if _, ok := m["noise"]; ok {
		t.Error("未声明字段漏出")
	}
}

// Trace 要能区分「键不存在」和「值是 null」。
//
// 混成一类的后果是诊断对正常数据报警：一条没有负责人的 Jira 事项会被报成
// 「声明的字段不存在」，作者很快就会学会无视这个诊断。
func TestProbePathDistinguishesMissingFromNull(t *testing.T) {
	var v any
	json.Unmarshal([]byte(`{"fields":{"summary":"标题","assignee":null,"status":{"name":"Done"}}}`), &v)

	cases := []struct {
		path   string
		ok     bool
		reason string
	}{
		{"fields.summary", true, ""},
		{"fields.status.name", true, ""},
		{"fields.assignee.displayName", false, "fields.assignee 是 null"},
		{"fields.priority.name", false, "没有键 fields.priority"},
		{"fields.summary.nope", false, "fields.summary 不是对象"},
		{"nope", false, "没有键 nope"},
	}
	for _, c := range cases {
		ok, reason := probePath(v, c.path)
		if ok != c.ok {
			t.Errorf("%s: ok=%v，期望 %v（%s）", c.path, ok, c.ok, reason)
			continue
		}
		if reason != c.reason {
			t.Errorf("%s: 原因是 %q，期望 %q", c.path, reason, c.reason)
		}
	}
}

// Trace 在真实调用里把三类情况分开记：取到的、键不存在的、值为空的。
func TestTraceRecordsFieldDiagnostics(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":1,"values":[{"key":"A-1","fields":{"summary":"有","assignee":null}}]}`))
	}))
	defer srv.Close()

	spec := base()
	spec.BaseURL = srv.URL
	spec.Tools = []Tool{{
		Name: "demo_list", Description: "列一下", ReadOnly: true,
		Method: "GET", Path: "/items",
		Result: Result{
			Path:   "values",
			Fields: []string{"key", "fields.summary", "fields.assignee.displayName", "fields.priority.name"},
			Meta:   []string{"total", "nextPageToken"},
		},
	}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("声明不合法: %v", err)
	}

	rt := Runtime{Spec: spec, Account: "me", Secret: func() (string, error) { return "tok-123456", nil }}
	c := &Client{HTTP: srv.Client(), Trace: &Trace{}}
	if _, _, err := c.Call(context.Background(), rt, "demo_list", nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	tr := c.Trace
	if tr.Items != 1 {
		t.Errorf("条数不对: %d", tr.Items)
	}
	if len(tr.Missing) != 1 || !strings.Contains(tr.Missing[0], "fields.priority") {
		t.Errorf("键不存在的应当只有 priority: %v", tr.Missing)
	}
	if len(tr.Nulls) != 1 || !strings.Contains(tr.Nulls[0], "assignee") {
		t.Errorf("值为空的应当只有 assignee: %v", tr.Nulls)
	}
	if len(tr.MetaFound) != 1 || tr.MetaFound[0] != "total" {
		t.Errorf("meta 取到的不对: %v", tr.MetaFound)
	}
	if len(tr.MetaMissing) != 1 || tr.MetaMissing[0] != "nextPageToken" {
		t.Errorf("meta 没取到的不对: %v", tr.MetaMissing)
	}
}

// 整个子树都取不到时要直接点出来，而不是逐个字段报错。
func TestTraceReportsMissingResultPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"somethingElse":[]}`))
	}))
	defer srv.Close()

	spec := base()
	spec.BaseURL = srv.URL
	spec.Tools = []Tool{{
		Name: "demo_list", Description: "列一下", ReadOnly: true,
		Method: "GET", Path: "/items",
		Result: Result{Path: "values", Fields: []string{"key"}},
	}}
	rt := Runtime{Spec: spec, Account: "me", Secret: func() (string, error) { return "tok-123456", nil }}
	c := &Client{HTTP: srv.Client(), Trace: &Trace{}}

	if _, _, err := c.Call(context.Background(), rt, "demo_list", nil); err == nil {
		t.Fatal("取不到声明的 result.path 应当是错误")
	}
	if len(c.Trace.Missing) != 1 || !strings.Contains(c.Trace.Missing[0], "整个子树") {
		t.Errorf("应当点出整个子树取不到: %v", c.Trace.Missing)
	}
}
