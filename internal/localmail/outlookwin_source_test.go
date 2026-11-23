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

	"github.com/nimysan/mcp-hub/internal/localmail/outlookwin"
)

// fakeWin 是一个假的 .pst / .ost：mails 按接收时间倒序。
type fakeWin struct {
	folders []outlookwin.Folder
	mails   []outlookwin.Summary
	closed  atomic.Bool
}

func (f *fakeWin) Folders(context.Context) ([]outlookwin.Folder, error) { return f.folders, nil }
func (f *fakeWin) List(_ context.Context, q outlookwin.Query) (outlookwin.Page, error) {
	var hit []outlookwin.Summary
	for _, m := range f.mails {
		if (q.FolderID == 0 || m.FolderID == q.FolderID) && strings.Contains(m.Subject, q.Subject) {
			hit = append(hit, m)
		}
	}
	p := outlookwin.Page{Total: len(hit), NextOffset: -1}
	end := min(q.Offset+q.Limit, len(hit))
	if q.Offset < end {
		p.Items = hit[q.Offset:end]
	}
	if end < len(hit) {
		p.NextOffset = end
	}
	return p, nil
}
func (f *fakeWin) Get(_ context.Context, id int64) (*outlookwin.Message, error) {
	for _, m := range f.mails {
		if m.ID == id {
			return &outlookwin.Message{Summary: m, Body: "正文", BodySource: outlookwin.BodyHTML,
				Recipients:  []outlookwin.Recipient{{Type: "to", Name: "张三", Address: "zs@example.com"}, {Type: "cc", Name: "李四"}},
				Attachments: []outlookwin.Attachment{{Name: "a.txt", Size: 3}}}, nil
		}
	}
	return nil, outlookwin.ErrNotFound
}
func (f *fakeWin) Stale() bool  { return false }
func (f *fakeWin) Close() error { f.closed.Store(true); return nil }

func fakeFile(base time.Time, n int, step time.Duration) *fakeWin {
	f := &fakeWin{folders: []outlookwin.Folder{
		{ID: 0x8022, Path: "收件箱", Special: "inbox", Mail: true, MessageCount: n},
		{ID: 0x8042, Path: "日历", Mail: false},
	}}
	for i := 0; i < n; i++ {
		f.mails = append(f.mails, outlookwin.Summary{ID: int64(0x200024 + i*0x20), FolderID: 0x8022,
			Subject: "主题", Received: base.Add(-time.Duration(i) * step)})
	}
	return f
}

// stubWin 把 openWinStore 换成按路径给假文件或错误。opens 记录打开次数。
func stubWin(t *testing.T, files map[string]*fakeWin, errs map[string]error) *atomic.Int32 {
	t.Helper()
	var opens atomic.Int32
	old := openWinStore
	openWinStore = func(p string, _ outlookwin.Options) (winStore, error) {
		opens.Add(1)
		if err := errs[filepath.Base(p)]; err != nil {
			return nil, err
		}
		f := files[filepath.Base(p)]
		f.closed.Store(false)
		return f, nil
	}
	t.Cleanup(func() { openWinStore = old })
	return &opens
}

// touch 建空文件，返回分号连接的路径（配置里手动指定的写法）。
func touch(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return strings.Join(out, " ; ")
}

