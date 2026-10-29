package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 配置页面没有登录：用户在浏览器里打开的任何网页都能向 127.0.0.1:7801 发 POST。
// 这些端点现在能写 NapCat 的配置文件、还能起进程，所以跨站请求必须被挡住。
//
// 这条闸靠的是浏览器一定会带、而网页脚本无法伪造的两个请求头。
func TestGuardBlocksCrossSite(t *testing.T) {
	s := New(nil, "test")
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"恶意网页的 fetch：Sec-Fetch-Site 是 cross-site",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site 也不行：另一个本机端口不是我们",
			map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"Origin 是别人",
			map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"Origin 是另一个本机端口",
			map[string]string{"Origin": "http://127.0.0.1:9999"}, http.StatusForbidden},
		{"我们自己页面的 fetch",
			map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7801"}, http.StatusOK},
		{"Sec-Fetch-Site 为 none、没有 Origin（用户自己打开的）",
			map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"curl 这类非浏览器调用：两个头都没有",
			map[string]string{}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 用 detect 做靶子：它不依赖 Manager，放行时回 200 + ok:false，
			// 所以 403 和 200 的区别干净地只反映闸的判断。
			r := httptest.NewRequest(http.MethodPost, "/api/napcat/detect", strings.NewReader(`{}`))
			r.Host = "127.0.0.1:7801"
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Errorf("状态 %d，想要 %d（body: %s）", rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// 写文件、起进程这两个端点尤其要在闸后面 —— 这是整条闸存在的理由。
func TestGuardCoversNapcatEndpoints(t *testing.T) {
	s := New(nil, "test")
	for _, path := range []string{"/api/napcat/detect", "/api/napcat/configure", "/api/napcat/start",
		"/api/channels", "/api/channels/delete", "/api/oauth/start"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		r.Host = "127.0.0.1:7801"
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 跨站请求没被拦，状态 %d", path, rec.Code)
		}
	}
}

// ncPost 走真的 mux，带上我们自己页面会带的头。
func ncPost(t *testing.T, s *Server, path, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Host = "127.0.0.1:7801"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Origin", "http://127.0.0.1:7801")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 状态 %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s 回的不是 JSON: %v", path, err)
	}
	return out
}

func fakeInstall(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, "napcat", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"network":{"httpServers":[{"name":"HTTP","enable":false,"host":"0.0.0.0","port":3000,"token":""}]},"parseMultMsg":true}`
	if err := os.WriteFile(filepath.Join(cfgDir, "onebot11_10001.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "napcat.bat"), []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNapcatDetectAndConfigure(t *testing.T) {
	s := New(nil, "test")
	root := fakeInstall(t)
	dir, _ := json.Marshal(root)

	got := ncPost(t, s, "/api/napcat/detect", `{"dir":`+string(dir)+`}`)
	if got["ok"] != true {
		t.Fatalf("detect 失败: %v", got["error"])
	}
	in := got["install"].(map[string]any)
	accs := in["accounts"].([]any)
	if len(accs) != 1 || accs[0].(map[string]any)["qq"] != "10001" {
		t.Fatalf("accounts = %v", accs)
	}
	// 启动命令要原样端给用户看：两个发行包传 QQ 号的方式不一样，搞错的
	// 后果是又弹一次二维码。按本机系统算 —— Windows 那条形状由
	// napcat.TestStartCommandShapes 钉住。
	cmd, _ := got["command"].(string)
	if !strings.Contains(cmd, "-q 10001") {
		t.Errorf("command = %q，应当带上 QQ 号", cmd)
	}
	if runtime.GOOS == "windows" && !strings.Contains(cmd, "cmd.exe /c start NapCat") {
		t.Errorf("Windows 上必须经 cmd.exe，得到 %q", cmd)
	}

	got = ncPost(t, s, "/api/napcat/configure", `{"dir":`+string(dir)+`,"qq":"10001"}`)
	if got["ok"] != true {
		t.Fatalf("configure 失败: %v", got["error"])
	}
	res := got["result"].(map[string]any)
	if tok, _ := res["token"].(string); len(tok) != 32 {
		t.Errorf("token = %q", tok)
	}
	if res["endpoint"] != "http://127.0.0.1:3000" {
		t.Errorf("endpoint = %v", res["endpoint"])
	}
	if res["needsRestart"] != true {
		t.Error("端口上没人答，必须提示重启")
	}

	// 真的落到了磁盘上，而且 host 被收到回环。
	raw, err := os.ReadFile(filepath.Join(root, "napcat", "config", "onebot11_10001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	srv := doc["network"].(map[string]any)["httpServers"].([]any)[0].(map[string]any)
	if srv["enable"] != true || srv["host"] != "127.0.0.1" {
		t.Errorf("写出来的 httpServers = %v", srv)
	}
	if doc["parseMultMsg"] != true {
		t.Error("无关字段被弄丢了")
	}
}

// 目录不对时回的是一句能照着做的话，而不是 500。
func TestNapcatDetectBadDirIsNotAServerError(t *testing.T) {
	s := New(nil, "test")
	dir, _ := json.Marshal(t.TempDir())
	got := ncPost(t, s, "/api/napcat/detect", `{"dir":`+string(dir)+`}`)
	if got["ok"] != false {
		t.Fatal("空目录应当被拒")
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "onebot11") {
		t.Errorf("报错应当点出要找的文件，得到 %q", msg)
	}
}

// configure / start 不信前端给的文件路径或可执行路径：它们只收一个目录，
// 服务端自己重新 Detect。这里证明换个目录并不能让它去写别处。
func TestNapcatConfigureRefusesUnknownAccount(t *testing.T) {
	s := New(nil, "test")
	dir, _ := json.Marshal(fakeInstall(t))
	got := ncPost(t, s, "/api/napcat/configure", `{"dir":`+string(dir)+`,"qq":"99999"}`)
	if got["ok"] != false {
		t.Fatal("不存在的账号应当被拒")
	}
}
