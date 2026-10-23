package webui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 内联 onclick 里拼模板变量曾让「编辑」按钮整个静默失效：
//
//	onclick="doSave('github',"nimysan")"
//	                        ^ 属性在这里被截断，onclick 变成语法错误
//
// 点下去什么都不发生 —— 没有报错、没有网络请求，用户以为保存成功了，
// 而配置文件根本没被写过。这类 bug 不会自己暴露，所以用测试钉住：
// 事件处理一律在 JS 里挂闭包，不准拼进 HTML 属性。
func TestNoInlineHandlersWithInterpolation(t *testing.T) {
	data, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	// 匹配 onX="..." / onX='...' 形式的内联事件属性
	attrRe := regexp.MustCompile(`on[a-z]+\s*=\s*["'][^"']*["']`)
	for _, m := range attrRe.FindAllString(html, -1) {
		if strings.Contains(m, "${") {
			t.Errorf("内联事件属性里拼了模板变量，参数含引号时会截断属性: %s", m)
		}
	}

	// attr()（JSON.stringify 包装）正是当初被塞进双引号属性的那个函数，
	// 它不该再出现，免得这条路被重新走一遍。
	if strings.Contains(html, "function attr(") {
		t.Error("attr() 已被移除，重新引入它意味着又在拼内联属性的参数")
	}
}

// 页面编译进二进制，升级后浏览器必须拿到新的那份。
func TestIndexIsNotCacheable(t *testing.T) {
	s := New(nil, "test")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("首页应当 200，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control 应当是 no-store，实际 %q", got)
	}
	if !strings.Contains(rec.Body.String(), "mcp-hub") {
		t.Error("首页内容不对")
	}
}
