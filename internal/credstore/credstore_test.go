package credstore

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// newTestStore 用内存版 keyring 和临时 HOME，避免测试碰到真实钥匙串
// 和真实的用户配置目录。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())            // darwin / linux 的 UserConfigDir 基准
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // linux 优先读这个
	return New("mcp-hub-test")
}

func TestSecretIsLazy(t *testing.T) {
	s := newTestStore(t)
	sec := s.Secret(Ref{"mail", "a@qq.com"}, "TEST_PW_LAZY")

	if sec.Loaded() {
		t.Fatal("取得 handle 不应该访问后端")
	}
	t.Setenv("TEST_PW_LAZY", "from-env")
	if _, err := sec.Value(); err != nil {
		t.Fatalf("Value: %v", err)
	}
	if !sec.Loaded() {
		t.Fatal("Value 之后应标记为已加载")
	}
}

func TestKeychainWinsOverEnv(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"mail", "a@qq.com"}
	if err := s.Put(ref, "from-keychain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Setenv("TEST_PW_PRIO", "from-env")

	got, err := s.Secret(ref, "TEST_PW_PRIO").Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "from-keychain" {
		t.Fatalf("钥匙串应优先于环境变量，got %q", got)
	}
}

func TestEnvFallback(t *testing.T) {
	s := newTestStore(t)
	t.Setenv("TEST_PW_FALLBACK", "from-env")

	sec := s.Secret(Ref{"mail", "a@qq.com"}, "MISSING_VAR", "TEST_PW_FALLBACK")
	got, err := sec.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "from-env" {
		t.Fatalf("got %q, want from-env", got)
	}
	if sec.Source() != "env:TEST_PW_FALLBACK" {
		t.Fatalf("Source = %q", sec.Source())
	}
}

func TestNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Secret(Ref{"mail", "nobody@qq.com"}, "TEST_PW_ABSENT").Value()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// 凭据只读一次：进程内缓存，不会每次调用都查后端。
func TestValueCachedPerProcess(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"mail", "a@qq.com"}
	if err := s.Put(ref, "v1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	sec := s.Secret(ref)
	if v, _ := sec.Value(); v != "v1" {
		t.Fatalf("first read = %q", v)
	}

	// 后端改了，但已缓存的 handle 不应再去读。
	if err := s.Put(ref, "v2"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if v, _ := sec.Value(); v != "v1" {
		t.Fatalf("缓存失效，second read = %q, want v1", v)
	}
	// 新 Store 才会看到新值。
	if v, _ := New("mcp-hub-test").Secret(ref).Value(); v != "v2" {
		t.Fatalf("新进程应读到 v2, got %q", v)
	}
}

// 同一个 Ref 返回同一个 handle，缓存跨调用点共享。
func TestSecretHandleIsShared(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"mail", "a@qq.com"}
	if s.Secret(ref) != s.Secret(ref) {
		t.Fatal("同一个 Ref 应返回同一个 handle")
	}
	if s.Secret(ref) == s.Secret(Ref{"jira", "a@qq.com"}) {
		t.Fatal("不同集成应是不同 handle")
	}
}

func TestListAndDelete(t *testing.T) {
	s := newTestStore(t)
	mail := Ref{"mail", "a@qq.com"}
	jira := Ref{"jira", "yexw"}
	for _, r := range []Ref{mail, jira} {
		if err := s.Put(r, "x"); err != nil {
			t.Fatalf("Put %s: %v", r, err)
		}
	}

	items, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("List 返回 %d 条, want 2", len(items))
	}
	// 索引按集成名排序：jira < mail
	if items[0].Ref != jira || items[1].Ref != mail {
		t.Fatalf("排序不对: %+v", items)
	}

	if err := s.Delete(jira); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	items, _ = s.List()
	if len(items) != 1 || items[0].Ref != mail {
		t.Fatalf("删除后剩余 %+v", items)
	}
	// 删不存在的条目不应报错。
	if err := s.Delete(jira); err != nil {
		t.Fatalf("重复 Delete 应幂等: %v", err)
	}
}

func TestPutRejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ref{"mail", ""}, "x"); err == nil {
		t.Fatal("空账号应被拒绝")
	}
	if err := s.Put(Ref{"mail", "a@qq.com"}, ""); err == nil {
		t.Fatal("空凭据应被拒绝")
	}
}

// 错误信息里不能出现凭据内容。
func TestErrorsDoNotLeakSecret(t *testing.T) {
	s := newTestStore(t)
	ref := Ref{"mail", "a@qq.com"}
	const secret = "s3cr3t-authcode"
	if err := s.Put(ref, secret); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Secret(Ref{"mail", "other@qq.com"}).Value(); err == nil {
		t.Fatal("want error")
	} else if contains(err.Error(), secret) {
		t.Fatalf("错误信息泄露了凭据: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
