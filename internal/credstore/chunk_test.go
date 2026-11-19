package credstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// 飞书令牌组的真实大小约 3.9KB。用这个量级测，而不是几十字节的假令牌 ——
// 分段的问题只在真实大小上出现。
func bigSecret(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("0123456789abcdefghijklmnopqrstuvwxyz")
	}
	return b.String()[:n]
}

func TestLargeSecretIsChunkedAndReadBack(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"feishu", "张三"}
	val := bigSecret(3900)
	if err := s.Put(ref, val); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 每一条都必须在 Windows 的 2560 字节上限之内。
	head, _ := keyring.Get("mcp-hub-test:feishu", ref.Account)
	n, ok := parseHead(head)
	if !ok || n != 2 {
		t.Fatalf("头条目应指向 2 段，实际 %q", head)
	}
	for i := 1; i <= n; i++ {
		part, err := keyring.Get("mcp-hub-test:feishu", chunkAccount(ref.Account, i))
		if err != nil {
			t.Fatalf("段 %d: %v", i, err)
		}
		if len(part) > 2560 {
			t.Fatalf("段 %d 有 %d 字节，超过 Windows 上限", i, len(part))
		}
	}
	// 用一个新的 Store 读，确认走的是后端而不是内存缓存。
	got, err := New("mcp-hub-test").Secret(ref).Value()
	if err != nil || got != val {
		t.Fatalf("读回不一致: err=%v len=%d", err, len(got))
	}
}

func TestSmallSecretStaysSingleEntry(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"mail", "a@qq.com"}
	if err := s.Put(ref, "short"); err != nil {
		t.Fatal(err)
	}
	if v, _ := keyring.Get("mcp-hub-test:mail", ref.Account); v != "short" {
		t.Fatalf("短凭据应原样存一条，实际 %q", v)
	}
	if _, err := keyring.Get("mcp-hub-test:mail", chunkAccount(ref.Account, 1)); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("短凭据不应产生分段")
	}
}

// 已经按旧方式存着的单条凭据（升级前写入的）必须照原样读出来。
func TestExistingSingleEntryStillReadable(t *testing.T) {
	newTestStore(t)
	if err := keyring.Set("mcp-hub-test:github", "octo", "ghp_legacy"); err != nil {
		t.Fatal(err)
	}
	got, err := New("mcp-hub-test").Secret(Ref{"github", "octo"}).Value()
	if err != nil || got != "ghp_legacy" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

// 从长变短（比如重新授权换成了手填令牌）时，旧的分段要清掉。
func TestShrinkingRemovesStaleChunks(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"feishu", "张三"}
	if err := s.Put(ref, bigSecret(5000)); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ref, bigSecret(2500)); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get("mcp-hub-test:feishu", chunkAccount(ref.Account, 3)); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("5000→2500 字节后第 3 段应被删除")
	}
	if err := s.Put(ref, "tiny"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := keyring.Get("mcp-hub-test:feishu", chunkAccount(ref.Account, i)); !errors.Is(err, keyring.ErrNotFound) {
			t.Fatalf("变成单条后第 %d 段应被删除", i)
		}
	}
	if got, _ := New("mcp-hub-test").Secret(ref).Value(); got != "tiny" {
		t.Fatalf("got %q", got)
	}
}

func TestDeleteRemovesAllChunks(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"feishu", "张三"}
	if err := s.Put(ref, bigSecret(4100)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 3; i++ {
		acc := ref.Account
		if i > 0 {
			acc = chunkAccount(ref.Account, i)
		}
		if _, err := keyring.Get("mcp-hub-test:feishu", acc); !errors.Is(err, keyring.ErrNotFound) {
			t.Fatalf("删除后 %s 仍在", acc)
		}
	}
}

// 缺了一段就报错，不能把半截凭据交出去。
func TestMissingChunkIsAnError(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"feishu", "张三"}
	if err := s.Put(ref, bigSecret(4100)); err != nil {
		t.Fatal(err)
	}
	if err := keyring.Delete("mcp-hub-test:feishu", chunkAccount(ref.Account, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := New("mcp-hub-test").Secret(ref).Value(); err == nil {
		t.Fatal("缺段时应报错")
	}
}
