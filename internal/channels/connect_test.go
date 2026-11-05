package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/oauthdev"
	"github.com/zalando/go-keyring"
)

// 这组测试走完整条「点连接」链路：要码 -> 轮询 -> 拿到令牌 -> 向对方系统问
// 身份 -> 凭据进密码管理器 -> 实例落配置 -> 后续工具调用真的用上这个令牌。
//
// 授权服务和业务接口由同一台仿真 TLS 服务承担（一张证书省事），用的是
// 目录本地层注入的一份声明 —— 也就是说这条链路和远端下发一个 connector
// 走的是同一套代码。

type fakeProvider struct {
	srv *httptest.Server

	pollsBeforeOK int
	polls         int
	tokenError    string
	// expiringToken 为真时发一套 8 小时过期、带 refresh_token 的令牌。
	expiringToken bool

	// gotBearer 记录业务接口收到的 Authorization。
	gotBearer []string
}

func newFakeProvider(t *testing.T, f *fakeProvider) *fakeProvider {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"device_code":"dev-code","user_code":"WDJB-MJHT",
			"verification_uri":"https://example.test/login/device","expires_in":900,"interval":1}`)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if f.tokenError != "" {
			io.WriteString(w, `{"error":"`+f.tokenError+`","error_description":"说明"}`)
			return
		}
		if strings.Contains(string(body), "grant_type=refresh_token") {
			io.WriteString(w, `{"access_token":"gho_refreshed","refresh_token":"ghr_next",
				"token_type":"bearer","expires_in":28800,"refresh_token_expires_in":15897600}`)
			return
		}
		f.polls++
		if f.polls <= f.pollsBeforeOK {
			io.WriteString(w, `{"error":"authorization_pending"}`)
			return
		}
		if f.expiringToken {
			io.WriteString(w, `{"access_token":"gho_abc123","refresh_token":"ghr_first",
				"token_type":"bearer","expires_in":28800,"refresh_token_expires_in":15897600}`)
			return
		}
		io.WriteString(w, `{"access_token":"gho_abc123","token_type":"bearer","scope":"repo"}`)
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		f.gotBearer = append(f.gotBearer, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"login":"test-user","name":"测试用户","extra":"不在声明里"}`)
	})

	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// writeLocalSpec 把一份声明放进目录的本地层 —— 和用户自己放一份、或者远端
// 下发一份，走的是同一条加载路径。
func writeLocalSpec(t *testing.T, f *fakeProvider, clientIDField bool) {
	t.Helper()
	dir, err := catalogDir()
	if err != nil {
		t.Fatalf("定位目录失败: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}

	clientPart := `"clientId": "Iv1.test",`
	fieldsPart := ""
	if clientIDField {
		clientPart = `"clientIdField": "oauthClientId",`
		fieldsPart = `"fields":[{"name":"oauthClientId","label":"Client ID"}],`
	}

	spec := fmt.Sprintf(`{
	  "id": "oauthdemo",
	  "name": "授权演示",
	  "version": 1,
	  "integration": "oauthdemo",
	  "baseUrl": %q,
	  "account": {"name":"login","label":"账号"},
	  "secret": {"name":"token","label":"访问令牌"},
	  "auth": {"kind":"bearer"},
	  %s
	  "oauth": {
	    "flow": "device",
	    %s
	    "deviceCodeUrl": %q,
	    "tokenUrl": %q,
	    "scopes": ["repo","read:user"],
	    "setupUrl": "https://example.test/settings/developers",
	    "help": "记得勾上 Enable Device Flow"
	  },
	  "test": {"method":"GET","path":"/user","result":{"fields":["login","name"]}},
	  "tools": [{
	    "name":"oauthdemo_whoami","title":"当前身份","description":"看看是谁",
	    "readOnly":true,"method":"GET","path":"/user",
	    "result":{"fields":["login","name"]}
	  }]
	}`, f.srv.URL, fieldsPart, clientPart,
		f.srv.URL+"/login/device/code", f.srv.URL+"/login/oauth/access_token")

	if err := os.WriteFile(filepath.Join(dir, "oauthdemo.json"), []byte(spec), 0o600); err != nil {
		t.Fatalf("写声明失败: %v", err)
	}
}

func catalogDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, config.Namespace, "catalog"), nil
}

