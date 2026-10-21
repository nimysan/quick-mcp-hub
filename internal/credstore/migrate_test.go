package credstore

import (
	"errors"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestMigrateFrom(t *testing.T) {
	keyring.MockInit()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	oldStore := New("oldname")
	ref := Ref{"mail", "a@qq.com"}
	if err := oldStore.Put(ref, "authcode"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	newStore := New("newname")
	moved, err := newStore.MigrateFrom("oldname")
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}

	// 新 namespace 下读得到。
	got, err := newStore.Secret(ref).Value()
	if err != nil {
		t.Fatalf("迁移后读取失败: %v", err)
	}
	if got != "authcode" {
		t.Fatalf("got %q", got)
	}

	// 旧条目和旧索引都已清掉。
	if _, err := keyring.Get("oldname:mail", ref.Account); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("旧钥匙串条目未删除: %v", err)
	}
	if _, err := os.Stat(oldStore.IndexPath()); !os.IsNotExist(err) {
		t.Fatalf("旧索引未删除: %v", err)
	}

	// 幂等：再跑一次不应报错，也不应搬任何东西（只是一次 stat）。
	moved, err = newStore.MigrateFrom("oldname")
	if err != nil || moved != 0 {
		t.Fatalf("重复迁移应为 0 条无错误, got %d / %v", moved, err)
	}
}

func TestMigrateFromNoopCases(t *testing.T) {
	keyring.MockInit()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	s := New("newname")
	for _, old := range []string{"", "newname", "never-existed"} {
		moved, err := s.MigrateFrom(old)
		if err != nil || moved != 0 {
			t.Fatalf("MigrateFrom(%q) = %d / %v, want 0 / nil", old, moved, err)
		}
	}
}
