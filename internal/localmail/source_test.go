package localmail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// L2：普通 Unix 权限不足（EACCES）不能被说成隐私保护，也不能引导去开完全磁盘访问权限；
// TCC（EPERM）才给授权步骤。
func TestExplainTellsTCCFromUnixPermission(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("隐私保护（TCC）只在 macOS 上有")
	}
	info := SourceInfo{Name: "测试来源"}
	tcc := Explain(info, &fs.PathError{Op: "open", Path: "/x", Err: syscall.EPERM})
	if !errors.Is(tcc, ErrAccess) || !strings.Contains(tcc.Error(), "完全磁盘访问权限") {
		t.Errorf("EPERM 应给授权指引，得到 %v", tcc)
	}
	for _, err := range []error{
		&fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES},
		fmt.Errorf("包装: %w", &fs.PathError{Op: "stat", Path: "/y", Err: syscall.EACCES}),
	} {
		got := Explain(info, err)
		if errors.Is(got, ErrAccess) || !errors.Is(got, ErrFilePermission) ||
			!strings.Contains(got.Error(), "不是 macOS 隐私保护") || !strings.Contains(got.Error(), "/") {
			t.Errorf("EACCES 应说明是文件权限问题，得到 %v", got)
		}
	}
}

type fakeSource struct {
	body  string
	pages int
}

func (f *fakeSource) Folders(context.Context) ([]Folder, error) { return nil, nil }
func (f *fakeSource) Search(_ context.Context, q Query) (Page, error) {
	f.pages++
	var p Page
	for i := 0; i < q.Limit+5; i++ { // 故意多给，验证 Hub 兜底截断
		p.Messages = append(p.Messages, Summary{ID: int64(i), Preview: strings.Repeat("长", 500)})
	}
	return p, nil
}
func (f *fakeSource) Get(context.Context, int64) (*Message, error) {
	return &Message{Body: f.body}, nil
}
func (f *fakeSource) Close() error { return nil }

// withFake 临时注册一个假来源；opens 记录真正打开了几次。
func withFake(t *testing.T, openErr *error, src *fakeSource) *int {
	t.Helper()
	opens := 0
	old := Sources
	Sources = append(append([]SourceInfo{}, old...), SourceInfo{
		ID: "fake", Name: "假来源",
		Open: func(OpenOptions) (Source, error) {
			opens++
			if *openErr != nil {
				return nil, *openErr
			}
			return src, nil
		},
	})
	t.Cleanup(func() { Sources = old })
	return &opens
}

func TestHubLazyOpenRetriesAfterAccessError(t *testing.T) {
	openErr := error(&fs.PathError{Op: "open", Path: "/x", Err: syscall.EPERM}) // macOS 隐私保护拦截
	if runtime.GOOS != "darwin" {
		openErr = ErrAccess // 别的平台没有 TCC，EPERM 只是普通权限错误；这里测的是失败不缓存
	}
	opens := withFake(t, &openErr, &fakeSource{})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "fake"}}})
	if *opens != 0 {
		t.Fatal("NewHub 不应打开来源")
	}
	_, err := h.Folders(context.Background(), "fake")
	if !errors.Is(err, ErrAccess) || !strings.Contains(err.Error(), "完全磁盘访问权限") {
		t.Fatalf("权限错误应变成授权指引，得到 %v", err)
	}
	openErr = nil // 用户授权了
	if _, err := h.Folders(context.Background(), "fake"); err != nil {
		t.Fatalf("授权后应能打开: %v", err)
	}
	h.Folders(context.Background(), "fake")
	if *opens != 2 {
		t.Errorf("打开次数 = %d，失败不该缓存、成功应缓存", *opens)
	}
}