// newTestManager 用内存钥匙串和临时配置目录，避免碰到真实凭据。
func newTestManager(t *testing.T, f *fakeProvider) *Manager {
	t.Helper()
	keyring.MockInit()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// Windows 的 os.UserConfigDir 读的是 %AppData%，不设它的话这里的隔离
	// 在 Windows 上是假的 —— 测试会写进 runner 真实的用户目录。
	t.Setenv("AppData", filepath.Join(home, "AppData", "Roaming"))

	store, err := NewStore(config.Namespace)
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	m := NewManager(store, credstore.New(config.Namespace))
	m.HTTP = f.srv.Client()
	return m
}

// awaitState 等连接会话进入终态。
func awaitState(t *testing.T, m *Manager, id string) ConnectStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := m.ConnectStatus(id)
		if err != nil {
			t.Fatalf("查状态失败: %v", err)
		}
		if st.State != ConnectPending {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("等待连接终态超时")
	return ConnectStatus{}
}

// TestConnectFlowStoresTokenAndInstance 是这次改动的核心断言。
func TestConnectFlowStoresTokenAndInstance(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{pollsBeforeOK: 1})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, false)

	sess, err := m.StartConnect("oauthdemo", map[string]string{})
	if err != nil {
		t.Fatalf("StartConnect 失败: %v", err)
	}
	if sess.UserCode != "WDJB-MJHT" {
		t.Fatalf("给用户看的码不对: %+v", sess)
	}
	if sess.VerificationURI != "https://example.test/login/device" {
		t.Fatalf("验证地址不对: %+v", sess)
	}
	// 设备码不能下发到前端：它等价于一次授权的凭据。
	raw, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("序列化会话失败: %v", err)
	}
	if strings.Contains(string(raw), "dev-code") {
		t.Fatalf("设备码泄漏到了前端响应里: %s", raw)
	}

	st := awaitState(t, m, sess.ID)
	if st.State != ConnectOK {
		t.Fatalf("授权没成功: %+v", st)
	}
	// 账号名是向对方系统问出来的，不是用户填的。
	if st.Account != "test-user" {
		t.Fatalf("账号 = %q，期望从身份接口读出 test-user", st.Account)
	}

	// 实例落到配置文件里了。
	in, ok, err := m.Store().Get("oauthdemo", "test-user")
	if err != nil || !ok {
		t.Fatalf("实例没保存: ok=%v err=%v", ok, err)
	}
	if !in.Enabled {
		t.Fatal("实例没有启用")
	}

	// 凭据是一套令牌组，不是裸字符串；而且配置文件里没有令牌。
	stored, err := m.creds.Secret(credstore.Ref{Integration: "oauthdemo", Account: "test-user"}).Value()
	if err != nil {
		t.Fatalf("凭据没存进密码管理器: %v", err)
	}
	tok, isSet := oauthdev.Parse(stored)
	if !isSet || tok.AccessToken != "gho_abc123" {
		t.Fatalf("存的不是令牌组: %q", stored)
	}
	cfgRaw, err := os.ReadFile(m.Store().Path())
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if strings.Contains(string(cfgRaw), "gho_abc123") {
		t.Fatalf("令牌写进了配置文件: %s", cfgRaw)
	}

	// 最要紧的一条：后续工具调用真的用上了这个令牌。
	groups, err := m.HTTPGroups()
	if err != nil {
		t.Fatalf("装配工具组失败: %v", err)
	}
	var rt *httpchan.Runtime
	for i := range groups {
		if groups[i].Spec.ID == "oauthdemo" {
			rt = &groups[i].Runtimes[0]
		}
	}
	if rt == nil {
		t.Fatal("没有装配出 oauthdemo 的运行态")
	}
	out, _, err := (&httpchan.Client{HTTP: f.srv.Client()}).
		Call(context.Background(), *rt, "oauthdemo_whoami", nil)
	if err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	if !strings.Contains(fmt.Sprint(out), "test-user") {
		t.Fatalf("工具结果不对: %v", out)
	}
	last := f.gotBearer[len(f.gotBearer)-1]
	if last != "Bearer gho_abc123" {
		t.Fatalf("业务请求带的是 %q，期望授权拿到的访问令牌", last)
	}
}

