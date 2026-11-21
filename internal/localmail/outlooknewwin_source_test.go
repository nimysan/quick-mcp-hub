package localmail

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nimysan/mcp-hub/internal/localmail/olkwin"
)

// fakeOlk 是一个假的新版 Outlook 快照。
type fakeOlk struct {
	folders []olkwin.Folder
	mails   []olkwin.Summary
	msgs    map[int64]*olkwin.Message
	stale   atomic.Bool
}

func (f *fakeOlk) Folders(context.Context) ([]olkwin.Folder, error) { return f.folders, nil }
func (f *fakeOlk) List(_ context.Context, q olkwin.Query) (olkwin.Page, error) {
	var hit []olkwin.Summary
	for _, m := range f.mails {
		if (q.FolderID == 0 || m.FolderID == q.FolderID) && strings.Contains(m.Subject, q.Subject) {
			hit = append(hit, m)
		}
	}
	p := olkwin.Page{Total: len(hit), NextOffset: -1}
	end := min(q.Offset+q.Limit, len(hit))
	if q.Offset < end {
		p.Items = hit[q.Offset:end]
	}
	if end < len(hit) {
		p.NextOffset = end
	}
	return p, nil
}
func (f *fakeOlk) Get(_ context.Context, id int64) (*olkwin.Message, error) {
	if m, ok := f.msgs[id]; ok {
		return m, nil
	}
	return nil, olkwin.ErrNotFound
}
func (f *fakeOlk) Stale() bool  { return f.stale.Load() }
func (f *fakeOlk) Close() error { return nil }

func newFakeOlk() *fakeOlk {
	recv := time.Date(2026, 11, 21, 21, 54, 0, 0, time.UTC)
	f := &fakeOlk{
		folders: []olkwin.Folder{
			{ID: 1, Path: "Top of Information Store", Special: "msgfolderroot", Mail: true},
			{ID: 2, Path: "Inbox", Special: "inbox", Mail: true, MessageCount: 2, UnreadCount: 1, ServerTotal: 3035, Account: "u@example.com"},
			{ID: 3, Path: "Notes", Special: "notes", Mail: false},
		},
		mails: []olkwin.Summary{
			{ID: 11, FolderID: 2, Subject: "mcp-hub-test 111", From: "Yiming Liang", Received: recv, HasAttachment: true},
			{ID: 12, FolderID: 2, Subject: "草稿", Sent: recv.Add(-time.Hour), Incomplete: []string{"received", "from", "sent"}},
		},
		msgs: map[int64]*olkwin.Message{},
	}
	f.msgs[11] = &olkwin.Message{Summary: f.mails[0], Body: "你好", BodySource: "html", MessageID: "<a@b>",
		Recipients:  []olkwin.Recipient{{Type: "to", Name: "张三", Address: "zs@example.com"}, {Type: "cc", Name: "李四"}},
		Attachments: []olkwin.Attachment{{Name: "报告.pdf", ContentType: "application/pdf", Size: 1339765}}}
	m12 := &olkwin.Message{Summary: f.mails[1], Body: "摘要", BodySource: "preview"}
	m12.Incomplete = append(append([]string{}, f.mails[1].Incomplete...), "body", "recipients")
	f.msgs[12] = m12
	return f
}

