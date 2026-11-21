package httpchan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nimysan/mcp-hub/internal/oauthdev"
)

// 仿真的百度网盘开放接口。用例跑的是目录里那份真声明（ByID("baidupan")）。
//
// 钉住的几件事：
//   - 凭据是查询参数 access_token，而凭据槽里装的可能是令牌组 JSON、也可能是
//     用户粘来的整个授权后地址 —— 两者都必须变成裸令牌再发出去
//   - method=list 这类写死在 path 里的查询串要和参数合并，不能出两个 ?
//   - 文件管理只收表单，filelist 是一段 JSON 文本
//   - HTTP 200 + errno 非 0 是失败
type panMock struct {
	srv *httptest.Server

	mu       sync.Mutex
	gotToken string
	gotUA    string
	gotQuery url.Values
	gotCT    string
	gotForm  url.Values
	calls    []string
}

func newPanMock(t *testing.T) *panMock {
	t.Helper()
	m := &panMock{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		q := r.URL.Query()
		m.gotToken = q.Get("access_token")
		m.gotUA = r.Header.Get("User-Agent")
		m.gotQuery = q
		m.gotCT = r.Header.Get("Content-Type")
		m.gotForm = nil
		if r.Method == http.MethodPost {
			r.ParseForm()
			m.gotForm = r.PostForm
		}
		m.calls = append(m.calls, r.Method+" "+r.URL.Path+"?method="+q.Get("method")+"&opera="+q.Get("opera"))
	}
	write := func(w http.ResponseWriter, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("access_token") != "tok-1" {
			write(w, map[string]any{"errno": -6, "errmsg": "身份验证失败", "request_id": 1})
			return false
		}
		return true
	}
	mux.HandleFunc("/rest/2.0/xpan/nas", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !auth(w, r) {
			return
		}
		write(w, map[string]any{"errno": 0, "errmsg": "succ", "baidu_name": "pan_user", "netdisk_name": "网盘昵称",
			"uk": 4242, "vip_type": 2, "avatar_url": "https://example.invalid/a.png", "request_id": 1})
	})
	mux.HandleFunc("/api/quota", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !auth(w, r) {
			return
		}
		write(w, map[string]any{"errno": 0, "total": 2205465706496, "used": 686653888910, "free": 2205465706496, "expire": false})
	})
	mux.HandleFunc("/rest/2.0/xpan/file", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !auth(w, r) {
			return
		}
		switch r.URL.Query().Get("method") {
		case "list":
			if r.URL.Query().Get("dir") == "/不存在" {
				write(w, map[string]any{"errno": -9, "request_id": 2})
				return
			}
			write(w, map[string]any{"errno": 0, "guid_info": "", "list": []any{
				map[string]any{"fs_id": 1001, "path": "/工作/合同.pdf", "server_filename": "合同.pdf", "size": 2048,
					"isdir": 0, "server_mtime": 1763600000, "category": 4, "md5": "noise", "thumbs": map[string]any{"url1": "x"}},
			}, "request_id": 3, "guid": 0})
		case "search":
			write(w, map[string]any{"errno": 0, "has_more": 1, "contentlist": []any{}, "list": []any{
				map[string]any{"fs_id": 1001, "path": "/工作/合同.pdf", "server_filename": "合同.pdf", "size": 2048, "isdir": 0},
			}})
		case "create":
			write(w, map[string]any{"errno": 0, "fs_id": 2002, "path": r.PostForm.Get("path"), "isdir": 1, "ctime": 1763600001})
		case "filemanager":
			write(w, map[string]any{"errno": 0, "info": []any{map[string]any{"errno": 0, "path": "/工作/草稿.docx"}}, "request_id": 4})
		default:
			write(w, map[string]any{"errno": 2, "errmsg": "param error"})
		}
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *panMock) runtime(t *testing.T, secret string, allowWrite bool) Runtime {
	t.Helper()
	spec, found := ByID("baidupan")
	if !found {
		t.Fatal("目录里没有 baidupan 声明")
	}
	spec.BaseURL = m.srv.URL
	settings := map[string]string{}
	if allowWrite {
		settings[AllowWriteField] = "true"
	}
	return Runtime{Spec: spec, Account: "pan_user", Settings: settings,
		Secret: func() (string, error) { return secret, nil }}
}