// TestExpiredTokenRefreshesAndPersists：过期令牌在调用时自动换新，
// 并写回密码管理器 —— 不写回的话，轮换过的刷新令牌会作废。
func TestExpiredTokenRefreshesAndPersists(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, false)

	d, ok := ByType("oauthdemo")
	if !ok {
		t.Fatal("本地层的声明没被加载")
	}
	ref := credstore.Ref{Integration: "oauthdemo", Account: "test-user"}
	expired := oauthdev.TokenSet{
		Marker:           oauthdev.TokenMarker,
		AccessToken:      "gho_stale",
		RefreshToken:     "ghr_first",
		ExpiresAt:        time.Now().Add(-time.Minute),
		RefreshExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	storedExpired, err := expired.Marshal()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := m.creds.Put(ref, storedExpired); err != nil {
		t.Fatalf("写凭据失败: %v", err)
	}
	if err := m.Store().Upsert(Instance{
		Type: "oauthdemo", Account: "test-user", Enabled: true, Settings: map[string]string{},
	}); err != nil {
		t.Fatalf("写实例失败: %v", err)
	}

	rt, err := m.httpRuntime(d, "test-user", nil)
	if err != nil {
		t.Fatalf("装配运行态失败: %v", err)
	}
	if _, _, err := (&httpchan.Client{HTTP: f.srv.Client()}).
		Call(context.Background(), rt, "oauthdemo_whoami", nil); err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	if got := f.gotBearer[len(f.gotBearer)-1]; got != "Bearer gho_refreshed" {
		t.Fatalf("业务请求带的是 %q，期望刷新后的令牌", got)
	}
	// 新令牌必须落库，否则下次又得刷一遍，而旧的刷新令牌可能已被轮换作废。
	after, err := m.creds.Secret(ref).Value()
	if err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	tok, isSet := oauthdev.Parse(after)
	if !isSet || tok.AccessToken != "gho_refreshed" || tok.RefreshToken != "ghr_next" {
		t.Fatalf("新令牌没有写回: %q", after)
	}
}

// TestHandFilledTokenStillWorks：同一个凭据槽也要能装用户手填的长期令牌，
// 原样当 bearer 用 —— 有的组织不允许给桌面程序注册 OAuth App。
func TestHandFilledTokenStillWorks(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, false)

	d, _ := ByType("oauthdemo")
	ref := credstore.Ref{Integration: "oauthdemo", Account: "pat-user"}
	if err := m.creds.Put(ref, "ghp_classicToken"); err != nil {
		t.Fatalf("写凭据失败: %v", err)
	}
	if err := m.Store().Upsert(Instance{
		Type: "oauthdemo", Account: "pat-user", Enabled: true, Settings: map[string]string{},
	}); err != nil {
		t.Fatalf("写实例失败: %v", err)
	}
	rt, err := m.httpRuntime(d, "pat-user", nil)
	if err != nil {
		t.Fatalf("装配运行态失败: %v", err)
	}
	if _, _, err := (&httpchan.Client{HTTP: f.srv.Client()}).
		Call(context.Background(), rt, "oauthdemo_whoami", nil); err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	if got := f.gotBearer[0]; got != "Bearer ghp_classicToken" {
		t.Fatalf("手填令牌没有原样使用: %q", got)
	}
}

// TestDeviceFlowDisabledIsActionable：最常见的配置错误要说清怎么改。
func TestDeviceFlowDisabledIsActionable(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{tokenError: "device_flow_disabled"})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, false)

	sess, err := m.StartConnect("oauthdemo", map[string]string{})
	if err != nil {
		t.Fatalf("StartConnect 失败: %v", err)
	}
	st := awaitState(t, m, sess.ID)
	if st.State != ConnectFailed {
		t.Fatalf("本该失败: %+v", st)
	}
	if !strings.Contains(st.Error, "Enable Device Flow") {
		t.Fatalf("错误信息没说怎么改: %q", st.Error)
	}
	// 失败不该留下半个渠道。
	if _, ok, _ := m.Store().Get("oauthdemo", "test-user"); ok {
		t.Fatal("授权失败却保存了实例")
	}
}

// TestClientIDFieldMustBeFilled：client id 由用户填的渠道，空着就要当场说，
// 而不是等轮询时报一个看不懂的错。
func TestClientIDFieldMustBeFilled(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, true)

	_, err := m.StartConnect("oauthdemo", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "请先填写") {
		t.Fatalf("err = %v，期望提示填 client id", err)
	}
	// 填上就能开始。
	sess, err := m.StartConnect("oauthdemo", map[string]string{"oauthClientId": "Iv1.filled"})
	if err != nil {
		t.Fatalf("填了 client id 还是失败: %v", err)
	}
	if st := awaitState(t, m, sess.ID); st.State != ConnectOK {
		t.Fatalf("授权没成功: %+v", st)
	}
}

