package oauthdev

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 仿真的飞书授权服务。形状照 2026-11 实测抄的。
type feishuAuthMock struct {
	srv *httptest.Server

	mu         sync.Mutex
	authed     bool // 用户是否已在飞书里确认授权
	registered bool // 用户是否已确认建应用
	lark       bool // 扫码的是 Lark 租户
	refreshes  int
	lastForm   url.Values
	lastJSON   map[string]string
	lastAuth   string
}

const (
	mockAppID     = "cli_mock"
	mockAppSecret = "sec_mock"
)

func newFeishuAuthMock(t *testing.T) *feishuAuthMock {
	t.Helper()
	m := &feishuAuthMock{}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.lastForm = r.PostForm
		switch r.PostForm.Get("action") {
		case "begin":
			writeJSON(w, 200, map[string]any{
				"device_code": "reg-dev", "user_code": "GY4U-LJSZ",
				"verification_uri":          "https://open.feishu.cn/page/launcher",
				"verification_uri_complete": "https://open.feishu.cn/page/launcher?user_code=GY4U-LJSZ",
				"expires_in":                3600, "interval": 1,
			})
		case "poll":
			switch {
			case m.lark:
				writeJSON(w, 200, map[string]any{"error": "authorization_pending", "user_info": map[string]any{"tenant_brand": "lark"}})
			case m.registered:
				writeJSON(w, 200, map[string]any{"client_id": mockAppID, "client_secret": mockAppSecret,
					"user_info": map[string]any{"open_id": "ou_me", "tenant_brand": "feishu"}})
			default:
				writeJSON(w, 200, map[string]any{"error": "authorization_pending"})
			}
		}
	})
	mux.HandleFunc("/oauth/v1/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		m.mu.Lock()
		m.lastForm, m.lastAuth = r.PostForm, r.Header.Get("Authorization")
		m.mu.Unlock()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(mockAppID+":"+mockAppSecret))
		if r.Header.Get("Authorization") != want {
			writeJSON(w, 401, map[string]any{"error": "invalid_client", "error_description": "bad secret"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"device_code": "user-dev", "user_code": "YGQK-PS6U",
			"verification_uri":          "https://accounts.feishu.cn/oauth/v1/device/verify",
			"verification_uri_complete": "https://accounts.feishu.cn/oauth/v1/device/verify?user_code=YGQK-PS6U",
			"expires_in":                600, "interval": 1,
		})
	})
	mux.HandleFunc("/open-apis/authen/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]string
			json.Unmarshal(raw, &body)
			m.lastJSON = body
			if body["grant_type"] != "refresh_token" || body["client_secret"] != mockAppSecret {
				writeJSON(w, 400, map[string]any{"code": 20050, "error": "invalid_client"})
				return
			}
			m.refreshes++
			writeJSON(w, 200, map[string]any{"code": 0, "access_token": "at-refreshed", "refresh_token": "rt-rotated",
				"token_type": "Bearer", "expires_in": 7200, "refresh_token_expires_in": 604800})
			return
		}
		r.ParseForm()
		m.lastForm = r.PostForm
		if r.PostForm.Get("client_secret") != mockAppSecret {
			writeJSON(w, 400, map[string]any{"code": 20050, "error": "invalid_client"})
			return
		}
		if !m.authed {
			writeJSON(w, 400, map[string]any{"code": 20064, "error": "authorization_pending"})
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "access_token": "at-1", "refresh_token": "rt-1",
			"token_type": "Bearer", "expires_in": 7200, "refresh_token_expires_in": 604800,
			"scope": "im:message offline_access"})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *feishuAuthMock) config(secret string) Config {
	return Config{
		ClientID: mockAppID, ClientSecret: secret, Dialect: DialectFeishu,
		DeviceCodeURL: m.srv.URL + "/oauth/v1/device_authorization",
		TokenURL:      m.srv.URL + "/open-apis/authen/v2/oauth/token",
		Scopes:        []string{"im:message"},
	}
}

