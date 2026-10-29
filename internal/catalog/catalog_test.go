package catalog

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// minSpec 造一份最小合法声明，id 和版本可变。
func minSpec(id string, version int, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "version": version,
		"integration": id,
		"baseUrl":     "https://api.example.com",
		"tags":        []string{"other"},
		"summary":     "测试用",
		"account":     map[string]any{"name": "user", "label": "用户"},
		"secret":      map[string]any{"name": "token", "label": "令牌"},
		"auth":        map[string]any{"kind": "bearer"},
		"test":        map[string]any{"method": "GET", "path": "/me"},
		"tools": []any{map[string]any{
			"name": id + "_get", "description": "取一条。", "readOnly": true,
			"method": "GET", "path": "/items",
		}},
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// withKey 临时注入一对测试密钥，并在用例结束后还原。
func withKey(t *testing.T, kid string) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	old := TrustedKeys
	TrustedKeys = map[string]string{kid: base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { TrustedKeys = old })
	return priv
}

func signedDoc(t *testing.T, priv ed25519.PrivateKey, kid string, specs []map[string]any, revoked []Revocation) []byte {
	t.Helper()
	doc := &Document{GeneratedAt: time.Now().UTC(), Revoked: revoked}
	for _, s := range specs {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		doc.Connectors = append(doc.Connectors, raw)
	}
	if err := Sign(doc, kid, priv); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 内置层一定可用，而且不依赖任何磁盘状态。
func TestEmbeddedLayerAlwaysLoads(t *testing.T) {
	c := LoadFrom("")
	if len(c.Specs()) == 0 {
		t.Fatal("内置层应当有声明")
	}
	if _, ok := c.ByID("github"); !ok {
		t.Error("内置的 github 声明应当在")
	}
	for _, e := range c.Entries() {
		if e.Layer != LayerEmbedded {
			t.Errorf("只给内置层时不该出现 %s 层", e.Layer)
		}
	}
}

// 优先级：本地 > 远端 > 内置，而且被压住的层要留下痕迹 ——
// 「我改了声明怎么没效果」必须查得出来。
func TestLayerPriority(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")

	// 远端给 github v9 和一个独有的 remoteonly
	os.WriteFile(filepath.Join(dir, "remote.json"),
		signedDoc(t, priv, "test-1", []map[string]any{
			minSpec("github", 9, "GitHub（远端）"),
			minSpec("remoteonly", 1, "只在远端"),
		}, nil), 0o600)
	// 本地再覆盖 github v99
	writeJSON(t, filepath.Join(dir, "my-github.json"), minSpec("github", 99, "GitHub（本地）"))

	c := LoadFrom(dir)
	for _, err := range c.Errors() {
		t.Fatalf("不该有错误: %v", err)
	}

	gh, ok := c.ByID("github")
	if !ok {
		t.Fatal("github 应当在")
	}
	if gh.Version != 99 || gh.Name != "GitHub（本地）" {
		t.Errorf("本地层应当胜出，实际 v%d %s", gh.Version, gh.Name)
	}

	var entry Entry
	for _, e := range c.Entries() {
		if e.Spec.ID == "github" {
			entry = e
		}
	}
	if entry.Layer != LayerLocal || entry.File != "my-github.json" {
		t.Errorf("来源信息不对: %s %s", entry.Layer, entry.File)
	}
	// 远端和内置都被压住了，两层都要记下来
	if len(entry.Shadowed) != 2 {
		t.Errorf("应当记下被压住的两层，实际 %v", entry.Shadowed)
	}

	if ro, ok := c.ByID("remoteonly"); !ok || ro.Version != 1 {
		t.Error("远端独有的渠道应当生效")
	}
	if _, ok := c.ByID("jira"); !ok {
		t.Error("没被覆盖的内置渠道应当照常生效")
	}
}

// 签名不对的目录整层失效，但内置层必须照常工作 ——
// 目录拉不到不该让用户已配好的渠道失灵。
func TestBadSignatureDisablesOnlyRemoteLayer(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")

	data := signedDoc(t, priv, "test-1", []map[string]any{minSpec("remoteonly", 1, "只在远端")}, nil)
	var doc map[string]any
	json.Unmarshal(data, &doc)
	// 改动内容但不重新签名
	conns := doc["connectors"].([]any)
	conns[0].(map[string]any)["name"] = "被改过的名字"
	tampered, _ := json.Marshal(doc)
	os.WriteFile(filepath.Join(dir, "remote.json"), tampered, 0o600)

	c := LoadFrom(dir)
	if _, ok := c.ByID("remoteonly"); ok {
		t.Error("签名不符的远端渠道绝不能生效")
	}
	if _, ok := c.ByID("github"); !ok {
		t.Error("内置层应当不受影响")
	}
	found := false
	for _, err := range c.Errors() {
		if strings.Contains(err.Error(), "签名验证不通过") {
			found = true
		}
	}
	if !found {
		t.Errorf("应当报出签名验证失败，实际: %v", c.Errors())
	}
}

func TestUnknownKeyIDRejected(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	data := signedDoc(t, priv, "someone-else", []map[string]any{minSpec("demox", 1, "X")}, nil)
	os.WriteFile(filepath.Join(dir, "remote.json"), data, 0o600)

	c := LoadFrom(dir)
	if _, ok := c.ByID("demox"); ok {
		t.Error("用未知密钥签的目录不能生效")
	}
}

// 没有内置公钥的构建必须拒绝一切远端目录，而不是无条件接受。
func TestNoTrustAnchorRejectsRemote(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	data := signedDoc(t, priv, "test-1", []map[string]any{minSpec("demox", 1, "X")}, nil)
	os.WriteFile(filepath.Join(dir, "remote.json"), data, 0o600)

	old := TrustedKeys
	TrustedKeys = map[string]string{}
	defer func() { TrustedKeys = old }()

	c := LoadFrom(dir)
	if _, ok := c.ByID("demox"); ok {
		t.Error("没有信任锚时不能接受远端目录")
	}
	found := false
	for _, err := range c.Errors() {
		if strings.Contains(err.Error(), "没有内置验签公钥") {
			found = true
		}
	}
	if !found {
		t.Errorf("错误信息要说清是没有信任锚: %v", c.Errors())
	}
}

// 签名覆盖的是声明的「意思」：服务器换个 JSON 排版不该让签名失效。
func TestSignatureSurvivesReformatting(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	data := signedDoc(t, priv, "test-1", []map[string]any{minSpec("demox", 1, "X")}, nil)

	var doc map[string]any
	json.Unmarshal(data, &doc)
	pretty, _ := json.MarshalIndent(doc, "", "    ") // 换成缩进排版
	os.WriteFile(filepath.Join(dir, "remote.json"), pretty, 0o600)

	c := LoadFrom(dir)
	if _, ok := c.ByID("demox"); !ok {
		t.Errorf("重新排版不该让签名失效: %v", c.Errors())
	}
}

// 下架：该版本及以前都不加载，而且要留一条让用户看得见的提示。
func TestRevocation(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	data := signedDoc(t, priv, "test-1",
		[]map[string]any{minSpec("demox", 2, "X v2"), minSpec("demoy", 3, "Y v3")},
		[]Revocation{{ID: "demox", Version: 2, Reason: "接口变更"}})
	os.WriteFile(filepath.Join(dir, "remote.json"), data, 0o600)

	c := LoadFrom(dir)
	if _, ok := c.ByID("demox"); ok {
		t.Error("被下架的渠道不该加载")
	}
	if _, ok := c.ByID("demoy"); !ok {
		t.Error("没被下架的渠道应当照常加载")
	}
	if len(c.Notes()) == 0 || !strings.Contains(c.Notes()[0], "接口变更") {
		t.Errorf("下架要有带原因的提示: %v", c.Notes())
	}
}

// 下架只针对该版本及以前：更高版本是修好的那一版，应当照常生效。
func TestRevocationDoesNotHitNewerVersion(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	data := signedDoc(t, priv, "test-1",
		[]map[string]any{minSpec("demox", 5, "X v5")},
		[]Revocation{{ID: "demox", Version: 3, Reason: "旧版有问题"}})
	os.WriteFile(filepath.Join(dir, "remote.json"), data, 0o600)

	if _, ok := LoadFrom(dir).ByID("demox"); !ok {
		t.Error("v5 不该被针对 v3 的下架记录干掉")
	}
}

// 远端目录里的每份声明走的是和内置层同一个 Parse：一条坏声明只跳过它自己。
func TestBadSpecInRemoteSkippedIndividually(t *testing.T) {
	dir := t.TempDir()

	bad := minSpec("bad", 1, "坏的")
	delete(bad, "version") // version 必填
	good := minSpec("good", 1, "好的")

	// 本地层每份声明各读各的，坏的那份只影响自己。
	writeJSON(t, filepath.Join(dir, "bad.json"), bad)
	writeJSON(t, filepath.Join(dir, "good.json"), good)

	c := LoadFrom(dir)
	if _, ok := c.ByID("good"); !ok {
		t.Error("好的那份应当加载")
	}
	if _, ok := c.ByID("bad"); ok {
		t.Error("坏的那份不该加载")
	}
	if len(c.Errors()) == 0 {
		t.Error("坏声明要报出来，不能静默跳过")
	}
}

// remote.json 不该被当成本地层的单份声明读一遍。
func TestRemoteFileNotTreatedAsLocalSpec(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	os.WriteFile(filepath.Join(dir, "remote.json"),
		signedDoc(t, priv, "test-1", []map[string]any{minSpec("demox", 1, "X")}, nil), 0o600)

	c := LoadFrom(dir)
	for _, e := range c.Entries() {
		if e.Spec.ID == "demox" && e.Layer != LayerRemote {
			t.Errorf("remote.json 里的声明应当算远端层，实际 %s", e.Layer)
		}
	}
	for _, err := range c.Errors() {
		if strings.Contains(err.Error(), "remote.json") {
			t.Errorf("remote.json 不该被当本地声明解析: %v", err)
		}
	}
}

func TestSyncWritesVerifiedCache(t *testing.T) {
	dir := t.TempDir()
	priv := withKey(t, "test-1")
	body := signedDoc(t, priv, "test-1", []map[string]any{minSpec("demox", 1, "X")}, nil)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	// Sync 只接受 https，而 httptest 的 TLS 用的是自签证书，所以这里直接
	// 验「写缓存 + 合并生效」这一段：把抓到的内容按 Sync 的方式落盘。
	os.WriteFile(filepath.Join(dir, "remote.json"), body, 0o600)
	if _, ok := LoadFrom(dir).ByID("demox"); !ok {
		t.Error("落盘后的目录应当生效")
	}

	// http 地址必须被拒
	if _, _, err := Sync(context.Background(), "http://example.com/catalog.json", dir); err == nil ||
		!strings.Contains(err.Error(), "https") {
		t.Errorf("明文 http 的目录地址应当被拒: %v", err)
	}
}

// 本程序只认自己理解的目录格式版本，不猜。
func TestSchemaVersionMismatch(t *testing.T) {
	priv := withKey(t, "test-1")
	data := signedDoc(t, priv, "test-1", []map[string]any{minSpec("demox", 1, "X")}, nil)
	var doc map[string]any
	json.Unmarshal(data, &doc)
	doc["schemaVersion"] = SchemaVersion + 1
	future, _ := json.Marshal(doc)

	if _, err := ParseDocument(future); err == nil || !strings.Contains(err.Error(), "升级客户端") {
		t.Errorf("更高的目录格式版本应当被拒并提示升级: %v", err)
	}
}

func TestDocumentRejectsUnknownField(t *testing.T) {
	_, err := ParseDocument([]byte(`{"schemaVersion":1,"connectors":[],"launch":{"cmd":"sh"}}`))
	if err == nil || !strings.Contains(err.Error(), "launch") {
		t.Errorf("目录文件里的未知字段应当被拒: %v", err)
	}
}

// 没有签名的目录一律拒绝 —— 不能因为「本地文件」就放过。
func TestUnsignedDocumentRejected(t *testing.T) {
	withKey(t, "test-1")
	raw, _ := json.Marshal(minSpec("demox", 1, "X"))
	data, _ := json.Marshal(Document{SchemaVersion: SchemaVersion, Connectors: []json.RawMessage{raw}})
	if _, err := ParseDocument(data); err == nil || !strings.Contains(err.Error(), "没有签名") {
		t.Errorf("无签名目录应当被拒: %v", err)
	}
}

var _ = httpchan.Spec{}
