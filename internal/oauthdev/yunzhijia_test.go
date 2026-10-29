package oauthdev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 仿真的云之家授权服务。形状照实测抄的：一律 HTTP 200，外层
// {data,error,errorCode,success} 信封，授权状态在 data 里面。
type yzjAuthMock struct {
	srv *httptest.Server

	mu       sync.Mutex
	calls    []string         // "METHOD /path"
	bodies   []map[string]any // 每次请求的 JSON body
	authed   bool             // 用户是否已经在手机上确认
	denyOnce bool
}

func newYZJAuthMock(t *testing.T) *yzjAuthMock {
	t.Helper()
	m := &yzjAuthMock{}
	mux := http.NewServeMux()

	record := func(r *http.Request) map[string]any {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var body map[string]any
		json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.calls = append(m.calls, r.Method+" "+r.URL.Path)
		m.bodies = append(m.bodies, body)
		m.mu.Unlock()
		return body
	}
	envelope := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": data, "error": nil, "errorCode": 0, "success": true,
		})
	}
	fail := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": nil, "error": msg, "errorCode": 11003008, "success": false,
		})
	}

	mux.HandleFunc("/device/genDeviceCode", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		envelope(w, map[string]any{
			"clientId": "cli_test", "deviceCode": "dev-code-1",
			"userCode": "ABCDEFGHIJ", "expiresIn": 900, "interval": 5,
		})
	})
	mux.HandleFunc("/cli/pollTokenByDeviceCode", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		if body["deviceCode"] != "dev-code-1" {
			envelope(w, map[string]any{
				"success": false, "errorCode": "expired_token",
				"errorDescription": "设备码已过期，请重新发起授权请求",
			})
			return
		}
		m.mu.Lock()
		authed, deny := m.authed, m.denyOnce
		m.mu.Unlock()
		switch {
		case deny:
			envelope(w, map[string]any{
				"success": false, "errorCode": "access_denied", "errorDescription": "用户拒绝",
			})
		case !authed:
			envelope(w, map[string]any{
				"success": false, "errorCode": "authorization_pending",
				"errorDescription": "等待用户完成授权",
			})
		default:
			envelope(w, map[string]any{
				"success": true, "accessToken": "at-1", "refreshToken": "rt-1",
				"scope": "searchUserInfo", "expiresIn": 5983,
			})
		}
	})
	mux.HandleFunc("/oauth2/token/refreshToken", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		if body["refreshToken"] != "rt-1" {
			fail(w, "令牌非法")
			return
		}
		if _, ok := body["timestamp"]; !ok {
			fail(w, "timestamp不能为空")
			return
		}
		envelope(w, map[string]any{
			"success": true, "accessToken": "at-2", "refreshToken": "rt-2", "expiresIn": 5983,
		})
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *yzjAuthMock) config() Config {
	return Config{
		ClientID:        "cli_test",
		Dialect:         DialectYunzhijia,
		DeviceCodeURL:   m.srv.URL + "/device/genDeviceCode",
		TokenURL:        m.srv.URL + "/cli/pollTokenByDeviceCode",
		RefreshURL:      m.srv.URL + "/oauth2/token/refreshToken",
		VerificationURI: "https://www.yunzhijia.com/developers/cli/device-auth.html",
		Scopes:          []string{"searchUserInfo"},
	}
}

func (m *yzjAuthMock) lastBody() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return nil
	}
	return m.bodies[len(m.bodies)-1]
}

// 出码要用云之家的参数名（appId，不是 client_id），而且因为它的响应里
// 不带验证地址，一键地址必须由声明的那一页加上 ?code= 拼出来。
func TestYunzhijiaStartUsesAppIDAndBuildsCompleteURI(t *testing.T) {
	m := newYZJAuthMock(t)
	p, err := m.config().Start(context.Background(), m.srv.Client())
	if err != nil {
		t.Fatalf("出码失败: %v", err)
	}
	body := m.lastBody()
	if body["appId"] != "cli_test" {
		t.Errorf("参数名应是 appId=cli_test，实际 body=%v", body)
	}
	if _, bad := body["client_id"]; bad {
		t.Errorf("不该发 client_id（那是 RFC 8628 的名字）: %v", body)
	}
	if body["scope"] != "searchUserInfo" {
		t.Errorf("scope 没带上: %v", body)
	}
	if p.UserCode != "ABCDEFGHIJ" || p.DeviceCode != "dev-code-1" {
		t.Errorf("码解析错了: %+v", p)
	}
	want := "https://www.yunzhijia.com/developers/cli/device-auth.html?code=ABCDEFGHIJ"
	if p.VerificationURIComplete != want {
		t.Errorf("一键地址 = %q, 想要 %q", p.VerificationURIComplete, want)
	}
	if p.Interval != 5*time.Second {
		t.Errorf("轮询间隔 = %v, 想要 5s", p.Interval)
	}
}

// 没有验证地址就不能出码：那一页是用户真正输账号密码的地方，
// 猜一个出来等于把人送到陌生页面登录。
func TestYunzhijiaStartNeedsVerificationURI(t *testing.T) {
	m := newYZJAuthMock(t)
	c := m.config()
	c.VerificationURI = ""
	_, err := c.Start(context.Background(), m.srv.Client())
	if err == nil || !strings.Contains(err.Error(), "verificationUri") {
		t.Fatalf("想要点名 verificationUri 的报错，实际 %v", err)
	}
}