func stubOlk(t *testing.T, f *fakeOlk, err error) *atomic.Int32 {
	t.Helper()
	var opens atomic.Int32
	old := openOlkStore
	openOlkStore = func(string, olkwin.Options) (olkStore, error) {
		opens.Add(1)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	t.Cleanup(func() { openOlkStore = old })
	return &opens
}

func TestOutlookNewWin(t *testing.T) {
	f := newFakeOlk()
	opens := stubOlk(t, f, nil)
	register(t, outlookNewWinInfo) // 非 Windows 平台上 Sources 里没有它
	h := NewHub(Config{Sources: []SourceConfig{{ID: SourceOutlookNewWin, Dir: `C:\x\IndexedDB`}}})
	defer h.Close()
	ctx := context.Background()

	fs, err := h.Folders(ctx, SourceOutlookNewWin)
	if err != nil || len(fs) != 1 || fs[0].Path != "Inbox" || fs[0].Total != 2 || fs[0].Unread != 1 || fs[0].Special != "inbox" {
		t.Fatalf("Folders = %+v, %v（根文件夹和便笺不该出现）", fs, err)
	}
	p, err := h.Search(ctx, SourceOutlookNewWin, Query{Limit: 1})
	if err != nil || len(p.Messages) != 1 || !p.HasMore || p.NextOffset != 1 || p.Total != 2 {
		t.Fatalf("Search = %+v, %v", p, err)
	}
	if m := p.Messages[0]; m.Subject != "mcp-hub-test 111" || !m.HasAttachment || len(m.Incomplete) != 0 {
		t.Errorf("第一封 = %+v", m)
	}
	p, _ = h.Search(ctx, SourceOutlookNewWin, Query{Limit: 5, Offset: 1})
	if len(p.Messages) != 1 || p.HasMore {
		t.Fatalf("第二页 = %+v", p)
	}
	// 没有接收时间时用发送时间，incomplete 里不报 date；sent 在本包没有对应字段，不报
	if m := p.Messages[0]; m.Date.IsZero() || strings.Join(m.Incomplete, ",") != "from" {
		t.Errorf("草稿 date=%v incomplete=%v", m.Date, m.Incomplete)
	}

	g, err := h.Get(ctx, SourceOutlookNewWin, 11)
	if err != nil {
		t.Fatal(err)
	}
	if g.Body != "你好" || !strings.Contains(g.BodyNote, "HTML") || g.MessageID != "<a@b>" ||
		strings.Join(g.To, ",") != "张三 <zs@example.com>" || strings.Join(g.Cc, ",") != "李四" ||
		len(g.Attachments) != 1 || g.Attachments[0].Name != "报告.pdf" || g.Attachments[0].Size != 1339765 {
		t.Errorf("Get = %+v", g)
	}
	g, _ = h.Get(ctx, SourceOutlookNewWin, 12)
	if !strings.Contains(g.BodyNote, "摘要") || strings.Join(g.Incomplete, ",") != "from,body,recipients" {
		t.Errorf("只有摘要的信 = note %q incomplete %v", g.BodyNote, g.Incomplete)
	}
	if _, err := h.Get(ctx, SourceOutlookNewWin, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的信：%v", err)
	}

	// olk 写入了新数据：Stale 为真，下一次调用重开
	before := opens.Load()
	f.stale.Store(true)
	h.Folders(ctx, SourceOutlookNewWin)
	if opens.Load() == before {
		t.Error("Stale 之后应该重开")
	}
}

func TestOutlookNewWinErrors(t *testing.T) {
	register(t, outlookNewWinInfo)
	ctx := context.Background()

	// 没装或没登录：ErrNotFound，不停用
	stubOlk(t, nil, errors.Join(olkwin.ErrNotFound, errors.New("没有新版 Outlook 账户的离线库")))
	h := NewHub(Config{Sources: []SourceConfig{{ID: SourceOutlookNewWin}}})
	if _, err := h.Folders(ctx, SourceOutlookNewWin); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "新版 Outlook for Windows") {
		t.Errorf("未找到：%v", err)
	}
	h.Close()

	// 格式变了：ErrUnsupported，来源停用
	opens := stubOlk(t, nil, errors.Join(olkwin.ErrUnsupportedFormat, errors.New("V8 序列化版本 17")))
	h = NewHub(Config{Sources: []SourceConfig{{ID: SourceOutlookNewWin}}})
	defer h.Close()
	for i := 0; i < 2; i++ {
		_, err := h.Folders(ctx, SourceOutlookNewWin)
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "V8 序列化版本 17") {
			t.Errorf("第 %d 次：%v", i+1, err)
		}
	}
	if opens.Load() != 1 {
		t.Errorf("格式不支持后应停用，不再打开；打开了 %d 次", opens.Load())
	}

	// 被锁：提示不提「退出经典版 Outlook」
	got := Explain(outlookNewWinInfo, olkErr(errors.Join(olkwin.ErrLocked, errors.New("sharing violation"))))
	if !errors.Is(got, ErrLocked) || strings.Contains(got.Error(), "OUTLOOK.EXE") || !strings.Contains(got.Error(), "稍后重试") {
		t.Errorf("Explain(locked) = %v", got)
	}
}
