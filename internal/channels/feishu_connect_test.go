package channels

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/oauthdev"
)

// 仿真飞书：一键建应用、设备授权、换令牌、刷新、身份和发消息，全在一台
// TLS 服务上。令牌故意做得和真实的一样大（约 1.9KB），这样钥匙串分段这条
// 路径在连接链路里也被走到。
type fakeFeishu struct {
	srv *httptest.Server

	mu          sync.Mutex
	registered  bool
	authed      bool
	appNotReady bool // 应用还没被管理员审批：出码被拒
	gotSend     map[string]any
	gotBearer   string
	refreshes   int
}

const (
	ffAppID     = "cli_fake"
	ffAppSecret = "secret_fake_123"
)

var ffAccess = "u-" + strings.Repeat("a", 1900)
var ffRefresh = "ur-" + strings.Repeat("r", 1900)

func newFakeFeishu(t *testing.T) *fakeFeishu {
	t.Helper()
	f := &fakeFeishu{}
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PostForm.Get("action") == "begin" {
			js(w, 200, map[string]any{"device_code": "reg-dev", "user_code": "REG-CODE",
				"verification_uri":          "https://open.feishu.cn/page/launcher",
				"verification_uri_complete": "https://open.feishu.cn/page/launcher?user_code=REG-CODE",
				"expires_in":                3600, "interval": 1})
			return
		}
		if !f.registered {
			js(w, 200, map[string]any{"error": "authorization_pending"})
			return
		}
		js(w, 200, map[string]any{"client_id": ffAppID, "client_secret": ffAppSecret,
			"user_info": map[string]any{"open_id": "ou_me", "tenant_brand": "feishu"}})
	})
	mux.HandleFunc("/oauth/v1/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if u, p, ok := r.BasicAuth(); !ok || u != ffAppID || p != ffAppSecret {
			js(w, 401, map[string]any{"error": "invalid_client"})
			return
		}
		if f.appNotReady {
			js(w, 400, map[string]any{"error": "unauthorized_client", "error_description": "app not published"})
			return
		}
		js(w, 200, map[string]any{"device_code": "user-dev", "user_code": "USER-CODE",
			"verification_uri":          "https://accounts.feishu.cn/oauth/v1/device/verify",
			"verification_uri_complete": "https://accounts.feishu.cn/oauth/v1/device/verify?user_code=USER-CODE",
			"expires_in":                600, "interval": 1})
	})
	mux.HandleFunc("/open-apis/authen/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		secret := ""
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			secret = b["client_secret"]
			if b["grant_type"] == "refresh_token" && secret == ffAppSecret {
				f.refreshes++
				js(w, 200, map[string]any{"code": 0, "access_token": "u-refreshed", "refresh_token": "ur-rotated",
					"token_type": "Bearer", "expires_in": 7200, "refresh_token_expires_in": 604800})
				return
			}
		} else {
			r.ParseForm()
			secret = r.PostForm.Get("client_secret")
		}
		if secret != ffAppSecret {
			js(w, 400, map[string]any{"code": 20050, "error": "invalid_client"})
			return
		}
		if !f.authed {
			js(w, 400, map[string]any{"code": 20064, "error": "authorization_pending"})
			return
		}
		js(w, 200, map[string]any{"code": 0, "access_token": ffAccess, "refresh_token": ffRefresh,
			"token_type": "Bearer", "expires_in": 7200, "refresh_token_expires_in": 604800})
	})
	mux.HandleFunc("/open-apis/authen/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gotBearer = r.Header.Get("Authorization")
		f.mu.Unlock()
		js(w, 200, map[string]any{"code": 0, "msg": "success",
			"data": map[string]any{"name": "张三", "open_id": "ou_me", "avatar_big": "https://x/very-long"}})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		json.Unmarshal(raw, &b)
		f.mu.Lock()
		f.gotSend = b
		f.mu.Unlock()
		js(w, 200, map[string]any{"code": 0, "msg": "success",
			"data": map[string]any{"message_id": "om_1", "chat_id": "oc_1", "sender": map[string]any{"sender_type": "user"}}})
	})
	mux.HandleFunc("/open-apis/im/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		// 飞书的业务失败：HTTP 200，code 非 0。
		js(w, 200, map[string]any{"code": 99991679, "msg": "应用未获取所需的用户授权"})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFeishu) set(fn func(*fakeFeishu)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// writeFeishuSpec 把内置的 feishu.json 改成指向仿真服务，放进目录本地层
// （本地层优先于内置层）。测的是真正要发布的那份声明，不是另写一份。
func writeFeishuSpec(t *testing.T, f *fakeFeishu) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "httpchan", "catalog", "feishu.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.NewReplacer(
		"https://open.feishu.cn", f.srv.URL,
		"https://accounts.feishu.cn", f.srv.URL,
	).Replace(string(raw))
	dir, err := catalogDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feishu.json"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFeishuManager(t *testing.T) (*Manager, *fakeFeishu) {
	t.Helper()
	f := newFakeFeishu(t)
	m := newTestManager(t, &fakeProvider{srv: f.srv})
	writeFeishuSpec(t, f)
	return m, f
}