func TestOutlookWinAggregates(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a, b := fakeFile(now, 3, 2*time.Hour), fakeFile(now.Add(-time.Hour), 3, 2*time.Hour) // 交错：a0 b0 a1 b1 a2 b2
	stubWin(t, map[string]*fakeWin{"a@x.com.ost": a, "archive.pst": b}, nil)
	src, err := openOutlookWin(OpenOptions{Dir: touch(t, "a@x.com.ost", "archive.pst")})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ctx := context.Background()

	fs, err := src.Folders(ctx)
	if err != nil || len(fs) != 2 {
		t.Fatalf("Folders = %+v, %v（日历不该出现）", fs, err)
	}
	if fs[0].Path != "a@x.com/收件箱" || fs[0].Account != "a@x.com" || fs[1].ID != 1<<32|0x8022 || fs[1].Special != "inbox" {
		t.Errorf("文件夹 = %+v", fs)
	}

	p, err := src.Search(ctx, Query{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 6 || !p.HasMore || p.NextOffset != 4 || len(p.Messages) != 4 {
		t.Fatalf("第一页 total=%d more=%v next=%d n=%d", p.Total, p.HasMore, p.NextOffset, len(p.Messages))
	}
	for i := 1; i < len(p.Messages); i++ {
		if p.Messages[i].Date.After(p.Messages[i-1].Date) {
			t.Fatal("跨文件合并后没有按时间倒序")
		}
	}
	if p.Messages[1].ID>>32 != 1 || p.Messages[1].FolderID != 1<<32|0x8022 {
		t.Errorf("第二封应来自第二个文件：%+v", p.Messages[1])
	}
	p2, _ := src.Search(ctx, Query{Limit: 4, Offset: 4})
	if len(p2.Messages) != 2 || p2.HasMore {
		t.Errorf("第二页 n=%d more=%v", len(p2.Messages), p2.HasMore)
	}

	pf, err := src.Search(ctx, Query{FolderID: 1<<32 | 0x8022, Limit: 10})
	if err != nil || len(pf.Messages) != 3 || pf.Messages[0].ID>>32 != 1 {
		t.Errorf("按文件夹 = %+v, %v", pf, err)
	}
	if _, err := src.Search(ctx, Query{FolderID: 7<<32 | 0x8022, Limit: 10}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的文件序号应是 ErrNotFound，得到 %v", err)
	}

	m, err := src.Get(ctx, p.Messages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != p.Messages[1].ID || len(m.To) != 1 || !strings.Contains(m.To[0], "zs@example.com") || len(m.Cc) != 1 ||
		!m.HasAttachment || len(m.Attachments) != 1 || m.BodyNote == "" {
		t.Errorf("Get = %+v", m)
	}
	if _, err := src.Get(ctx, 0x200024+99*0x20); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的邮件应是 ErrNotFound，得到 %v", err)
	}
}

// 文件被占用：返回退出 Outlook 的提示；来源不停用，Outlook 退出后下一次调用就能读。
func TestOutlookWinLockedDoesNotDisable(t *testing.T) {
	now := time.Now()
	lock := errors.Join(outlookwin.ErrLocked, errors.New("sharing violation"))
	errs := map[string]error{"a.ost": lock}
	opens := stubWin(t, map[string]*fakeWin{"a.ost": fakeFile(now, 2, time.Hour), "b.pst": fakeFile(now, 1, time.Hour)}, errs)
	register(t, outlookWinInfo) // 非 Windows 平台上 Sources 里没有它
	h := NewHub(Config{Sources: []SourceConfig{{ID: SourceOutlookClassicWin, Dir: touch(t, "a.ost", "b.pst")}}})
	defer h.Close()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := h.Folders(ctx, SourceOutlookClassicWin)
		if !errors.Is(err, ErrLocked) || errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "退出") || !strings.Contains(err.Error(), "a.ost") {
			t.Fatalf("第 %d 次：%v", i+1, err)
		}
	}
	delete(errs, "a.ost") // Outlook 退出了
	fs, err := h.Folders(ctx, SourceOutlookClassicWin)
	if err != nil || len(fs) != 2 {
		t.Fatalf("解锁后 Folders = %d, %v", len(fs), err)
	}
	if opens.Load() < 5 {
		t.Errorf("被锁时不该缓存失败结果，打开次数 %d", opens.Load())
	}
}

