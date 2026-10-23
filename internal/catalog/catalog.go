// Package catalog 把渠道声明的来源分成三层，按优先级合并：
//
//	本地私有  ~/.../mcp-hub/catalog/*.json     用户自建系统、作者本地调试
//	远端下发  ~/.../mcp-hub/catalog/remote.json  服务器发布的签名目录（缓存）
//	内置      go:embed                          编译进二进制，离线可用
//
// 优先级 本地 > 远端 > 内置：同一个 id 出现在多层时，靠上的那层生效，
// 并且在 -catalog-list 里看得出来是哪一层生效的 —— 「为什么我改了声明没效果」
// 必须能一眼查出来。
//
// 三条设计约束：
//
//  1. 启动不联网。远端目录只在显式 -catalog-sync 时抓取，MCP 服务启动只读
//     磁盘缓存。stdio 握手要是卡在一次网络请求上，客户端会直接判超时。
//  2. 拉不到不影响已配好的渠道。验签失败、文件损坏、网络不通都只是让这一层
//     失效并记一条错误，内置那层照常工作。
//  3. 签名覆盖的是「意思」而不是字节。验签前先把每份声明解析成 Spec 再重新
//     序列化，所以服务器换个 JSON 缩进不会让签名失效；而 Parse 拒绝未知字段，
//     所以也不存在「字段被悄悄丢掉后签名照过」。这两条互为前提。
package catalog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// Layer 是一份声明的来源层。
type Layer string

const (
	LayerLocal    Layer = "local"
	LayerRemote   Layer = "remote"
	LayerEmbedded Layer = "embedded"
)

// rank 决定同一个 id 冲突时谁生效，数字小的赢。
func (l Layer) rank() int {
	switch l {
	case LayerLocal:
		return 0
	case LayerRemote:
		return 1
	default:
		return 2
	}
}

// Entry 是一份生效的声明，带上它的来源。
type Entry struct {
	Spec  httpchan.Spec
	Layer Layer
	// File 是来源文件名（内置层为空），用于排查「哪个文件在生效」。
	File string
	// ShadowedBy 记录这份声明压住了哪些层的同 id 声明。
	Shadowed []Layer
}

// Revocation 是一条下架记录：某个 id 在某版本及以前都不再可用。
type Revocation struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Reason  string `json:"reason,omitempty"`
}

// Signature 是目录文件的签名。
type Signature struct {
	KeyID string `json:"kid"`
	Alg   string `json:"alg"`
	Sig   string `json:"sig"`
}

// Document 是远端下发的目录文件。
type Document struct {
	SchemaVersion int               `json:"schemaVersion"`
	GeneratedAt   time.Time         `json:"generatedAt"`
	Connectors    []json.RawMessage `json:"connectors"`
	Revoked       []Revocation      `json:"revoked,omitempty"`
	Signature     *Signature        `json:"signature,omitempty"`
}

// SchemaVersion 是本程序能理解的目录格式版本。
const SchemaVersion = 1

// TrustedKeys 是验签公钥，kid -> base64 编码的 ed25519 公钥，编译期固定。
//
// 默认是空的：没有内置信任锚的构建会拒绝一切远端目录，而不是无条件接受。
// 发布时用 -ldflags 注入，或在这里写死 —— 公钥不是秘密，写死没有问题，
// 写死才是重点（它是信任的起点，不能从网上取）。
var TrustedKeys = map[string]string{}

// Catalog 是合并后的目录。
type Catalog struct {
	entries []Entry
	revoked []Revocation
	notes   []string
	errs    []error
}

// Dir 返回本地目录所在的文件夹。
func Dir(namespace string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return filepath.Join(base, namespace, "catalog"), nil
}

// RemotePath 返回远端目录缓存文件的路径。
func RemotePath(namespace string) (string, error) {
	dir, err := Dir(namespace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "remote.json"), nil
}

// Load 读磁盘上的三层来源并合并。不联网。
func Load(namespace string) *Catalog {
	dir, err := Dir(namespace)
	if err != nil {
		c := LoadFrom("")
		c.errs = append(c.errs, err)
		return c
	}
	return LoadFrom(dir)
}