// awaitPhase 等会话进入某个阶段（或终态）。
func awaitPhase(t *testing.T, m *Manager, id, phase string) ConnectStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := m.ConnectStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != ConnectPending || st.Phase == phase {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待阶段 %s 超时", phase)
	return ConnectStatus{}
}

func storedToken(t *testing.T, m *Manager, account string) oauthdev.TokenSet {
	t.Helper()
	v, err := m.creds.Secret(credstore.Ref{Integration: "feishu", Account: account}).Value()
	if err != nil {
		t.Fatalf("凭据没存进密码管理器: %v", err)
	}
	tok, ok := oauthdev.Parse(v)
	if !ok {
		t.Fatalf("存的不是令牌组")
	}
	return tok
}

// 一键连接：什么都不填，扫码建应用 → 扫码授权 → 保存。
func TestFeishuOneClickConnect(t *testing.T) {
	m, f := newFeishuManager(t)
	sess, err := m.StartConnect("feishu", map[string]string{})
	if err != nil {
		t.Fatalf("StartConnect: %v", err)
	}
	if sess.UserCode != "REG-CODE" || !strings.Contains(sess.VerificationURIComplete, "open.feishu.cn/page/launcher") {
		t.Fatalf("第一步应是建应用链接: %+v", sess)
	}
	st, _ := m.ConnectStatus(sess.ID)
	if st.Phase != PhaseRegister {
		t.Fatalf("phase = %q", st.Phase)
	}
	f.set(func(f *fakeFeishu) { f.registered = true })
	st = awaitPhase(t, m, sess.ID, PhaseAuthorize)
	if st.Phase != PhaseAuthorize || st.UserCode != "USER-CODE" || st.Fill["appId"] != ffAppID {
		t.Fatalf("第二步应切到个人授权并回填 App ID: %+v", st)
	}
	f.set(func(f *fakeFeishu) { f.authed = true })
	st = awaitState(t, m, sess.ID)
	if st.State != ConnectOK || st.Account != "张三" {
		t.Fatalf("连接没成功: %+v", st)
	}
	tok := storedToken(t, m, "张三")
	if tok.AccessToken != ffAccess || tok.ClientSecret != ffAppSecret {
		t.Fatal("令牌组里应带着访问令牌和应用密钥")
	}
	in, ok, _ := m.Store().Get("feishu", "张三")
	if !ok || in.Settings["appId"] != ffAppID {
		t.Fatalf("App ID 应落进实例设置: %+v", in)
	}
	// 应用密钥和令牌都不能出现在配置文件里。
	cfgRaw, _ := os.ReadFile(m.Store().Path())
	if strings.Contains(string(cfgRaw), ffAppSecret) || strings.Contains(string(cfgRaw), "u-aaaa") {
		t.Fatalf("凭据写进了配置文件: %s", cfgRaw)
	}
	// 授权成功后，「已建好待授权」的那条临时密钥要清掉。
	if _, err := m.creds.Secret(credstore.Ref{Integration: "feishu", Account: "app:" + ffAppID}).Value(); err == nil {
		t.Fatal("临时保存的应用密钥没有清掉")
	}
}

