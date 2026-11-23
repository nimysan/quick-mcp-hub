package channels

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
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

// platformSource 取当前平台上的第一个来源：Mac 上是 Apple Mail，Windows 上是经典版 Outlook。
func platformSource(t *testing.T) string {
	t.Helper()
	if len(localmail.Sources) == 0 {
		t.Skip("这个平台上没有本机邮件来源")
	}
	return localmail.Sources[0].ID
}

// 本机邮件没有凭据：保存不能要求密码管理器里先有一条，默认也不能启用任何来源。
func TestLocalMailSaveWithoutSecret(t *testing.T) {
	m := newLocalTestManager(t)
	if _, ok := ByType(localMailType); !ok {
		t.Fatal("描述符里没有本机邮件渠道")
	}
	src := platformSource(t)
	in, err := m.Save(localMailType, map[string]string{
		src:                      "true",
		src + localMailDirSuffix: "/x/Data",
		localMailMaxBody:         "1000",
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
	if len(cfg.Sources) != 1 || cfg.Sources[0].ID != src || cfg.Sources[0].Dir != "/x/Data" {
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
	platformSource(t) // 没有来源的平台（Linux）上渠道不可添加，Save 本来就会拒绝
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
	src := platformSource(t)
	err := m.Test(context.Background(), localMailType, "", map[string]string{
		src:                      "true",
		src + localMailDirSuffix: filepath.Join(t.TempDir(), "nope"),
	})
	if err == nil {
		t.Fatal("目录不存在时 Test 应当失败")
	}
}

// 来源按平台显示：Mac 的三个来源只在 darwin 上有开关，Windows 的只在 windows 上有。
func TestLocalMailFieldsFollowPlatform(t *testing.T) {
	d := localMailDescriptor()
	bools := map[string]bool{}
	for _, f := range d.Fields {
		if f.Kind == KindBool {
			bools[f.Name] = true
		}
	}
	mac := []string{localmail.SourceAppleMail, localmail.SourceOutlookLegacy, localmail.SourceOutlook}
	win := []string{localmail.SourceOutlookClassicWin, localmail.SourceOutlookNewWin}
	want, hide := map[string][]string{"darwin": mac, "windows": win}[runtime.GOOS], append(append([]string{}, mac...), win...)
	for _, id := range want {
		if !bools[id] {
			t.Errorf("%s 上缺少来源开关 %s", runtime.GOOS, id)
		}
	}
	for _, id := range hide {
		if bools[id] && !containsID(want, id) {
			t.Errorf("%s 上不该出现来源开关 %s", runtime.GOOS, id)
		}
	}
	if d.Available != (len(want) > 0) || d.Platform != runtime.GOOS {
		t.Errorf("Available=%v Platform=%q", d.Available, d.Platform)
	}
	if runtime.GOOS == "windows" && strings.Contains(d.Summary, "Mac") {
		t.Errorf("Windows 上的说明不该提 Mac：%s", d.Summary)
	}
}

func containsID(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