// LoadFrom 从指定目录加载本地层和远端缓存层。dir 为空表示只有内置层。
func LoadFrom(dir string) *Catalog {
	c := &Catalog{}

	// 内置层：一定可用，先铺底。
	for _, s := range httpchan.All() {
		c.add(Entry{Spec: s, Layer: LayerEmbedded})
	}
	c.errs = append(c.errs, httpchan.Errors()...)

	if dir == "" {
		sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].Spec.ID < c.entries[j].Spec.ID })
		return c
	}

	// 远端层：缓存文件 + 验签。
	if doc, path, err := readRemote(dir); err != nil {
		c.errs = append(c.errs, err)
	} else if doc != nil {
		specs, derrs := doc.specs()
		c.errs = append(c.errs, derrs...)
		for _, s := range specs {
			c.add(Entry{Spec: s, Layer: LayerRemote, File: filepath.Base(path)})
		}
		c.revoked = append(c.revoked, doc.Revoked...)
	}

	// 本地层：用户自己放的单份声明。
	for _, e := range readLocal(dir, &c.errs) {
		c.add(e)
	}

	c.applyRevocations()
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].Spec.ID < c.entries[j].Spec.ID })
	return c
}

// add 按优先级插入一份声明：同 id 时高优先层压住低优先层，
// 并把被压住的层记下来。
func (c *Catalog) add(e Entry) {
	for i := range c.entries {
		if c.entries[i].Spec.ID != e.Spec.ID {
			continue
		}
		old := c.entries[i]
		if e.Layer.rank() < old.Layer.rank() {
			e.Shadowed = append(append([]Layer{}, old.Shadowed...), old.Layer)
			c.entries[i] = e
		} else {
			c.entries[i].Shadowed = append(c.entries[i].Shadowed, e.Layer)
		}
		return
	}
	c.entries = append(c.entries, e)
}

// applyRevocations 执行下架：被下架的 id 在该版本及以前都不再可用。
// 用户已配好的实例会因此失去工具，但凭据不动 —— 下架往往只是接口暂时变了。
func (c *Catalog) applyRevocations() {
	if len(c.revoked) == 0 {
		return
	}
	kept := c.entries[:0]
	for _, e := range c.entries {
		dropped := false
		for _, r := range c.revoked {
			if r.ID == e.Spec.ID && e.Spec.Version <= r.Version {
				reason := r.Reason
				if reason == "" {
					reason = "未说明原因"
				}
				c.notes = append(c.notes, fmt.Sprintf(
					"渠道 %s v%d 已被下架（%s），本次不加载", e.Spec.ID, e.Spec.Version, reason))
				dropped = true
				break
			}
		}
		if !dropped {
			kept = append(kept, e)
		}
	}
	c.entries = kept
}

// Entries 返回全部生效的声明。
func (c *Catalog) Entries() []Entry {
	out := make([]Entry, len(c.entries))
	copy(out, c.entries)
	return out
}

// Specs 返回全部生效声明的 Spec。
func (c *Catalog) Specs() []httpchan.Spec {
	out := make([]httpchan.Spec, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Spec)
	}
	return out
}

// ByID 按渠道标识取生效的声明。
func (c *Catalog) ByID(id string) (httpchan.Spec, bool) {
	for _, e := range c.entries {
		if e.Spec.ID == id {
			return e.Spec, true
		}
	}
	return httpchan.Spec{}, false
}

// Errors 返回加载过程中被跳过的东西及原因。
func (c *Catalog) Errors() []error { return c.errs }

// Notes 返回需要让用户知道但不算错误的事（例如下架）。
func (c *Catalog) Notes() []string { return c.notes }

func readRemote(dir string) (*Document, string, error) {
	path := filepath.Join(dir, "remote.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("读取远端目录缓存失败: %w", err)
	}
	doc, err := ParseDocument(data)
	if err != nil {
		// 缓存坏了或签名不对：这一层失效，但不影响内置层。
		return nil, path, fmt.Errorf("远端目录缓存不可用（%s）: %w", path, err)
	}
	return doc, path, nil
}

func readLocal(dir string, errs *[]error) []Entry {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		*errs = append(*errs, err)
		return nil
	}
	sort.Strings(names)
	var out []Entry
	for _, name := range names {
		if filepath.Base(name) == "remote.json" {
			continue // 那是远端层，上面读过了
		}
		data, err := os.ReadFile(name)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("%s: %w", filepath.Base(name), err))
			continue
		}
		s, err := httpchan.Parse(data)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("%s: %w", filepath.Base(name), err))
			continue
		}
		out = append(out, Entry{Spec: s, Layer: LayerLocal, File: filepath.Base(name)})
	}
	return out
}