// 企业要求管理员审批时：应用建好了但授权开始不了。用户等审批通过后再点
// 一次连接（表单上已回填 App ID），必须接着授权，而不是再建一个应用。
func TestFeishuResumeAfterAdminApproval(t *testing.T) {
	m, f := newFeishuManager(t)
	f.set(func(f *fakeFeishu) { f.registered, f.appNotReady = true, true })
	sess, err := m.StartConnect("feishu", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	st := awaitState(t, m, sess.ID)
	if st.State != ConnectFailed || st.Fill["appId"] != ffAppID || !strings.Contains(st.Error, "审批") {
		t.Fatalf("应失败并回填 App ID、提示审批: %+v", st)
	}
	f.set(func(f *fakeFeishu) { f.appNotReady, f.authed = false, true })
	sess2, err := m.StartConnect("feishu", map[string]string{"appId": ffAppID})
	if err != nil {
		t.Fatalf("带着 App ID 再连应直接走授权: %v", err)
	}
	if sess2.UserCode != "USER-CODE" {
		t.Fatalf("不应再建应用: %+v", sess2)
	}
	if st := awaitState(t, m, sess2.ID); st.State != ConnectOK {
		t.Fatalf("续连没成功: %+v", st)
	}
	if storedToken(t, m, "张三").ClientSecret != ffAppSecret {
		t.Fatal("续连后令牌组里应带着应用密钥")
	}
}

// 用 IT 建好的应用：手填 App ID 和 App Secret。
func TestFeishuManualAppCredentials(t *testing.T) {
	m, f := newFeishuManager(t)
	f.set(func(f *fakeFeishu) { f.authed = true })
	if _, err := m.StartConnect("feishu", map[string]string{"appId": ffAppID}); err == nil || !strings.Contains(err.Error(), "App Secret") {
		t.Fatalf("只填 App ID 没有密钥时应要求填 App Secret，实际 %v", err)
	}
	sess, err := m.StartConnect("feishu", map[string]string{"appId": ffAppID, "appSecret": ffAppSecret})
	if err != nil {
		t.Fatal(err)
	}
	if st := awaitState(t, m, sess.ID); st.State != ConnectOK {
		t.Fatalf("%+v", st)
	}
	in, _, _ := m.Store().Get("feishu", "张三")
	if _, leaked := in.Settings["appSecret"]; leaked {
		t.Fatal("App Secret 进了实例设置")
	}
}

// 重新授权：详情页只带着已保存的设置和账号，密钥从已存的令牌组里取。
func TestFeishuReauthReusesStoredSecret(t *testing.T) {
	m, f := newFeishuManager(t)
	f.set(func(f *fakeFeishu) { f.authed = true })
	sess, _ := m.StartConnect("feishu", map[string]string{"appId": ffAppID, "appSecret": ffAppSecret})
	awaitState(t, m, sess.ID)
	sess2, err := m.StartConnect("feishu", map[string]string{"appId": ffAppID, "name": "张三"})
	if err != nil {
		t.Fatalf("重新授权不应要求重填密钥: %v", err)
	}
	if st := awaitState(t, m, sess2.ID); st.State != ConnectOK {
		t.Fatalf("%+v", st)
	}
}

// 工具调用：发消息的 content 被序列化成 JSON 文本；过期令牌带着应用密钥
// 刷新并写回；code 非 0 的业务失败报错而不是当成空结果。
func TestFeishuToolsAfterConnect(t *testing.T) {
	m, f := newFeishuManager(t)
	d, _ := ByType("feishu")
	ref := credstore.Ref{Integration: "feishu", Account: "张三"}
	expired := oauthdev.TokenSet{Marker: oauthdev.TokenMarker, AccessToken: ffAccess, RefreshToken: ffRefresh,
		ExpiresAt: time.Now().Add(-time.Minute), RefreshExpiresAt: time.Now().Add(24 * time.Hour), ClientSecret: ffAppSecret}
	v, _ := expired.Marshal()
	if err := m.creds.Put(ref, v); err != nil {
		t.Fatal(err)
	}
	if err := m.Store().Upsert(Instance{Type: "feishu", Account: "张三", Enabled: true,
		Settings: map[string]string{"appId": ffAppID, "allowWrite": "true"}}); err != nil {
		t.Fatal(err)
	}
	rt, err := m.httpRuntime(d, "张三", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &httpchan.Client{HTTP: f.srv.Client()}
	ctx := context.Background()
	out, _, err := c.Call(ctx, rt, "feishu_send_message", map[string]any{
		"receive_id_type": "open_id", "receive_id": "ou_other", "text": `你好 "引号"`})
	if err != nil {
		t.Fatalf("发消息失败: %v", err)
	}
	if !strings.Contains(fmt.Sprint(out), "user") {
		t.Fatalf("结果应带 sender_type: %v", out)
	}
	if f.refreshes != 1 {
		t.Fatalf("过期令牌应刷新一次，实际 %d 次", f.refreshes)
	}
	content, ok := f.gotSend["content"].(string)
	if !ok || content != `{"text":"你好 \"引号\""}` || f.gotSend["msg_type"] != "text" || f.gotSend["receive_id"] != "ou_other" {
		t.Fatalf("发消息请求体 = %v", f.gotSend)
	}
	after := storedToken(t, m, "张三")
	if after.AccessToken != "u-refreshed" || after.RefreshToken != "ur-rotated" || after.ClientSecret != ffAppSecret {
		t.Fatalf("刷新后的令牌组没写回或丢了应用密钥: %+v", after)
	}
	if _, _, err := c.Call(ctx, rt, "feishu_list_chats", nil); err == nil || !strings.Contains(err.Error(), "用户授权") {
		t.Fatalf("code 非 0 应报错并带上飞书的说明，实际 %v", err)
	}
}

// 详情页的权限开关只提交开关和账号。保存后 App ID 必须还在，否则令牌
// 到期续期时报「请先填写 App ID」。
func TestFeishuSwitchSaveKeepsAppID(t *testing.T) {
	m, f := newFeishuManager(t)
	f.set(func(f *fakeFeishu) { f.authed = true })
	sess, _ := m.StartConnect("feishu", map[string]string{"appId": ffAppID, "appSecret": ffAppSecret})
	if st := awaitState(t, m, sess.ID); st.State != ConnectOK {
		t.Fatalf("%+v", st)
	}
	if _, err := m.Save("feishu", map[string]string{"name": "张三", "allowWrite": "true"}); err != nil {
		t.Fatal(err)
	}
	in, _, _ := m.Store().Get("feishu", "张三")
	if in.Settings["appId"] != ffAppID || in.Settings["allowWrite"] != "true" {
		t.Fatalf("权限开关保存后设置 = %v", in.Settings)
	}
}

// feishuStale 放一份已过期、带应用密钥的令牌组和一条实例。
func feishuStale(t *testing.T, m *Manager, refresh string) {
	t.Helper()
	tok := oauthdev.TokenSet{Marker: oauthdev.TokenMarker, AccessToken: "u-stale", RefreshToken: refresh,
		ExpiresAt: time.Now().Add(-time.Minute), RefreshExpiresAt: time.Now().Add(24 * time.Hour), ClientSecret: ffAppSecret}
	v, _ := tok.Marshal()
	if err := m.creds.Put(credstore.Ref{Integration: "feishu", Account: "张三"}, v); err != nil {
		t.Fatal(err)
	}
	if err := m.Store().Upsert(Instance{Type: "feishu", Account: "张三", Enabled: true,
		Settings: map[string]string{"appId": ffAppID}}); err != nil {
		t.Fatal(err)
	}
}

// 另一个进程（配置页）已经续过期了：MCP 进程手里的缓存作废，必须改用
// 密码管理器里的新令牌，而不是拿作废的刷新令牌去续。
func TestFeishuUsesTokenRefreshedByOtherProcess(t *testing.T) {
	m, f := newFeishuManager(t)
	feishuStale(t, m, ffRefresh)
	d, _ := ByType("feishu")
	rt, _ := m.httpRuntime(d, "张三", nil)
	if _, err := rt.Secret(); err != nil { // 让本进程缓存下这份过期令牌
		t.Fatal(err)
	}
	other := oauthdev.TokenSet{Marker: oauthdev.TokenMarker, AccessToken: "u-from-ui", RefreshToken: "ur-from-ui",
		ExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(7 * 24 * time.Hour), ClientSecret: ffAppSecret}
	v, _ := other.Marshal()
	// 用另一个 Store 写：模拟另一个进程，本进程的缓存不知道这次写入。
	if err := credstore.New("mcp-hub").Put(credstore.Ref{Integration: "feishu", Account: "张三"}, v); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&httpchan.Client{HTTP: f.srv.Client()}).Call(context.Background(), rt, "feishu_whoami", nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if f.refreshes != 0 || f.gotBearer != "Bearer u-from-ui" {
		t.Fatalf("应直接用另一个进程续好的令牌：refreshes=%d bearer=%q", f.refreshes, f.gotBearer)
	}
}

// 多个工具调用同时撞上过期：只续一次，其余的用续好的结果。
func TestFeishuConcurrentRefreshOnce(t *testing.T) {
	m, f := newFeishuManager(t)
	feishuStale(t, m, ffRefresh)
	d, _ := ByType("feishu")
	rt, _ := m.httpRuntime(d, "张三", nil)
	c := &httpchan.Client{HTTP: f.srv.Client()}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := c.Call(context.Background(), rt, "feishu_whoami", nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发调用失败: %v", err)
		}
	}
	if f.refreshes != 1 {
		t.Fatalf("应只续期一次，实际 %d 次", f.refreshes)
	}
}

// 超过 7 天没用：说清楚要重新授权。
func TestFeishuRefreshTokenExpiredMessage(t *testing.T) {
	m, f := newFeishuManager(t)
	tok := oauthdev.TokenSet{Marker: oauthdev.TokenMarker, AccessToken: "u-old", RefreshToken: "ur-old",
		ExpiresAt: time.Now().Add(-8 * 24 * time.Hour), RefreshExpiresAt: time.Now().Add(-time.Hour), ClientSecret: ffAppSecret}
	v, _ := tok.Marshal()
	m.creds.Put(credstore.Ref{Integration: "feishu", Account: "张三"}, v)
	m.Store().Upsert(Instance{Type: "feishu", Account: "张三", Enabled: true, Settings: map[string]string{"appId": ffAppID}})
	d, _ := ByType("feishu")
	rt, _ := m.httpRuntime(d, "张三", nil)
	_, _, err := (&httpchan.Client{HTTP: f.srv.Client()}).Call(context.Background(), rt, "feishu_whoami", nil)
	if err == nil || !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("应提示重新授权，实际 %v", err)
	}
}