// data.success 为 false 时要按 data.errorCode 分流 —— 它用的是 RFC 8628
// 的标准串，所以和 GitHub 共用同一套错误翻译。
func TestYunzhijiaPollStates(t *testing.T) {
	m := newYZJAuthMock(t)
	c := m.config()

	if _, err := c.Poll(context.Background(), m.srv.Client(), "dev-code-1"); !errors.Is(err, ErrPending) {
		t.Fatalf("未授权应是 ErrPending，实际 %v", err)
	}
	if _, err := c.Poll(context.Background(), m.srv.Client(), "someone-elses"); !errors.Is(err, ErrExpired) {
		t.Fatalf("无效设备码应是 ErrExpired，实际 %v", err)
	}

	m.mu.Lock()
	m.denyOnce = true
	m.mu.Unlock()
	if _, err := c.Poll(context.Background(), m.srv.Client(), "dev-code-1"); !errors.Is(err, ErrDenied) {
		t.Fatalf("拒绝授权应是 ErrDenied，实际 %v", err)
	}

	m.mu.Lock()
	m.denyOnce = false
	m.authed = true
	m.mu.Unlock()
	tok, err := c.Poll(context.Background(), m.srv.Client(), "dev-code-1")
	if err != nil {
		t.Fatalf("授权后应拿到令牌: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Errorf("令牌解析错了: %+v", tok)
	}
	// 云之家的令牌是会过期的（约 100 分钟），所以必须算出 ExpiresAt ——
	// 算不出就永远不会去刷新，用户得反复扫码。
	if tok.ExpiresAt.IsZero() {
		t.Error("expiresIn 没有换成 ExpiresAt，令牌会被当成永不过期")
	}
	if !tok.CanRefresh(time.Now()) {
		t.Error("有 refreshToken 却报告不能刷新")
	}
}

// 刷新打的是 refreshUrl（不是 tokenUrl），而且必须带 timestamp。
func TestYunzhijiaRefreshUsesOwnEndpoint(t *testing.T) {
	m := newYZJAuthMock(t)
	tok, err := m.config().Refresh(context.Background(), m.srv.Client(), "rt-1")
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if tok.AccessToken != "at-2" {
		t.Errorf("没拿到新令牌: %+v", tok)
	}
	m.mu.Lock()
	last := m.calls[len(m.calls)-1]
	m.mu.Unlock()
	if !strings.HasSuffix(last, "/oauth2/token/refreshToken") {
		t.Errorf("刷新打到了 %s，应该打 refreshUrl", last)
	}
	if _, ok := m.lastBody()["timestamp"]; !ok {
		t.Errorf("刷新没带 timestamp（云之家会拒）: %v", m.lastBody())
	}
}

// 外层信封失败（HTTP 仍是 200）必须报出来，并且带上它给的原因。
func TestYunzhijiaEnvelopeFailureSurfaces(t *testing.T) {
	m := newYZJAuthMock(t)
	_, err := m.config().Refresh(context.Background(), m.srv.Client(), "wrong-token")
	if err == nil {
		t.Fatal("假刷新令牌应该失败")
	}
	if !strings.Contains(err.Error(), "令牌非法") {
		t.Errorf("报错应带上服务端的原因，实际 %v", err)
	}
}

// 认不出的方言一律拒绝，不能退回默认实现：按错误格式打一个真实授权服务，
// 表现是「一直等待授权」这种看不出原因的样子。
func TestUnknownDialectRefused(t *testing.T) {
	c := Config{ClientID: "x", Dialect: "wechat-work", DeviceCodeURL: "https://e/x", TokenURL: "https://e/y"}
	if _, err := c.Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "方言") {
		t.Errorf("Start 应拒绝未知方言，实际 %v", err)
	}
	if _, err := c.Poll(context.Background(), nil, "d"); err == nil || !strings.Contains(err.Error(), "方言") {
		t.Errorf("Poll 应拒绝未知方言，实际 %v", err)
	}
	if _, err := c.Refresh(context.Background(), nil, "r"); err == nil || !strings.Contains(err.Error(), "方言") {
		t.Errorf("Refresh 应拒绝未知方言，实际 %v", err)
	}
}

// 空方言＝RFC 8628，GitHub 那条不能因为加了方言就变样。
func TestEmptyDialectStaysRFC8628(t *testing.T) {
	var got struct {
		contentType string
		form        string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.contentType = r.Header.Get("Content-Type")
		got.form = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"device_code":"dc","user_code":"UC","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
	}))
	defer srv.Close()

	c := Config{ClientID: "Iv1.x", DeviceCodeURL: srv.URL, TokenURL: srv.URL, Scopes: []string{"repo"}}
	p, err := c.Start(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("标准方言出码失败: %v", err)
	}
	if !strings.Contains(got.contentType, "x-www-form-urlencoded") {
		t.Errorf("标准方言应发 form 编码，实际 %q", got.contentType)
	}
	if !strings.Contains(got.form, "client_id=Iv1.x") {
		t.Errorf("标准方言应发 client_id，实际 %q", got.form)
	}
	if p.VerificationURI != "https://github.com/login/device" {
		t.Errorf("标准方言的验证地址来自响应，实际 %q", p.VerificationURI)
	}
}