func TestFeishuDeviceFlow(t *testing.T) {
	m := newFeishuAuthMock(t)
	cfg := m.config(mockAppSecret)
	ctx := context.Background()
	p, err := cfg.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.UserCode != "YGQK-PS6U" || !strings.Contains(p.VerificationURIComplete, "user_code=") {
		t.Fatalf("pending = %+v", p)
	}
	// 不申请 offline_access 就没有 refresh_token，2 小时后就得重扫。
	if sc := m.lastForm.Get("scope"); sc != "im:message offline_access" {
		t.Fatalf("scope = %q，应自动补上 offline_access", sc)
	}
	if _, err := cfg.Poll(ctx, nil, p.DeviceCode); !errors.Is(err, ErrPending) {
		t.Fatalf("用户未确认时应是 ErrPending，实际 %v", err)
	}
	m.mu.Lock()
	m.authed = true
	m.mu.Unlock()
	tok, err := cfg.Poll(ctx, nil, p.DeviceCode)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" || tok.ClientSecret != mockAppSecret {
		t.Fatalf("token = %+v", tok)
	}
	if m.lastForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Fatalf("grant_type = %q", m.lastForm.Get("grant_type"))
	}
	if tok.RefreshExpiresAt.Sub(tok.ObtainedAt) < 6*24*time.Hour {
		t.Fatal("refresh_token 有效期应约 7 天")
	}
	// 应用密钥跟着令牌组存：刷新时要用它。
	stored, _ := tok.Marshal()
	back, ok := Parse(stored)
	if !ok || back.ClientSecret != mockAppSecret {
		t.Fatal("ClientSecret 没有随令牌组序列化")
	}
}

func TestFeishuWrongSecretIsExplained(t *testing.T) {
	m := newFeishuAuthMock(t)
	_, err := m.config("wrong").Start(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "App Secret") {
		t.Fatalf("密钥错误时应说清是 App Secret 的问题，实际 %v", err)
	}
	if _, err := (Config{Dialect: DialectFeishu, ClientID: "x"}).Start(context.Background(), nil); err == nil {
		t.Fatal("没有密钥时不应发请求")
	}
}

func TestFeishuRefreshUsesJSONAndRotates(t *testing.T) {
	m := newFeishuAuthMock(t)
	tok, err := m.config(mockAppSecret).Refresh(context.Background(), nil, "rt-1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if m.lastJSON["refresh_token"] != "rt-1" || m.lastJSON["client_id"] != mockAppID {
		t.Fatalf("刷新请求体 = %v", m.lastJSON)
	}
	if tok.RefreshToken != "rt-rotated" || tok.ClientSecret != mockAppSecret {
		t.Fatalf("token = %+v", tok)
	}
}

func TestFeishuAppRegistration(t *testing.T) {
	m := newFeishuAuthMock(t)
	reg := AppRegistration{
		URL: m.srv.URL + "/oauth/v1/app/registration", Name: "MCP Hub（{user}）",
		UserScopes: []string{"im:message", "im:message.send_as_user"},
	}
	ctx := context.Background()
	p, err := reg.Begin(ctx, nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if m.lastForm.Get("archetype") != "PersonalAgent" || m.lastForm.Get("auth_method") != "client_secret" {
		t.Fatalf("begin 参数 = %v", m.lastForm)
	}
	u, _ := url.Parse(p.VerificationURIComplete)
	q := u.Query()
	if u.Host != "open.feishu.cn" || q.Get("user_code") != "GY4U-LJSZ" || q.Get("createOnly") != "true" || q.Get("name") != "MCP Hub（{user}）" {
		t.Fatalf("建应用链接 = %s", p.VerificationURIComplete)
	}
	// 预填权限的编码要和官方 SDK 一致，否则确认页上不会勾上。
	raw, err := base64.RawURLEncoding.DecodeString(q.Get("addons"))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != `{"scopes":{"user":["im:message","im:message.send_as_user"]}}` {
		t.Fatalf("addons = %s", plain)
	}
	if _, err := reg.Poll(ctx, nil, p.DeviceCode); !errors.Is(err, ErrPending) {
		t.Fatalf("未确认时应是 ErrPending，实际 %v", err)
	}
	m.mu.Lock()
	m.registered = true
	m.mu.Unlock()
	creds, err := reg.Await(ctx, nil, p)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if creds.ClientID != mockAppID || creds.ClientSecret != mockAppSecret || creds.OpenID != "ou_me" {
		t.Fatalf("creds = %+v", creds)
	}
}

func TestFeishuRegistrationRejectsLarkTenant(t *testing.T) {
	m := newFeishuAuthMock(t)
	m.lark = true
	reg := AppRegistration{URL: m.srv.URL + "/oauth/v1/app/registration"}
	if _, err := reg.Poll(context.Background(), nil, "reg-dev"); !errors.Is(err, ErrLarkTenant) {
		t.Fatalf("Lark 租户应被明确拒绝，实际 %v", err)
	}
}

// 建应用链接是用户确认「在自己企业里建应用」的那一页，主机不对就不给。
func TestFeishuRegistrationLinkMustBeFeishu(t *testing.T) {
	for _, raw := range []string{
		"https://evil.example.com/page/launcher?user_code=x",
		"http://open.feishu.cn/page/launcher",
		"https://feishu.cn.evil.com/x",
	} {
		if _, err := (AppRegistration{}).link(raw); err == nil {
			t.Errorf("%s 应被拒绝", raw)
		}
	}
}