func TestHubLimits(t *testing.T) {
	var none error
	src := &fakeSource{body: strings.Repeat("中", 100)} // 300 字节
	withFake(t, &none, src)
	h := NewHub(Config{Sources: []SourceConfig{{ID: "fake"}}, MaxBodyBytes: 100, MaxResults: 10})

	p, err := h.Search(context.Background(), "fake", Query{Limit: 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 10 || !p.HasMore || p.NextOffset != 10 {
		t.Errorf("条数 %d has_more=%v next=%d，想要 10/true/10", len(p.Messages), p.HasMore, p.NextOffset)
	}
	if n := len([]rune(p.Messages[0].Preview)); n != maxPreviewRunes+1 {
		t.Errorf("摘要 %d 个字符", n)
	}

	m, err := h.Get(context.Background(), "fake", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !m.BodyTruncated || len(m.Body) != 99 || !strings.HasPrefix(src.body, m.Body) {
		t.Errorf("正文截断不对：len=%d truncated=%v", len(m.Body), m.BodyTruncated)
	}
}

func TestPick(t *testing.T) {
	h := NewHub(Config{Sources: []SourceConfig{{ID: SourceAppleMail}, {ID: SourceOutlookLegacy}}})
	if _, err := h.Pick(""); err == nil {
		t.Error("多个来源时不填 source 应报错")
	}
	if _, err := h.Pick(SourceOutlook); err == nil {
		t.Error("未启用的来源应报错")
	}
	if id, err := h.Pick(SourceAppleMail); err != nil || id != SourceAppleMail {
		t.Errorf("Pick = %q, %v", id, err)
	}
	one := NewHub(Config{Sources: []SourceConfig{{ID: SourceOutlookLegacy}}})
	if id, _ := one.Pick(""); id != SourceOutlookLegacy {
		t.Errorf("只有一个来源时应默认选它，得到 %q", id)
	}
	if NewHub(Config{MaxResults: 10000}).Config().MaxResults != HardMaxResults {
		t.Error("MaxResults 应被限在 HardMaxResults")
	}
}

// L4：格式不支持的说明是一行，「多半是」后面没有多余空格，「详情」只给读取包的原因。
func TestExplainUnsupportedIsOneLine(t *testing.T) {
	err := Explain(outlookInfo, outlookErr(fmt.Errorf("%w: 不是 Nostromo 容器", hxErrUnsupported())))
	msg := err.Error()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("应仍是 ErrUnsupported：%v", err)
	}
	if strings.Contains(msg, "\n") || strings.Contains(msg, "多半是 ") {
		t.Errorf("说明应是一行、没有多余空格：%q", msg)
	}
	if !strings.Contains(msg, "多半是「新版 Outlook for Mac」升级了") || !strings.HasSuffix(msg, "详情：新版 Outlook 存储格式不支持: 不是 Nostromo 容器") {
		t.Errorf("文案不对：%q", msg)
	}
	nf := Explain(appleMailInfo, errors.Join(ErrNotFound, errors.New("未找到: /x/Envelope Index")))
	if strings.Contains(nf.Error(), "\n") || !strings.HasSuffix(nf.Error(), "Apple Mail 未找到: /x/Envelope Index") {
		t.Errorf("未找到的说明应是一行：%q", nf.Error())
	}
}

// 格式不支持：重试一次后停用该来源、不再重试，也不影响其他来源。
func TestHubDisablesUnsupportedSource(t *testing.T) {
	openErr := error(errors.Join(ErrUnsupported, errors.New("自检不达标")))
	opens := withFake(t, &openErr, &fakeSource{})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "fake"}}})
	for i := 0; i < 3; i++ {
		_, err := h.Folders(context.Background(), "fake")
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "已停用") {
			t.Fatalf("应返回格式不支持并说明已停用，得到 %v", err)
		}
	}
	if *opens != 2 {
		t.Errorf("格式不支持时应只重试一次，打开了 %d 次", *opens)
	}
}

func TestOutlookAdapterMarksGaps(t *testing.T) {
	if !errors.Is(outlookErr(fmt.Errorf("x: %w", hxErrUnsupported())), ErrUnsupported) {
		t.Error("hxstore.ErrUnsupportedFormat 应映射到 ErrUnsupported")
	}
	if !errors.Is(outlookErr(hxErrNotFound()), ErrNotFound) {
		t.Error("hxstore.ErrNotFound 应映射到 ErrNotFound")
	}
	s := outlookSummaryOf(hxSummary("recipients"), outlookMessageGaps)
	for _, f := range []string{"recipients", "read", "has_attachment", "attachments"} {
		found := false
		for _, x := range s.Incomplete {
			found = found || x == f
		}
		if !found {
			t.Errorf("incomplete 缺 %s：%v", f, s.Incomplete)
		}
	}
}

// 来源按平台过滤：Mac 的三个只在 darwin，经典版 Outlook for Windows 只在 windows，别的平台一个没有。
func TestSourcesFor(t *testing.T) {
	ids := func(goos string) string {
		var out []string
		for _, s := range SourcesFor(goos) {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids("darwin"); got != "apple_mail,outlook_legacy,outlook" {
		t.Errorf("darwin = %s", got)
	}
	if got := ids("windows"); got != "outlook_classic_win,outlook_new_win" {
		t.Errorf("windows = %s", got)
	}
	if got := ids("linux"); got != "" {
		t.Errorf("linux = %s", got)
	}
	if ids(runtime.GOOS) != strings.Join(func() []string {
		var o []string
		for _, s := range Sources {
			o = append(o, s.ID)
		}
		return o
	}(), ",") {
		t.Error("Sources 和 SourcesFor(runtime.GOOS) 不一致")
	}
}

// 文件被占用：说明里要叫用户退出 Outlook，而且不能停用来源（下一次调用要重新打开）。
func TestExplainLocked(t *testing.T) {
	got := Explain(outlookWinInfo, errors.Join(ErrLocked, errors.New("a.ost 正被锁住")))
	if !errors.Is(got, ErrLocked) || errors.Is(got, ErrUnsupported) ||
		!strings.Contains(got.Error(), "退出") || !strings.Contains(got.Error(), "OUTLOOK.EXE") || !strings.Contains(got.Error(), "a.ost") {
		t.Errorf("Explain(locked) = %v", got)
	}
}
