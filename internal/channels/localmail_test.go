package channels

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/localmail"
)

func newLocalTestManager(t *testing.T) *Manager {
	t.Helper()
	keyring.MockInit()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData", "Roaming"))
	store, err := NewStore(config.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(store, credstore.New(config.Namespace))
}

// 本机邮件没有凭据：保存不能要求密码管理器里先有一条，默认也不能启用任何来源。
func TestLocalMailSaveWithoutSecret(t *testing.T) {
	m := newLocalTestManager(t)
	if _, ok := ByType(localMailType); !ok {
		t.Fatal("描述符里没有本机邮件渠道")
	}
	in, err := m.Save(localMailType, map[string]string{
		localmail.SourceOutlookLegacy:                      "true",
		localmail.SourceOutlookLegacy + localMailDirSuffix: "/x/Data",
		localMailMaxBody:                                   "1000",
	})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if in.Account != localMailDefaultName {
		t.Errorf("账号 = %q，想要默认名 %q", in.Account, localMailDefaultName)
	}
	cfg, ok, ignored, err := m.LocalMailConfig()
	if err != nil || !ok || len(ignored) != 0 {
		t.Fatalf("LocalMailConfig ok=%v ignored=%v err=%v", ok, ignored, err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].ID != localmail.SourceOutlookLegacy || cfg.Sources[0].Dir != "/x/Data" {
		t.Errorf("来源 = %+v", cfg.Sources)
	}
	if cfg.MaxBodyBytes != 1000 || cfg.MaxResults != localmail.DefaultMaxResults {
		t.Errorf("上限 = %d / %d", cfg.MaxBodyBytes, cfg.MaxResults)
	}
	views, err := m.List()
	if err != nil || len(views) != 1 {
		t.Fatalf("List = %v, %v", views, err)
	}
	if err := m.Delete(localMailType, in.Account); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
}

func TestLocalMailDefaultsOff(t *testing.T) {
	m := newLocalTestManager(t)
	if _, err := m.Save(localMailType, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	cfg, ok, _, _ := m.LocalMailConfig()
	if !ok || len(cfg.Sources) != 0 {
		t.Fatalf("默认应一个来源都不启用，得到 %+v", cfg.Sources)
	}
	// 一个来源都没选时测试要说清楚，而不是假装通过。
	if err := m.Test(context.Background(), localMailType, localMailDefaultName, nil); err == nil {
		t.Error("没有来源时 Test 应当报错")
	}
}

// 选了一个不存在的目录：Test 要失败（多半是填错了），不能当成权限问题放过去。
func TestLocalMailTestMissingDir(t *testing.T) {
	m := newLocalTestManager(t)
	err := m.Test(context.Background(), localMailType, "", map[string]string{
		localmail.SourceOutlookLegacy:                      "true",
		localmail.SourceOutlookLegacy + localMailDirSuffix: filepath.Join(t.TempDir(), "nope"),
	})
	if err == nil {
		t.Fatal("目录不存在时 Test 应当失败")
	}
}