func (m *panMock) snapshot() (string, string, url.Values, string, url.Values) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotToken, m.gotUA, m.gotQuery, m.gotCT, m.gotForm
}

func tokenSetJSON(t *testing.T, tok oauthdev.TokenSet) string {
	t.Helper()
	tok.Marker = oauthdev.TokenMarker
	s, err := tok.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBaidupanSpecShape(t *testing.T) {
	spec, found := ByID("baidupan")
	if !found {
		t.Fatalf("目录里没有 baidupan 声明（加载错误: %v）", Errors())
	}
	if spec.Auth.Kind != AuthQuery || spec.Auth.Name != "access_token" {
		t.Fatalf("百度网盘的凭据是查询参数 access_token: %+v", spec.Auth)
	}
	if spec.OAuth == nil || spec.OAuth.Flow != FlowImplicit {
		t.Fatal("百度网盘走隐式授权")
	}
	if spec.PostReads {
		t.Error("读操作都是 GET，不该声明 postReads")
	}
	writes := map[string]bool{}
	for _, tl := range spec.Tools {
		if !tl.ReadOnly {
			writes[tl.Name] = true
			if tl.BodyEncoding != BodyForm {
				t.Errorf("%s: 百度的文件操作只收表单", tl.Name)
			}
		} else if tl.Method != "GET" {
			t.Errorf("%s: 只读工具必须是 GET", tl.Name)
		}
	}
	for _, name := range []string{"baidupan_mkdir", "baidupan_rename", "baidupan_move", "baidupan_copy", "baidupan_delete"} {
		if !writes[name] {
			t.Errorf("%s 必须是写工具（受写开关控制）", name)
		}
	}
	if len(writes) != 5 {
		t.Errorf("写工具应当正好 5 个，现在 %v", writes)
	}
}

// 「连接」拿到的令牌组和手动粘来的授权后地址，发出去都必须是裸令牌。
func TestBaidupanTokenInQuery(t *testing.T) {
	m := newPanMock(t)
	c := &Client{}
	secrets := map[string]string{
		"令牌组":    tokenSetJSON(t, oauthdev.TokenSet{AccessToken: "tok-1", ExpiresAt: time.Now().Add(24 * time.Hour)}),
		"粘来的地址":  "https://openapi.baidu.com/oauth/2.0/login_success#expires_in=2592000&access_token=tok-1&state=x",
		"手填的裸令牌": "tok-1",
	}
	for name, secret := range secrets {
		got, err := c.Test(context.Background(), m.runtime(t, secret, false))
		if err != nil {
			t.Fatalf("%s: 测试连通失败: %v", name, err)
		}
		tok, ua, q, _, _ := m.snapshot()
		if tok != "tok-1" {
			t.Fatalf("%s: 发出去的 access_token = %q", name, tok)
		}
		if ua != "pan.baidu.com" {
			t.Errorf("%s: User-Agent = %q", name, ua)
		}
		if q.Get("method") != "uinfo" {
			t.Errorf("%s: 写死在 path 里的 method=uinfo 丢了: %v", name, q)
		}
		if pickIdentityName(got) != "pan_user" {
			t.Errorf("%s: 身份结果里要有 baidu_name: %#v", name, got)
		}
	}
}

func pickIdentityName(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["baidu_name"].(string)
	return s
}

// 过期的隐式授权没有刷新令牌，要明确告诉用户去重新授权，而不是带着旧令牌去撞。
func TestBaidupanExpiredTokenAsksReauth(t *testing.T) {
	m := newPanMock(t)
	expired := tokenSetJSON(t, oauthdev.TokenSet{AccessToken: "tok-1", ExpiresAt: time.Now().Add(-time.Hour)})
	_, _, err := (&Client{}).Call(context.Background(), m.runtime(t, expired, false), "baidupan_whoami", nil)
	if err == nil || !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("过期令牌应当提示重新授权: %v", err)
	}
	m.mu.Lock()
	n := len(m.calls)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("令牌过期了就不该再发请求，现在发了 %d 次", n)
	}
}

