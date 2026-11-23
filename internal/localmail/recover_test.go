package localmail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// panicSource 的每个方法都 panic，模拟解析器在没见过的数据上越界。
type panicSource struct{}

func (panicSource) Folders(context.Context) ([]Folder, error) {
	var s []Folder
	_ = s[3] // 越界
	return nil, nil
}
func (panicSource) Search(context.Context, Query) (Page, error) { panic("机密邮件正文") }
func (panicSource) Get(context.Context, int64) (*Message, error) {
	var m *Message
	return m, errors.New(m.Body) // 空指针
}
func (panicSource) Close() error { return nil }

// M1：来源 panic 只让这次调用返回格式不支持的错误；进程不退出，其他来源照常可用；
// 重开重试一次仍然 panic 就停用，不再反复打开。
func TestHubRecoversSourcePanic(t *testing.T) {
	var opens atomic.Int32
	register(t,
		SourceInfo{ID: "boom", Name: "会崩的来源", Open: func(OpenOptions) (Source, error) {
			opens.Add(1)
			return panicSource{}, nil
		}},
		SourceInfo{ID: "ok", Name: "正常来源", Open: func(OpenOptions) (Source, error) {
			return &fakeSource{body: "你好"}, nil
		}},
	)
	h := NewHub(Config{Sources: []SourceConfig{{ID: "boom"}, {ID: "ok"}}})
	defer h.Close()
	ctx := context.Background()

	_, err := h.Folders(ctx, "boom")
	if !errors.Is(err, ErrUnsupported) || !errors.Is(err, errPanic) {
		t.Fatalf("panic 应转成格式不支持的工具错误，得到 %v", err)
	}
	if strings.Contains(err.Error(), "机密") {
		t.Error("错误里不应带 panic 值")
	}
	if n := opens.Load(); n != 2 {
		t.Errorf("panic 后应重开重试一次，打开了 %d 次", n)
	}
	for _, call := range []func() error{
		func() error { _, err := h.Search(ctx, "boom", Query{}); return err },
		func() error { _, err := h.Get(ctx, "boom", 1); return err },
	} {
		if err := call(); !errors.Is(err, ErrUnsupported) {
			t.Errorf("停用后应直接返回说明，得到 %v", err)
		}
	}
	if n := opens.Load(); n != 2 {
		t.Errorf("停用后不应再打开，打开了 %d 次", n)
	}

	if m, err := h.Get(ctx, "ok", 1); err != nil || m.Body != "你好" {
		t.Errorf("其他来源应不受影响：%v %v", m, err)
	}
	if _, err := h.Search(ctx, "ok", Query{}); err != nil {
		t.Errorf("其他来源应不受影响：%v", err)
	}
}

// 打开时 panic（在后台 goroutine 里）也不能带崩进程。
func TestHubRecoversOpenPanic(t *testing.T) {
	register(t, SourceInfo{ID: "boom", Name: "会崩的来源", Open: func(OpenOptions) (Source, error) {
		var m map[string]int
		m["x"] = 1 // 写 nil map
		return nil, nil
	}})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "boom"}}})
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.Folders(ctx, "boom"); !errors.Is(err, ErrUnsupported) || !errors.Is(err, errPanic) {
		t.Fatalf("打开时 panic 应转成工具错误，得到 %v", err)
	}
}

// flakySource 前 n 次 Folders 报格式不支持，之后正常（模拟读到客户端写了一半的页）。
type flakySource struct {
	fakeSource
	fails *atomic.Int32
}

func (f *flakySource) Folders(ctx context.Context) ([]Folder, error) {
	if f.fails.Add(-1) >= 0 {
		return nil, errors.Join(ErrUnsupported, errors.New("页校验不对"))
	}
	return []Folder{{ID: 1, Path: "收件箱"}}, nil
}