// 已有连接缺了 App ID（旧 bug 留下的）时点重新授权：报错让用户补，不能又建一个应用。
func TestFeishuReauthWithoutAppIDDoesNotRegister(t *testing.T) {
	m, _ := newFeishuManager(t)
	m.Store().Upsert(Instance{Type: "feishu", Account: "张三", Enabled: true, Settings: map[string]string{"allowWrite": "true"}})
	_, err := m.StartConnect("feishu", map[string]string{"name": "张三", "allowWrite": "true"})
	if err == nil || !strings.Contains(err.Error(), "App ID") {
		t.Fatalf("应要求先补 App ID，实际 %v", err)
	}
}

// 重新授权时页面只带账号，App ID 从已存的连接里取。
func TestFeishuReauthTakesAppIDFromInstance(t *testing.T) {
	m, f := newFeishuManager(t)
	f.set(func(f *fakeFeishu) { f.authed = true })
	feishuStale(t, m, ffRefresh)
	sess, err := m.StartConnect("feishu", map[string]string{"name": "张三"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserCode != "USER-CODE" {
		t.Fatalf("应直接走授权而不是建应用: %+v", sess)
	}
	if st := awaitState(t, m, sess.ID); st.State != ConnectOK {
		t.Fatalf("%+v", st)
	}
}

// fakeJWT 造一个载荷里带 client_id 的访问令牌（飞书的访问令牌是 JWT）。
func fakeJWT(clientID string) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "ES256"}) + "." + enc(map[string]string{"client_id": clientID}) + ".sig"
}

