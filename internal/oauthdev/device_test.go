package oauthdev

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeAuth 是一台仿真的授权服务，按 GitHub 的形状回话。
type fakeAuth struct {
	srv *httptest.Server
	// pollsBeforeOK 前几次轮询回 authorization_pending。
	pollsBeforeOK int
	polls         int
	// tokenError 非空时，轮询一律回这个错误码。
	tokenError string
	// expiring 为真时发一套会过期的令牌（带 refresh_token）。
	expiring bool
	// gotForms 记录每次收到的表单，用来断言参数。
	gotForms []url.Values
}

func newFakeAuth(t *testing.T, f *fakeAuth) *fakeAuth {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"device_code":"dev-code-40chars","user_code":"WDJB-MJHT",
			"verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		if f.tokenError != "" {
			io.WriteString(w, `{"error":"`+f.tokenError+`","error_description":"说明文字"}`)
			return
		}
		f.polls++
		if f.polls <= f.pollsBeforeOK {
			io.WriteString(w, `{"error":"authorization_pending","error_description":"等着"}`)
			return
		}
		if f.expiring {
			io.WriteString(w, `{"access_token":"gho_new","refresh_token":"ghr_new",
				"token_type":"bearer","scope":"repo,read:user",
				"expires_in":28800,"refresh_token_expires_in":15897600}`)
			return
		}
		io.WriteString(w, `{"access_token":"gho_abc123","token_type":"bearer","scope":"repo,read:user"}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuth) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	v, _ := url.ParseQuery(string(body))
	f.gotForms = append(f.gotForms, v)
}

func (f *fakeAuth) config() Config {
	return Config{
		ClientID:      "Iv1.test",
		DeviceCodeURL: f.srv.URL + "/login/device/code",
		TokenURL:      f.srv.URL + "/login/oauth/access_token",
		Scopes:        []string{"repo", "read:user"},
	}
}

func TestStartRequestsCodeWithScopes(t *testing.T) {
	f := newFakeAuth(t, &fakeAuth{})
	p, err := f.config().Start(context.Background(), f.srv.Client())
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if p.UserCode != "WDJB-MJHT" || p.VerificationURI != "https://github.com/login/device" {
		t.Fatalf("给用户看的码/地址不对: %+v", p)
	}
	if p.Interval != time.Second {
		t.Fatalf("interval = %v，应当用服务端给的值", p.Interval)
	}
	if p.ExpiresAt.IsZero() {
		t.Fatal("没有算出过期时间")
	}
	form := f.gotForms[0]
	if form.Get("client_id") != "Iv1.test" {
		t.Fatalf("client_id 没发出去: %v", form)
	}
	// 作用域必须是空格分隔（RFC 6749），逗号分隔会被当成一个作用域名。
	if form.Get("scope") != "repo read:user" {
		t.Fatalf("scope = %q，应当空格分隔", form.Get("scope"))
	}
	// 设备流不该出现 client_secret —— 公共客户端存不住它。
	if form.Get("client_secret") != "" {
		t.Fatal("请求里出现了 client_secret")
	}
}

func TestAwaitPollsUntilAuthorized(t *testing.T) {
	f := newFakeAuth(t, &fakeAuth{pollsBeforeOK: 2})
	cfg := f.config()
	p, err := cfg.Start(context.Background(), f.srv.Client())
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	tok, err := cfg.Await(context.Background(), f.srv.Client(), p)
	if err != nil {
		t.Fatalf("Await 失败: %v", err)
	}
	if tok.AccessToken != "gho_abc123" {
		t.Fatalf("令牌不对: %+v", tok)
	}
	if f.polls != 3 {
		t.Fatalf("轮询了 %d 次，期望 3（2 次 pending + 1 次成功）", f.polls)
	}
	// 不过期的令牌永远不该被判定为该刷新。
	if tok.Stale(time.Now().Add(100 * 24 * time.Hour)) {
		t.Fatal("不过期的令牌被判成过期了")
	}
	if tok.CanRefresh(time.Now()) {
		t.Fatal("没有 refresh_token 却说可以刷新")
	}
	// 轮询的三个参数一个不能少。
	last := f.gotForms[len(f.gotForms)-1]
	if last.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Fatalf("grant_type = %q", last.Get("grant_type"))
	}
	if last.Get("device_code") != "dev-code-40chars" {
		t.Fatalf("device_code = %q", last.Get("device_code"))
	}
}

func TestExpiringTokenCarriesRefresh(t *testing.T) {
	f := newFakeAuth(t, &fakeAuth{expiring: true})
	cfg := f.config()
	p, _ := cfg.Start(context.Background(), f.srv.Client())
	tok, err := cfg.Await(context.Background(), f.srv.Client(), p)
	if err != nil {
		t.Fatalf("Await 失败: %v", err)
	}
	if tok.RefreshToken != "ghr_new" || tok.ExpiresAt.IsZero() {
		t.Fatalf("过期令牌的字段不全: %+v", tok)
	}
	if !tok.CanRefresh(time.Now()) {
		t.Fatal("有 refresh_token 却说不能刷新")
	}
	// 到期前 60 秒就该认为该换了：卡着到期时刻用，请求在途中就失效。
	if !tok.Stale(tok.ExpiresAt.Add(-30 * time.Second)) {
		t.Fatal("到期前 30 秒没有判定为该刷新")
	}
	if tok.Stale(tok.ExpiresAt.Add(-10 * time.Minute)) {
		t.Fatal("离到期还有 10 分钟就判定该刷新了")
	}
}

func TestRefreshExchangesNewToken(t *testing.T) {
	f := newFakeAuth(t, &fakeAuth{expiring: true})
	tok, err := f.config().Refresh(context.Background(), f.srv.Client(), "ghr_old")
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if tok.AccessToken != "gho_new" {
		t.Fatalf("没拿到新令牌: %+v", tok)
	}
	form := f.gotForms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "ghr_old" {
		t.Fatalf("刷新参数不对: %v", form)
	}
}

// TestErrorCodesAreActionable：每种错误码都要翻成能据以行动的话。
// 全写成「授权失败」用户无从下手 —— 这几条都是真会遇到的。
func TestErrorCodesAreActionable(t *testing.T) {
	cases := []struct {
		code     string
		sentinel error
		want     string
	}{
		{"authorization_pending", ErrPending, ""},
		{"slow_down", ErrSlowDown, ""},
		{"expired_token", ErrExpired, ""},
		{"access_denied", ErrDenied, ""},
		{"device_flow_disabled", nil, "勾上 Enable Device Flow"},
		{"incorrect_client_credentials", nil, "client id 不对"},
		{"incorrect_device_code", nil, "设备码无效"},
		{"unsupported_grant_type", nil, "不支持设备流"},
		{"some_new_code", nil, "说明文字"},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			f := newFakeAuth(t, &fakeAuth{tokenError: c.code})
			_, err := f.config().Poll(context.Background(), f.srv.Client(), "dev-code")
			if err == nil {
				t.Fatal("本该报错")
			}
			if c.sentinel != nil {
				if !errors.Is(err, c.sentinel) {
					t.Fatalf("err = %v，期望哨兵 %v", err, c.sentinel)
				}
				return
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %q，期望包含 %q", err, c.want)
			}
		})
	}
}

// TestSlowDownIncreasesInterval：收到 slow_down 要加 5 秒间隔，
// 而不是原速重试 —— 原速重试会一直撞限流。
func TestSlowDownIncreasesInterval(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.Header().Set("Content-Type", "application/json")
		if polls == 1 {
			io.WriteString(w, `{"error":"slow_down","error_description":"慢点"}`)
			return
		}
		io.WriteString(w, `{"access_token":"gho_ok","token_type":"bearer"}`)
	}))
	defer srv.Close()

	cfg := Config{ClientID: "x", TokenURL: srv.URL}
	start := time.Now()
	tok, err := cfg.Await(context.Background(), srv.Client(), Pending{
		DeviceCode: "d", Interval: 10 * time.Millisecond,
		ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Await 失败: %v", err)
	}
	if tok.AccessToken != "gho_ok" {
		t.Fatalf("令牌不对: %+v", tok)
	}
	// 第二次轮询前至少等了 5 秒多一点（10ms + 5s）。
	if elapsed := time.Since(start); elapsed < 5*time.Second {
		t.Fatalf("只等了 %v，slow_down 后没有加间隔", elapsed)
	}
}

func TestAwaitStopsWhenCodeExpired(t *testing.T) {
	f := newFakeAuth(t, &fakeAuth{pollsBeforeOK: 1000})
	_, err := f.config().Await(context.Background(), f.srv.Client(), Pending{
		DeviceCode: "d", Interval: 10 * time.Millisecond,
		ExpiresAt: time.Now().Add(-time.Second), // 已经过期
	})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v，期望 ErrExpired", err)
	}
	if f.polls != 0 {
		t.Fatalf("码已过期还轮询了 %d 次", f.polls)
	}
}

// TestParseTellsTokenSetFromPAT：同一个凭据槽要装两种东西，
// 靠标记区分，不能靠猜字符串长相。
func TestParseTellsTokenSetFromPAT(t *testing.T) {
	set := TokenSet{AccessToken: "gho_x", ObtainedAt: time.Now()}
	stored, err := set.Marshal()
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	got, ok := Parse(stored)
	if !ok || got.AccessToken != "gho_x" {
		t.Fatalf("没认出令牌组: ok=%v got=%+v", ok, got)
	}

	for _, pat := range []string{
		"ghp_classicPersonalAccessToken",
		"github_pat_11ABCDE",
		"",
		"   ",
		`{"accessToken":"没有标记"}`,                 // 像 JSON 但没有标记
		`{"mcphubOAuth":"v1"}`,                   // 有标记但没有令牌
		`{"mcphubOAuth":"v9","accessToken":"x"}`, // 未来版本的标记
	} {
		if _, ok := Parse(pat); ok {
			t.Fatalf("%q 被误判成令牌组", pat)
		}
	}
}