// ParseDocument 解析并验签一份目录文件。
func ParseDocument(data []byte) (*Document, error) {
	var doc Document
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	if doc.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("目录格式版本是 %d，本程序只认 %d（升级客户端）",
			doc.SchemaVersion, SchemaVersion)
	}
	if err := doc.verify(); err != nil {
		return nil, err
	}
	return &doc, nil
}

// signedPayload 是实际被签名的内容。
//
// Connectors 是解析后又重新序列化的 Spec，所以签名覆盖的是声明的「意思」，
// 不是服务器那边的 JSON 排版 —— 换个缩进不会让签名失效。
type signedPayload struct {
	SchemaVersion int             `json:"schemaVersion"`
	GeneratedAt   string          `json:"generatedAt"`
	Connectors    []httpchan.Spec `json:"connectors"`
	Revoked       []Revocation    `json:"revoked,omitempty"`
}

// Canonical 返回该文件应被签名的字节。服务器和客户端用同一个函数算。
func (d *Document) Canonical() ([]byte, error) {
	specs, errs := d.specs()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return json.Marshal(signedPayload{
		SchemaVersion: d.SchemaVersion,
		GeneratedAt:   d.GeneratedAt.UTC().Format(time.RFC3339),
		Connectors:    specs,
		Revoked:       d.Revoked,
	})
}

// specs 解析目录里的每份声明。每份都走 httpchan.Parse，和内置层同一套校验。
func (d *Document) specs() ([]httpchan.Spec, []error) {
	var out []httpchan.Spec
	var errs []error
	seen := map[string]bool{}
	for i, raw := range d.Connectors {
		s, err := httpchan.Parse(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("远端目录第 %d 条声明无效: %w", i+1, err))
			continue
		}
		if seen[s.ID] {
			errs = append(errs, fmt.Errorf("远端目录里 id %q 重复", s.ID))
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	return out, errs
}

func (d *Document) verify() error {
	if d.Signature == nil {
		return fmt.Errorf("目录没有签名，拒绝加载")
	}
	if d.Signature.Alg != "ed25519" {
		return fmt.Errorf("不支持的签名算法 %q", d.Signature.Alg)
	}
	if len(TrustedKeys) == 0 {
		return fmt.Errorf("这个构建没有内置验签公钥，无法接受远端目录")
	}
	pubB64, ok := TrustedKeys[d.Signature.KeyID]
	if !ok {
		keys := make([]string, 0, len(TrustedKeys))
		for k := range TrustedKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("签名用的密钥 %q 不在信任列表里（已知 %s）",
			d.Signature.KeyID, strings.Join(keys, ", "))
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("内置公钥 %q 格式不对", d.Signature.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(d.Signature.Sig)
	if err != nil {
		return fmt.Errorf("签名不是合法的 base64")
	}
	payload, err := d.Canonical()
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return fmt.Errorf("签名验证不通过（内容被改过，或签名与密钥 %q 不匹配）", d.Signature.KeyID)
	}
	return nil
}

// Sign 给一份目录签名，供发布侧使用（也让测试能造出合法目录）。
func Sign(d *Document, keyID string, priv ed25519.PrivateKey) error {
	d.SchemaVersion = SchemaVersion
	payload, err := d.Canonical()
	if err != nil {
		return err
	}
	d.Signature = &Signature{
		KeyID: keyID,
		Alg:   "ed25519",
		Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)),
	}
	return nil
}

// Sync 从服务器抓一份目录，验签通过后写入本地缓存。
//
// 这是唯一会联网的入口，由用户显式触发（-catalog-sync 或配置页面），
// 不在 MCP 服务启动路径上：stdio 握手卡在一次网络请求上会被客户端判超时。
//
// 验签失败时**不覆盖**现有缓存 —— 旧的、验过签的那份比一份来历不明的新文件
// 更可信。
func Sync(ctx context.Context, url, dir string) (*Document, string, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, "", fmt.Errorf("目录地址必须是 https（现在是 %q）", url)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mcp-hub")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("抓取目录失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("抓取目录返回 %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("读取目录失败: %w", err)
	}

	doc, err := ParseDocument(data)
	if err != nil {
		return nil, "", fmt.Errorf("目录不可用，已保留原有缓存: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("创建目录文件夹失败: %w", err)
	}
	path := filepath.Join(dir, "remote.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, "", fmt.Errorf("写入目录缓存失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, "", fmt.Errorf("替换目录缓存失败: %w", err)
	}
	return doc, path, nil
}