// App ID 填成了另一个应用（两个一键建的应用 ID 前缀几乎一样）：续期必须用
// 签发令牌的那个应用，保存时也要拦下并说出正确的 ID。
func TestFeishuWrongAppIDInSettings(t *testing.T) {
	m, f := newFeishuManager(t)
	// 旧令牌组：没记 ClientID，只能从 JWT 里读。
	tok := oauthdev.TokenSet{Marker: oauthdev.TokenMarker, AccessToken: fakeJWT(ffAppID), RefreshToken: ffRefresh,
		ExpiresAt: time.Now().Add(-time.Minute), RefreshExpiresAt: time.Now().Add(24 * time.Hour), ClientSecret: ffAppSecret}
	v, _ := tok.Marshal()
	m.creds.Put(credstore.Ref{Integration: "feishu", Account: "张三"}, v)
	m.Store().Upsert(Instance{Type: "feishu", Account: "张三", Enabled: true,
		Settings: map[string]string{"appId": "cli_wrong_other_app"}})

	d, _ := ByType("feishu")
	rt, _ := m.httpRuntime(d, "张三", nil)
	if _, _, err := (&httpchan.Client{HTTP: f.srv.Client()}).Call(context.Background(), rt, "feishu_whoami", nil); err != nil {
		t.Fatalf("应按令牌里的应用续期，不看填错的 App ID: %v", err)
	}
	if f.refreshes != 1 {
		t.Fatalf("refreshes = %d", f.refreshes)
	}
	// 续期后令牌组里记下了签发应用。
	if got := storedToken(t, m, "张三").ClientID; got != ffAppID {
		t.Fatalf("续期后 ClientID = %q", got)
	}
	// 保存时填错的 App ID 被改成签发令牌的那个；只提交开关时也一样。
	for _, vals := range []map[string]string{
		{"name": "张三", "appId": "cli_wrong_other_app"},
		{"name": "张三", "allowWrite": "true"},
	} {
		if _, err := m.Save("feishu", vals); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		if in, _, _ := m.Store().Get("feishu", "张三"); in.Settings["appId"] != ffAppID {
			t.Fatalf("保存后 App ID = %q，应改成签发令牌的 %s", in.Settings["appId"], ffAppID)
		}
	}
}