func TestBaidupanListMergesFixedQuery(t *testing.T) {
	m := newPanMock(t)
	out, _, err := (&Client{}).Call(context.Background(), m.runtime(t, "tok-1", false), "baidupan_list_files",
		map[string]any{"dir": "/工作", "limit": float64(50)})
	if err != nil {
		t.Fatal(err)
	}
	_, _, q, _, _ := m.snapshot()
	if q.Get("method") != "list" || q.Get("dir") != "/工作" || q.Get("limit") != "50" || q.Get("order") != "time" {
		t.Fatalf("查询参数不对: %v", q)
	}
	items, ok := out.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("结果: %#v", out)
	}
	first := items[0].(map[string]any)
	if first["path"] != "/工作/合同.pdf" || first["fs_id"] == nil {
		t.Fatalf("字段不对: %#v", first)
	}
	if _, noisy := first["thumbs"]; noisy {
		t.Error("缩略图这类噪音应当裁掉")
	}
}

func TestBaidupanSearchCarriesHasMore(t *testing.T) {
	m := newPanMock(t)
	_, meta, err := (&Client{}).Call(context.Background(), m.runtime(t, "tok-1", false), "baidupan_search",
		map[string]any{"key": "合同"})
	if err != nil {
		t.Fatal(err)
	}
	if meta["has_more"] != float64(1) {
		t.Fatalf("has_more 要带给模型，否则它不知道还能翻页: %v", meta)
	}
}

// errno 非 0 是业务失败。
func TestBaidupanErrnoIsFailure(t *testing.T) {
	m := newPanMock(t)
	c := &Client{}
	if _, _, err := c.Call(context.Background(), m.runtime(t, "tok-1", false), "baidupan_list_files",
		map[string]any{"dir": "/不存在"}); err == nil || !strings.Contains(err.Error(), "-9") {
		t.Fatalf("errno -9 应当报错并带出错误码: %v", err)
	}
	if _, err := c.Test(context.Background(), m.runtime(t, "wrong", false)); err == nil || !strings.Contains(err.Error(), "身份验证失败") {
		t.Fatalf("令牌不对应当报出百度的说明: %v", err)
	}
}

func TestBaidupanWritesNeedSwitch(t *testing.T) {
	m := newPanMock(t)
	args := map[string]any{"filelist": []any{map[string]any{"path": "/工作/草稿.docx", "newname": "终稿.docx"}}}
	if _, _, err := (&Client{}).Call(context.Background(), m.runtime(t, "tok-1", false), "baidupan_rename", args); err == nil {
		t.Fatal("写开关关着时重命名必须被挡住")
	}
	m.mu.Lock()
	n := len(m.calls)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("被挡住的调用不该发出请求，现在发了 %d 次", n)
	}
}

