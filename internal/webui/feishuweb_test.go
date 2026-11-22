package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 交任务的端点回的是文档正文：Host 不是回环（DNS rebinding）或者请求来自
// 别的网站时都要拒。
func TestFeishuJobRejectsNonLoopbackAndCrossSite(t *testing.T) {
	s := New(nil, "test")
	cases := []struct {
		name   string
		host   string
		origin string
	}{
		{"rebinding", "evil.example:7801", "http://evil.example:7801"},
		{"cross-site", "127.0.0.1:7801", "https://evil.example"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/feishu-web/job", strings.NewReader(`{"kind":"recent"}`))
		req.Host = c.host
		req.Header.Set("Origin", c.origin)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: 期望 403，得到 %d %s", c.name, rec.Code, rec.Body)
		}
	}
}

// 本机非浏览器调用（MCP 进程）能进来；扩展没连时回明确的错误，而不是挂住。
func TestFeishuJobWithoutExtension(t *testing.T) {
	s := New(nil, "test")
	req := httptest.NewRequest(http.MethodPost, "/api/feishu-web/job", strings.NewReader(`{"kind":"recent"}`))
	req.Host = "127.0.0.1:7801"
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "浏览器扩展没有连上") {
		t.Errorf("得到 %d %s", rec.Code, rec.Body)
	}
}

// 扩展端点不过同站闸，但只认固定扩展 ID 的 Origin。
func TestExtensionEndpointChecksOrigin(t *testing.T) {
	s := New(nil, "test")
	req := httptest.NewRequest(http.MethodGet, "/ext/ws", nil)
	req.Host = "127.0.0.1:7801"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("期望 403，得到 %d", rec.Code)
	}
}