// M2：一次性的格式错误重开重试一次就过去，来源不停用。
func TestHubRetriesTransientUnsupported(t *testing.T) {
	var fails atomic.Int32
	fails.Store(1)
	var opens atomic.Int32
	register(t, SourceInfo{ID: "x", Name: "来源X", Open: func(OpenOptions) (Source, error) {
		opens.Add(1)
		return &flakySource{fails: &fails}, nil
	}})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "x"}}})
	defer h.Close()
	f, err := h.Folders(context.Background(), "x")
	if err != nil || len(f) != 1 {
		t.Fatalf("重试一次后应成功：%v %v", f, err)
	}
	if n := opens.Load(); n != 2 {
		t.Errorf("应重开一次，打开了 %d 次", n)
	}
	if _, err := h.Folders(context.Background(), "x"); err != nil {
		t.Errorf("来源不应停用：%v", err)
	}
}

// M2：停用之后，数据文件的大小或修改时间变了就解除停用、重新打开；没变就不打开。
func TestHubReenablesWhenDataFileChanges(t *testing.T) {
	file := filepath.Join(t.TempDir(), "store.dat")
	if err := os.WriteFile(file, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var broken atomic.Bool
	broken.Store(true)
	var opens atomic.Int32
	register(t, SourceInfo{ID: "x", Name: "来源X",
		Open: func(OpenOptions) (Source, error) {
			opens.Add(1)
			if broken.Load() {
				return nil, errors.Join(ErrUnsupported, errors.New("头部不认识"))
			}
			return &fakeSource{}, nil
		},
		Fingerprint: func(opt OpenOptions) string { return statSig(opt.Dir) },
	})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "x", Dir: file}}})
	defer h.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := h.Folders(ctx, "x"); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("第 %d 次应是格式不支持：%v", i+1, err)
		}
	}
	if n := opens.Load(); n != 2 {
		t.Fatalf("文件没变时停用后不应再打开，打开了 %d 次", n)
	}
	broken.Store(false)
	if err := os.WriteFile(file, []byte("v2 longer"), 0o600); err != nil { // 客户端改写了文件
		t.Fatal(err)
	}
	if _, err := h.Folders(ctx, "x"); err != nil {
		t.Fatalf("文件变了应自动恢复：%v", err)
	}
	if n := opens.Load(); n != 3 {
		t.Errorf("应重新打开一次，打开了 %d 次", n)
	}
}

// 五个真实来源都有签名函数，而且数据文件变了签名就变。
func TestSourceFingerprintsTrackDataFiles(t *testing.T) {
	write := func(t *testing.T, p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		info  SourceInfo
		setup func(t *testing.T, dir string) (opt OpenOptions, data string)
	}{
		{appleMailInfo, func(t *testing.T, d string) (OpenOptions, string) {
			p := filepath.Join(d, "V10", "MailData", "Envelope Index")
			write(t, p, "a")
			return OpenOptions{Dir: d}, p
		}},
		{outlookLegacyInfo, func(t *testing.T, d string) (OpenOptions, string) {
			p := filepath.Join(d, "Outlook.sqlite")
			write(t, p, "a")
			return OpenOptions{Dir: d}, p
		}},
		{outlookInfo, func(t *testing.T, d string) (OpenOptions, string) {
			p := filepath.Join(d, "HxStore.hxd")
			write(t, p, "a")
			return OpenOptions{Dir: d}, p
		}},
		{outlookWinInfo, func(t *testing.T, d string) (OpenOptions, string) {
			p := filepath.Join(d, "a@example.com.ost")
			write(t, p, "a")
			return OpenOptions{Dir: d}, p
		}},
		{outlookNewWinInfo, func(t *testing.T, d string) (OpenOptions, string) {
			p := filepath.Join(d, "https_outlook.office.com_0.indexeddb.leveldb", "000003.log")
			write(t, p, "a")
			return OpenOptions{Dir: d}, p
		}},
	}
	for _, c := range cases {
		t.Run(c.info.ID, func(t *testing.T) {
			if c.info.Fingerprint == nil {
				t.Fatal("没有签名函数：停用后不会自动恢复")
			}
			opt, data := c.setup(t, t.TempDir())
			a := c.info.Fingerprint(opt)
			if a == "" || a != c.info.Fingerprint(opt) {
				t.Fatalf("签名应非空且稳定：%q", a)
			}
			write(t, data, "longer")
			if b := c.info.Fingerprint(opt); b == a {
				t.Errorf("数据文件变了签名没变：%q", b)
			}
		})
	}
}
