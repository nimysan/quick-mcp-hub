package feishuweb

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func bp(v bool) *bool { return &v }

// 只有 copyable===true && exportable===true && isForceSetSecurity!==true 才放行；
// 字段缺失一律拒读。
func TestGate(t *testing.T) {
	cases := []struct {
		name  string
		flags *Flags
		want  string
	}{
		{"全部允许", &Flags{Copyable: bp(true), Exportable: bp(true), IsForceSetSecurity: bp(false)}, ""},
		{"安全标志缺失也放行", &Flags{Copyable: bp(true), Exportable: bp(true)}, ""},
		{"禁止复制", &Flags{Copyable: bp(false), Exportable: bp(true)}, ReasonCopyRestricted},
		{"禁止导出", &Flags{Copyable: bp(true), Exportable: bp(false)}, ReasonExportRestricted},
		{"两者都禁", &Flags{Copyable: bp(false), Exportable: bp(false)}, ReasonCopyRestricted},
		{"强制安全", &Flags{Copyable: bp(true), Exportable: bp(true), IsForceSetSecurity: bp(true)}, ReasonSecurityRestricted},
		{"copyable 缺失", &Flags{Exportable: bp(true)}, ReasonPermissionUnknown},
		{"exportable 缺失", &Flags{Copyable: bp(true)}, ReasonPermissionUnknown},
		{"没有标志", nil, ReasonPermissionUnknown},
	}
	for _, c := range cases {
		if got := Gate(c.flags); got != c.want {
			t.Errorf("%s: Gate = %q, want %q", c.name, got, c.want)
		}
	}
}

// 扩展把 null 传过来时要落成「缺失」，而不是 false 或 true。
func TestGateFromExtensionJSON(t *testing.T) {
	var raw RawResult
	if err := json.Unmarshal([]byte(`{"status":"ok","flags":{"copyable":null,"exportable":true,"isForceSetSecurity":null}}`), &raw); err != nil {
		t.Fatal(err)
	}
	if got := Gate(raw.Flags); got != ReasonPermissionUnknown {
		t.Errorf("Gate = %q", got)
	}
}

// 扩展说 ok 却带回了禁止复制的正文：mcp-hub 侧必须丢掉。
func TestBridgeReGatesExtensionResult(t *testing.T) {
	b := &Bridge{}
	res := b.finishRead("https://x.feishu.cn/docx/abcdefgh12", RawResult{
		Status: StatusOK,
		Flags:  &Flags{Copyable: bp(false), Exportable: bp(true)},
		Root:   &Block{Type: "page", Children: []*Block{{Type: "text", Ops: []Op{{Insert: "机密"}}}}},
	})
	if res.Status != StatusRefused || res.Reason != ReasonCopyRestricted || res.Markdown != "" {
		t.Fatalf("应当拒读：%+v", res)
	}
	if len(b.history) != 0 {
		t.Error("拒读的文档不该进最近列表")
	}
}

