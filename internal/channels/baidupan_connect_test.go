package channels

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/oauthdev"
)

// 隐式授权的整条链路：点连接 → 拿到授权页地址 → 用户粘回授权后的地址 →
// 用令牌问身份 → 凭据进密码管理器、实例落配置。跑的是内置的 baidupan.json，
// 只把两个主机换成仿真服务。
func newBaidupanManager(t *testing.T) (*Manager, *httptest.Server, *[]string) {
	t.Helper()
	var gotTokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/2.0/xpan/nas", func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("access_token")
		gotTokens = append(gotTokens, tok)
		w.Header().Set("Content-Type", "application/json")
		if tok != "121.good" {
			json.NewEncoder(w).Encode(map[string]any{"errno": -6, "errmsg": "身份验证失败"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"errno": 0, "errmsg": "succ", "baidu_name": "pan_user",
			"netdisk_name": "昵称", "uk": 7, "vip_type": 0})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	m := newTestManager(t, &fakeProvider{srv: srv})

	raw, err := os.ReadFile(filepath.Join("..", "httpchan", "catalog", "baidupan.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.NewReplacer("https://pan.baidu.com", srv.URL, "https://openapi.baidu.com", srv.URL).Replace(string(raw))
	dir, err := catalogDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "baidupan.json"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, srv, &gotTokens
}

func TestBaidupanImplicitConnect(t *testing.T) {
	m, srv, gotTokens := newBaidupanManager(t)

	d, ok := ByType("baidupan")
	if !ok || d.OAuth == nil || d.OAuth.Flow != httpchan.FlowImplicit {
		t.Fatalf("描述符要告诉页面这是隐式授权: %+v", d.OAuth)
	}

	sess, err := m.StartConnect("baidupan", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := m.ConnectStatus(sess.ID)
	if st.Phase != PhasePaste || st.State != ConnectPending {
		t.Fatalf("隐式授权开始后应当停在等粘贴: %+v", st)
	}
	link, err := url.Parse(sess.VerificationURIComplete)
	if err != nil || !strings.HasPrefix(sess.VerificationURIComplete, srv.URL+"/oauth/2.0/authorize?") {
		t.Fatalf("授权页地址不对: %q", sess.VerificationURIComplete)
	}
	q := link.Query()
	state := q.Get("state")
	if q.Get("response_type") != "token" || q.Get("redirect_uri") != "oob" || q.Get("scope") != "basic,netdisk" || state == "" {
		t.Fatalf("授权参数不对: %v", q)
	}

	// 粘错了：只报错，会话还在等。
	for _, bad := range []string{
		"https://openapi.baidu.com/oauth/2.0/login_success#access_token=121.good&state=别的会话",
		sess.VerificationURIComplete, // 没授权就把授权页本身的地址复制了回来
	} {
		if _, err := m.CompleteConnect(sess.ID, bad); err == nil {
			t.Fatalf("应当拒绝: %q", bad)
		}
	}
	if st, _ := m.ConnectStatus(sess.ID); st.State != ConnectPending {
		t.Fatalf("粘错不该结束会话: %+v", st)
	}
	if len(*gotTokens) != 0 {
		t.Fatal("粘错的内容不该拿去访问对方系统")
	}

	pasted := "https://openapi.baidu.com/oauth/2.0/login_success#expires_in=2592000&access_token=121.good&session_secret=&session_key=&scope=basic+netdisk&state=" + state
	st, err = m.CompleteConnect(sess.ID, pasted)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != ConnectOK || st.Account != "pan_user" {
		t.Fatalf("连接应当成功并问出账号名: %+v", st)
	}
	if got := *gotTokens; len(got) != 1 || got[0] != "121.good" {
		t.Fatalf("问身份时发出去的必须是裸令牌: %v", got)
	}

	if _, ok, err := m.Store().Get("baidupan", "pan_user"); err != nil || !ok {
		t.Fatalf("实例没保存: %v %v", ok, err)
	}
	v, err := m.creds.Secret(credstore.Ref{Integration: "baidupan", Account: "pan_user"}).Value()
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := oauthdev.Parse(v)
	if !ok || tok.AccessToken != "121.good" || tok.RefreshToken != "" {
		t.Fatalf("存的应当是令牌组: %+v", tok)
	}
	if d := time.Until(tok.ExpiresAt); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("过期时间应当是约 30 天后: %v", tok.ExpiresAt)
	}

	// 再交一次：会话已经结束，原样回终态，不会再保存一遍。
	st, err = m.CompleteConnect(sess.ID, pasted)
	if err != nil || st.State != ConnectOK {
		t.Fatalf("重复提交: %+v %v", st, err)
	}
	if len(*gotTokens) != 1 {
		t.Fatal("重复提交不该再走一遍身份查询")
	}
}

// 令牌对方不认：这次连接失败，报出百度的说明。
func TestBaidupanImplicitConnectBadToken(t *testing.T) {
	m, _, _ := newBaidupanManager(t)
	sess, err := m.StartConnect("baidupan", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.CompleteConnect(sess.ID, "121.revoked")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != ConnectFailed || !strings.Contains(st.Error, "身份验证失败") {
		t.Fatalf("应当失败并带出原因: %+v", st)
	}
}

// 设备流的会话不接受粘贴。
func TestCompleteConnectRejectsDeviceSessions(t *testing.T) {
	f := newFakeProvider(t, &fakeProvider{pollsBeforeOK: 100})
	m := newTestManager(t, f)
	writeLocalSpec(t, f, false)
	sess, err := m.StartConnect("oauthdemo", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.CancelConnect(sess.ID)
	if _, err := m.CompleteConnect(sess.ID, "https://x.test/#access_token=a"); err == nil {
		t.Fatal("设备流会话不该接受粘贴的令牌")
	}
}
