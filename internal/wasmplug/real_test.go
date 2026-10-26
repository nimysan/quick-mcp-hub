package wasmplug

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
)

// 真实环境验证：拿钥匙串里的 GitHub 令牌，让 wasm 插件打真的 api.github.com。
//
// 默认跳过 —— 它要网络、要已配好的凭据，不能进常规测试。打开方式：
//
//	MCPHUB_PLUGIN_REAL=1 GITHUB_ACCOUNT=nimysan go test ./internal/wasmplug/ -run Real -v
//
// 这是插件版的 catalog try：仿真服务能验实现，验不了「真实响应里那些字段
// 到底在不在」。
func TestRealGitHubThroughPlugin(t *testing.T) {
	if os.Getenv("MCPHUB_PLUGIN_REAL") != "1" {
		t.Skip("需要网络和真实凭据；设 MCPHUB_PLUGIN_REAL=1 开启")
	}
	account := os.Getenv("GITHUB_ACCOUNT")
	if account == "" {
		t.Skip("需要 GITHUB_ACCOUNT=<你的 GitHub 用户名>")
	}

	raw, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("读 manifest 失败: %v", err)
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("解析 manifest 失败: %v", err)
	}
	module, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("读模块失败: %v", err)
	}

	p, err := Open(context.Background(), man, module)
	if err != nil {
		t.Fatalf("加载插件失败: %v", err)
	}
	defer p.Close(context.Background())
	p.Log = func(s string) { t.Logf("[插件] %s", s) }

	// 凭据从钥匙串惰性取用，和正常运行时走同一条路。
	store := credstore.New(config.Namespace)
	sec := store.Secret(credstore.Ref{Integration: "github", Account: account})
	inst := Instance{
		Account:  account,
		Settings: map[string]string{"allowWrite": "false"}, // 真实环境不开写
		Secret:   sec.Value,
	}

	who, err := p.Test(context.Background(), inst)
	if err != nil {
		t.Fatalf("测连通失败: %v", err)
	}
	t.Logf("身份: %s", mustJSON(t, who))

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"github_whoami", nil},
		{"github_list_my_repos", map[string]any{"per_page": float64(3)}},
		{"github_search_repos", map[string]any{"q": "wazero language:go", "per_page": float64(2)}},
	} {
		out, meta, err := p.Call(context.Background(), inst, c.tool, c.args)
		if err != nil {
			t.Errorf("%s 失败: %v", c.tool, err)
			continue
		}
		t.Logf("%s -> %s  meta=%s", c.tool, mustJSON(t, out), mustJSON(t, meta))
	}

	// 写工具在真实环境下必须被开关拦住。
	if _, _, err := p.Call(context.Background(), inst, "github_create_issue", map[string]any{
		"owner": account, "repo": "quick-mcp-hub", "title": "不该出现",
	}); err == nil {
		t.Error("写开关关着，create_issue 竟然执行了")
	}

	t.Logf("已放行请求 %d 次，被沙箱拒绝 %v", p.Fetches(), p.Denied())
}