// 新增的日历、任务、多维表格工具：请求的方法、路径、参数和请求体形状。
func TestFeishuNewToolsRequestShape(t *testing.T) {
	var got struct {
		sync.Mutex
		method, path, query string
		body                map[string]any
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.Lock()
		got.method, got.path, got.query, got.body = r.Method, r.URL.Path, r.URL.RawQuery, nil
		json.Unmarshal(raw, &got.body)
		got.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"msg":"success","data":{"items":[],"task":{"guid":"g1"},"event":{"event_id":"e1"},"attendees":[],"record":{"record_id":"r1"}}}`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	m, _ := newFeishuManager(t)
	tok := oauthdev.TokenSet{Marker: oauthdev.TokenMarker, AccessToken: "u-ok", ExpiresAt: time.Now().Add(time.Hour), ClientSecret: ffAppSecret}
	v, _ := tok.Marshal()
	m.creds.Put(credstore.Ref{Integration: "feishu", Account: "张三"}, v)
	m.Store().Upsert(Instance{Type: "feishu", Account: "张三", Enabled: true, Settings: map[string]string{"appId": ffAppID, "allowWrite": "true"}})
	d, _ := ByType("feishu")
	rt, _ := m.httpRuntime(d, "张三", nil)
	rt.Spec.BaseURL = srv.URL
	c := &httpchan.Client{HTTP: srv.Client()}
	call := func(tool string, args map[string]any) {
		t.Helper()
		if _, _, err := c.Call(context.Background(), rt, tool, args); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
	}
	loc := time.Local
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, loc)

	call("feishu_calendar_agenda", map[string]any{"start_time": "2026-11-20", "end_time": "2026-11-21"})
	if got.method != "GET" || got.path != "/open-apis/calendar/v4/calendars/primary/events/instance_view" ||
		!strings.Contains(got.query, fmt.Sprintf("start_time=%d", start.Unix())) {
		t.Fatalf("agenda: %s %s ?%s", got.method, got.path, got.query)
	}

	call("feishu_calendar_create_event", map[string]any{"summary": "周会", "start_time": "2026-11-20 15:00", "end_time": "2026-11-20 16:00"})
	st, _ := got.body["start_time"].(map[string]any)
	if st["timestamp"] != fmt.Sprint(start.Add(15*time.Hour).Unix()) || got.body["summary"] != "周会" {
		t.Fatalf("create_event body: %v", got.body)
	}

	call("feishu_calendar_add_attendees", map[string]any{"event_id": "e1", "user_ids": []any{"ou_a", "ou_b"}})
	att, _ := got.body["attendees"].([]any)
	if len(att) != 2 || att[0].(map[string]any)["type"] != "user" || att[1].(map[string]any)["user_id"] != "ou_b" || got.body["need_notification"] != true {
		t.Fatalf("add_attendees body: %v", got.body)
	}

	call("feishu_task_create", map[string]any{"summary": "交报表", "due": "2026-11-20", "assignees": []any{"ou_a"}})
	due, _ := got.body["due"].(map[string]any)
	mem, _ := got.body["members"].([]any)
	if due["timestamp"] != fmt.Sprint(start.UnixMilli()) || len(mem) != 1 || mem[0].(map[string]any)["role"] != "assignee" {
		t.Fatalf("task_create body: %v", got.body)
	}

	before := time.Now().UnixMilli()
	call("feishu_task_complete", map[string]any{"task_guid": "g1"})
	task, _ := got.body["task"].(map[string]any)
	ca, _ := task["completed_at"].(string)
	if got.method != "PATCH" || got.path != "/open-apis/task/v2/tasks/g1" || ca == "" || ca < fmt.Sprint(before) {
		t.Fatalf("task_complete: %s %s %v", got.method, got.path, got.body)
	}

	call("feishu_bitable_search_records", map[string]any{"app_token": "bas1", "table_id": "tbl1",
		"filter": map[string]any{"conjunction": "and", "conditions": []any{}}, "field_names": []any{"客户"}})
	if got.path != "/open-apis/bitable/v1/apps/bas1/tables/tbl1/records/search" || got.body["filter"] == nil {
		t.Fatalf("bitable search: %s %v", got.path, got.body)
	}

	call("feishu_bitable_update_record", map[string]any{"app_token": "bas1", "table_id": "tbl1", "record_id": "r1", "fields": map[string]any{"状态": "已发货"}})
	if got.method != "PUT" || got.body["fields"].(map[string]any)["状态"] != "已发货" {
		t.Fatalf("bitable update: %s %v", got.method, got.body)
	}
}