// 文件管理走表单，filelist 是 JSON 文本，async / ondup 由声明钉死。
func TestBaidupanRenameIsFormWithJSONList(t *testing.T) {
	m := newPanMock(t)
	args := map[string]any{"filelist": []any{map[string]any{"path": "/工作/草稿.docx", "newname": "终稿.docx"}}}
	if _, _, err := (&Client{}).Call(context.Background(), m.runtime(t, "tok-1", true), "baidupan_rename", args); err != nil {
		t.Fatal(err)
	}
	tok, _, q, ct, form := m.snapshot()
	if tok != "tok-1" || q.Get("method") != "filemanager" || q.Get("opera") != "rename" {
		t.Fatalf("查询串不对: %v", q)
	}
	if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if form.Get("async") != "1" || form.Get("ondup") != "fail" {
		t.Fatalf("钉死的表单项不对: %v", form)
	}
	var list []map[string]string
	if err := json.Unmarshal([]byte(form.Get("filelist")), &list); err != nil {
		t.Fatalf("filelist 应当是 JSON 文本: %q (%v)", form.Get("filelist"), err)
	}
	if len(list) != 1 || list[0]["path"] != "/工作/草稿.docx" || list[0]["newname"] != "终稿.docx" {
		t.Fatalf("filelist = %v", list)
	}
}

func TestBaidupanFilelistShapeIsEnforced(t *testing.T) {
	m := newPanMock(t)
	rt := m.runtime(t, "tok-1", true)
	c := &Client{}
	bad := []map[string]any{
		{"filelist": []any{map[string]any{"path": "/a"}}},                                       // 缺 newname
		{"filelist": []any{map[string]any{"path": "/a", "newname": "b", "ondup": "overwrite"}}}, // 多了键
		{"filelist": []any{}},
	}
	for i, args := range bad {
		if _, _, err := c.Call(context.Background(), rt, "baidupan_rename", args); err == nil {
			t.Errorf("第 %d 组参数应当在发出前被拒绝", i+1)
		}
	}
	// 移动的 newname 可选。
	if _, _, err := c.Call(context.Background(), rt, "baidupan_move", map[string]any{
		"filelist": []any{map[string]any{"path": "/下载/发票.pdf", "dest": "/工作"}}}); err != nil {
		t.Fatalf("移动不带 newname 应当可以: %v", err)
	}
	_, _, _, _, form := m.snapshot()
	if form.Get("ondup") != "fail" {
		t.Fatalf("ondup 默认应当是 fail: %v", form)
	}
	m.mu.Lock()
	n := len(m.calls)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("只有最后那次合法调用该发出去，现在发了 %d 次", n)
	}
}

// 删除不能把逗号分隔的字符串拆开：路径里可以有逗号。
func TestBaidupanDeleteDoesNotSplitCommas(t *testing.T) {
	m := newPanMock(t)
	rt := m.runtime(t, "tok-1", true)
	c := &Client{}
	if _, _, err := c.Call(context.Background(), rt, "baidupan_delete", map[string]any{"filelist": "/a,b.txt"}); err == nil {
		t.Fatal("逗号分隔的字符串必须拒绝，不能拆成两个路径去删")
	}
	if _, _, err := c.Call(context.Background(), rt, "baidupan_delete", map[string]any{"filelist": []any{"/报表/1,2 月.xlsx"}}); err != nil {
		t.Fatal(err)
	}
	_, _, q, _, form := m.snapshot()
	if q.Get("opera") != "delete" || form.Get("filelist") != `["/报表/1,2 月.xlsx"]` {
		t.Fatalf("删除请求不对: %v %v", q, form)
	}
	// JSON 数组文本也认。
	if _, _, err := c.Call(context.Background(), rt, "baidupan_delete", map[string]any{"filelist": `["/x.txt"]`}); err != nil {
		t.Fatal(err)
	}
}

func TestBaidupanMkdirForm(t *testing.T) {
	m := newPanMock(t)
	out, _, err := (&Client{}).Call(context.Background(), m.runtime(t, "tok-1", true), "baidupan_mkdir",
		map[string]any{"path": "/工作/2026 报销"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, q, _, form := m.snapshot()
	if q.Get("method") != "create" || form.Get("path") != "/工作/2026 报销" || form.Get("isdir") != "1" || form.Get("rtype") != "0" {
		t.Fatalf("建文件夹请求不对: %v %v", q, form)
	}
	if got := out.(map[string]any)["path"]; got != "/工作/2026 报销" {
		t.Fatalf("结果: %#v", out)
	}
}