func TestValidateDocURL(t *testing.T) {
	ok := map[string]string{
		"https://abc.feishu.cn/docx/AbCdEf123456":              "https://abc.feishu.cn/docx/AbCdEf123456",
		"https://abc.feishu.cn/wiki/AbCdEf123456?from=share#x": "https://abc.feishu.cn/wiki/AbCdEf123456",
		"https://x.larksuite.com/docx/AbCdEf123456":            "https://x.larksuite.com/docx/AbCdEf123456",
		"https://x.y.larkoffice.com/wiki/AbCdEf123456/":        "https://x.y.larkoffice.com/wiki/AbCdEf123456",
	}
	for in, want := range ok {
		got, err := ValidateDocURL(in)
		if err != nil || got != want {
			t.Errorf("ValidateDocURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{
		"http://abc.feishu.cn/docx/AbCdEf123456",
		"https://feishu.cn/docx/AbCdEf123456",
		"https://abc.feishu.cn.evil.com/docx/AbCdEf123456",
		"https://evilfeishu.cn/docx/AbCdEf123456",
		"https://abc.feishu.cn/sheets/AbCdEf123456",
		"https://abc.feishu.cn/base/AbCdEf123456",
		"https://abc.feishu.cn/docx/",
		"https://abc.feishu.cn/docx/../../x",
		"https://u:p@abc.feishu.cn/docx/AbCdEf123456",
		"https://abc.feishu.cn:8443/docx/AbCdEf123456",
		"javascript:alert(1)",
	} {
		if _, err := ValidateDocURL(bad); err == nil {
			t.Errorf("ValidateDocURL(%q) 应当拒绝", bad)
		}
	}
}

type manifest struct {
	Key             string   `json:"key"`
	Permissions     []string `json:"permissions"`
	HostPermissions []string `json:"host_permissions"`
	ManifestVersion int      `json:"manifest_version"`
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	data, err := fs.ReadFile(ExtensionFiles(), "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 扩展 ID 由 manifest 的 key 推出（sha256 前 16 字节，每 4 位映射到 a-p），
// 桥接端点只认这个 ID。两边漂了扩展就永远连不上。
func TestExtensionIDMatchesKey(t *testing.T) {
	der, err := base64.StdEncoding.DecodeString(readManifest(t).Key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	var id strings.Builder
	for _, b := range sum[:16] {
		id.WriteByte('a' + b>>4)
		id.WriteByte('a' + b&0xf)
	}
	if id.String() != ExtensionID {
		t.Errorf("manifest key 推出的 ID 是 %s，代码里是 %s", id.String(), ExtensionID)
	}
}

// 权限只能是这些：不要 cookies、debugger、<all_urls>。
func TestExtensionPermissionsMinimal(t *testing.T) {
	m := readManifest(t)
	if m.ManifestVersion != 3 {
		t.Errorf("manifest_version = %d", m.ManifestVersion)
	}
	perms := append([]string(nil), m.Permissions...)
	sort.Strings(perms)
	if want := []string{"scripting", "storage", "tabs"}; !reflect.DeepEqual(perms, want) {
		t.Errorf("permissions = %v, want %v", perms, want)
	}
	hosts := append([]string(nil), m.HostPermissions...)
	sort.Strings(hosts)
	want := []string{"http://127.0.0.1/*", "https://*.feishu.cn/*", "https://*.larkoffice.com/*", "https://*.larksuite.com/*"}
	if !reflect.DeepEqual(hosts, want) {
		t.Errorf("host_permissions = %v, want %v", hosts, want)
	}
}

// 扩展代码里不该出现读 cookie、调飞书内部接口、绕过导出的东西。
func TestExtensionHasNoForbiddenCalls(t *testing.T) {
	_ = fs.WalkDir(ExtensionFiles(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		data, _ := fs.ReadFile(ExtensionFiles(), p)
		src := string(data)
		for _, bad := range []string{"chrome.cookies", "document.cookie", "chrome.debugger", "/space/api", "client_vars", "fetch(", "XMLHttpRequest"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s 里出现了 %q", p, bad)
			}
		}
		return nil
	})
}

// 统一错误形态（BUG-04）：参数错误、环境错误、业务状态各归其位。
func TestErrorResultStatuses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrNotConnected, StatusExtensionDisconnected},
		{ErrHubNotRunning, StatusExtensionDisconnected},
		{argErr("x"), StatusInvalidArgument},
		{errors.New("超时"), StatusError},
	} {
		if got := ErrorResult(c.err); got.Status != c.want || got.Message == "" {
			t.Errorf("ErrorResult(%v) = %+v, want %s", c.err, got, c.want)
		}
	}
	if _, err := ValidateDocURL("https://abc.feishu.cn/sheets/AbCdEf123456"); ErrorResult(err).Status != StatusUnsupportedType {
		t.Errorf("电子表格应当是 unsupported_type：%v", err)
	}
	if _, err := ValidateDocURL("https://evil.example/docx/AbCdEf123456"); ErrorResult(err).Status != StatusInvalidArgument {
		t.Errorf("外站应当是 invalid_argument：%v", err)
	}
	for st, want := range map[string]bool{
		StatusInvalidArgument: true, StatusExtensionDisconnected: true, StatusError: true,
		StatusOK: false, StatusNotFound: false, StatusNoPermission: false, StatusRefused: false,
		StatusLoginRequired: false, StatusStructureUnknown: false, StatusUnsupportedType: false,
	} {
		if IsErrorStatus(st) != want {
			t.Errorf("IsErrorStatus(%s) != %v", st, want)
		}
	}
}

// 假 token（BUG-02）：扩展回 not_found / no_permission 时原样交出，不再说成 unsupported_type；
// engine_missing 归成 structure_unknown；认不出的状态归成 error。
func TestFinishReadStatuses(t *testing.T) {
	b := &Bridge{}
	for st, want := range map[string]string{
		StatusNotFound:        StatusNotFound,
		StatusNoPermission:    StatusNoPermission,
		StatusLoginRequired:   StatusLoginRequired,
		StatusEngineMissing:   StatusStructureUnknown,
		StatusUnsupportedType: StatusUnsupportedType,
		"weird":               StatusError,
	} {
		r := b.finishRead("https://x.feishu.cn/docx/AbCdEf1234567890", RawResult{Status: st, Root: &Block{Type: "page"}})
		if r.Status != want || r.Message == "" || r.Markdown != "" || r.URL == "" {
			t.Errorf("%s: %+v", st, r)
		}
	}
	if len(b.history) != 0 {
		t.Error("没读成的文档不该进最近列表")
	}
}

// recent（BUG-03/06/07、NOTE-02）：空结果 items 是 []；标题去水印和后缀；
// 开着又读过的合成一条并标 readBefore；每条带 type；非文档地址丢掉。
func TestFinishRecent(t *testing.T) {
	b := &Bridge{}
	if r := b.finishRecent(RawResult{Status: StatusOK}); r.Status != StatusOK || r.Items == nil || len(r.Items) != 0 {
		t.Errorf("空结果不对：%+v", r)
	}
	if data, _ := json.Marshal(b.finishRecent(RawResult{Status: StatusOK})); !strings.Contains(string(data), `"status":"ok"`) {
		t.Errorf("序列化不对：%s", data)
	}
	b.remember(Item{Title: "读过的", URL: "https://x.feishu.cn/docx/AbCdEf1234567890", Source: "read_history"})
	r := b.finishRecent(RawResult{Status: StatusOK, Items: []Item{
		{Title: "\u200b\u2063\ufeff读过的 - Feishu Docs", URL: "https://x.feishu.cn/docx/AbCdEf1234567890", Source: "open_tab"},
		{Title: "说明书.pdf", URL: "https://x.feishu.cn/file/FileTok12345678", Source: "open_tab"},
		{Title: "坏的", URL: "javascript:alert(1)", Source: "open_tab"},
	}})
	want := []Item{
		{Title: "读过的", URL: "https://x.feishu.cn/docx/AbCdEf1234567890", Type: "docx", Source: "open_tab", ReadBefore: true},
		{Title: "说明书.pdf", URL: "https://x.feishu.cn/file/FileTok12345678", Type: "file", Source: "open_tab"},
	}
	if !reflect.DeepEqual(r.Items, want) {
		t.Errorf("得到 %+v\n期望 %+v", r.Items, want)
	}
}