func TestOutlookWinUnsupportedFiles(t *testing.T) {
	bad := errors.Join(outlookwin.ErrUnsupported, errors.New("EDP"))
	stubWin(t, map[string]*fakeWin{"ok.pst": fakeFile(time.Now(), 1, time.Hour)}, map[string]error{"edp.pst": bad})
	src, err := openOutlookWin(OpenOptions{Dir: touch(t, "edp.pst", "ok.pst")})
	if err != nil {
		t.Fatalf("一个文件不支持不该连累其他文件：%v", err)
	}
	p, err := src.Search(context.Background(), Query{Limit: 10})
	if err != nil || len(p.Messages) != 1 || p.Messages[0].ID>>32 != 1 {
		t.Errorf("Search = %+v, %v", p, err)
	}
	src.Close()
	if _, err := openOutlookWin(OpenOptions{Dir: touch(t, "edp.pst")}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("全都不支持应是 ErrUnsupported，得到 %v", err)
	}
}

// 空闲后关掉文件句柄（免得挡住 Outlook 启动），下一次调用自动重开。
func TestOutlookWinIdleClose(t *testing.T) {
	old := winIdleClose
	winIdleClose = 20 * time.Millisecond
	t.Cleanup(func() { winIdleClose = old })
	f := fakeFile(time.Now(), 1, time.Hour)
	opens := stubWin(t, map[string]*fakeWin{"a.ost": f}, nil)
	src, err := openOutlookWin(OpenOptions{Dir: touch(t, "a.ost")})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !f.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !f.closed.Load() {
		t.Fatal("空闲后没有关闭文件")
	}
	if _, err := src.Folders(context.Background()); err != nil || opens.Load() != 2 || f.closed.Load() {
		t.Fatalf("空闲关闭后应重开：opens=%d err=%v", opens.Load(), err)
	}
}

func TestOutlookWinPaths(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"x.OST", "y.pst", "note.txt"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o600)
	}
	one := filepath.Join(dir, "y.pst")
	got, err := winPaths(`"` + dir + `";` + one)
	if err != nil || len(got) != 2 {
		t.Fatalf("目录展开并去重：%v, %v", got, err)
	}
	if _, err := winPaths(filepath.Join(dir, "nope.pst")); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "nope.pst") {
		t.Errorf("不存在的路径：%v", err)
	}
	if _, err := winPaths(t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Errorf("空目录：%v", err)
	}
	old := discoverWinFiles
	t.Cleanup(func() { discoverWinFiles = old })
	discoverWinFiles = func() ([]outlookwin.DataFile, error) { return nil, nil }
	if _, err := winPaths(""); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "完整路径") {
		t.Errorf("自动发现没找到：%v", err)
	}
	discoverWinFiles = func() ([]outlookwin.DataFile, error) { return []outlookwin.DataFile{{Path: one}}, nil }
	if got, err := winPaths(" "); err != nil || len(got) != 1 || got[0] != one {
		t.Errorf("自动发现：%v, %v", got, err)
	}
}

// OUTLOOKWIN_SAMPLES=<目录> 时用真实样本走一遍适配器（Folders → Search 翻完 → 逐封 Get）。
func TestOutlookWinSamples(t *testing.T) {
	dir := os.Getenv("OUTLOOKWIN_SAMPLES")
	if dir == "" {
		t.Skip("设 OUTLOOKWIN_SAMPLES=<放 .pst/.ost 的目录> 才跑")
	}
	src, err := openOutlookWin(OpenOptions{Dir: dir, MaxBodyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ctx := context.Background()
	fs, err := src.Folders(ctx)
	if err != nil || len(fs) == 0 {
		t.Fatalf("Folders = %d, %v", len(fs), err)
	}
	n := 0
	for off := 0; ; {
		p, err := src.Search(ctx, Query{Limit: 50, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Messages {
			if _, err := src.Get(ctx, s.ID); err != nil {
				t.Errorf("Get(%d): %v", s.ID, err)
			}
			n++
		}
		if !p.HasMore {
			break
		}
		off = p.NextOffset
	}
	t.Logf("%d 个邮件文件夹，%d 封邮件", len(fs), n)
}