// TestConnectRefusedWithoutOAuth：没有 oauth 声明的渠道不该有连接按钮，
// 后端也要拒绝。
func TestConnectRefusedWithoutOAuth(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{})
	m := newTestManager(t, f)
	if _, err := m.StartConnect("qqmail", map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "不支持") {
		t.Fatalf("err = %v，期望拒绝", err)
	}
}

// TestCancelStopsPolling：关掉弹窗就不该继续打授权服务。
func TestCancelStopsPolling(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{pollsBeforeOK: 1000})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, false)

	sess, err := m.StartConnect("oauthdemo", map[string]string{})
	if err != nil {
		t.Fatalf("StartConnect 失败: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // 让它至少轮一次
	m.CancelConnect(sess.ID)
	before := f.polls
	time.Sleep(2500 * time.Millisecond)
	if f.polls > before {
		t.Fatalf("取消后还在轮询：%d -> %d", before, f.polls)
	}
	if _, err := m.ConnectStatus(sess.ID); err == nil {
		t.Fatal("取消后会话还查得到")
	}
}

// TestAuthInfoFromStoredSecret 守住端给页面的令牌元数据：要够判断状态，
// 又不能带出任何一个字节的令牌。
//
// 「够判断状态」是有具体含义的：访问令牌只有两小时，所以单看 ExpiresAt
// 会把每一条正常连接都显示成快过期了。真正决定「要不要人来处理」的是
// CanRefresh —— 有刷新令牌就是程序自己能续，没有才要用户重新授权。
func TestAuthInfoFromStoredSecret(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

	t.Run("手工填的长期令牌没有元数据", func(t *testing.T) {
		if got := authInfo("ghp_abcdef123456", now); got != nil {
			t.Errorf("手填令牌不该有到期信息，实际 %+v", got)
		}
	})

	t.Run("可自动续期的令牌", func(t *testing.T) {
		stored := mustMarshalToken(t, oauthdev.TokenSet{
			Marker:       oauthdev.TokenMarker,
			AccessToken:  "at",
			RefreshToken: "rt",
			ObtainedAt:   now.Add(-30 * time.Minute),
			ExpiresAt:    now.Add(90 * time.Minute),
		})
		a := authInfo(stored, now)
		if a == nil {
			t.Fatal("令牌组应该端出元数据")
		}
		if !a.CanRefresh || a.Stale {
			t.Errorf("该是「有效且能自动续期」，实际 %+v", a)
		}
		if !a.ExpiresAt.Equal(now.Add(90 * time.Minute)) {
			t.Errorf("到期时间不对: %v", a.ExpiresAt)
		}
	})

	t.Run("过期但有刷新令牌不算故障", func(t *testing.T) {
		stored := mustMarshalToken(t, oauthdev.TokenSet{
			Marker:       oauthdev.TokenMarker,
			AccessToken:  "at",
			RefreshToken: "rt",
			ObtainedAt:   now.Add(-3 * time.Hour),
			ExpiresAt:    now.Add(-time.Hour),
		})
		a := authInfo(stored, now)
		if !a.Stale {
			t.Error("已过期却没标 Stale")
		}
		if !a.CanRefresh {
			t.Error("有刷新令牌却报告不能续期：页面会说这条连接要重新授权，而它其实会自己好")
		}
	})

	t.Run("过期且无刷新令牌才要人处理", func(t *testing.T) {
		stored := mustMarshalToken(t, oauthdev.TokenSet{
			Marker:      oauthdev.TokenMarker,
			AccessToken: "at",
			ObtainedAt:  now.Add(-3 * time.Hour),
			ExpiresAt:   now.Add(-time.Hour),
		})
		a := authInfo(stored, now)
		if !a.Stale || a.CanRefresh {
			t.Errorf("该是「已失效且无法自动续期」，实际 %+v", a)
		}
	})

	t.Run("元数据里不含任何令牌", func(t *testing.T) {
		stored := mustMarshalToken(t, oauthdev.TokenSet{
			Marker:       oauthdev.TokenMarker,
			AccessToken:  "secret-access-token",
			RefreshToken: "secret-refresh-token",
			ObtainedAt:   now,
			ExpiresAt:    now.Add(time.Hour),
		})
		b, err := json.Marshal(authInfo(stored, now))
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"secret-access-token", "secret-refresh-token"} {
			if strings.Contains(string(b), leak) {
				t.Fatalf("令牌泄漏到了页面数据里: %s", b)
			}
		}
	})
}

func mustMarshalToken(t *testing.T, tok oauthdev.TokenSet) string {
	t.Helper()
	s, err := tok.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
