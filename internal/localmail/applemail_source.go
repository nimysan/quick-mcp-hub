package localmail

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/nimysan/mcp-hub/internal/localmail/applemail"
)

var appleMailInfo = SourceInfo{
	ID:   SourceAppleMail,
	Name: "Apple Mail",
	Help: "读 macOS「邮件」App 在本机的数据（~/Library/Mail）。" +
		"需要给 Amazon Quick 授「完全磁盘访问权限」：系统设置 → 隐私与安全性 → 完全磁盘访问权限，" +
		"加入 Amazon Quick 并打开开关，再完全退出并重新打开 Amazon Quick。没授权时工具会返回这段指引，其他来源照常可用",
	DirLabel: "Apple Mail 数据目录",
	DirHelp:  "留空用默认的 ~/Library/Mail，一般不用填",
	GOOS:     "darwin",
	Open:     openAppleMail,
	Fingerprint: func(opt OpenOptions) string {
		root := opt.Dir
		if root == "" {
			root = applemail.DefaultRoot()
		}
		ver, err := applemail.DetectVersion(root)
		if err != nil {
			return statSig(root)
		}
		db := filepath.Join(root, ver, "MailData", "Envelope Index")
		return statSig(db, db+"-wal")
	},
}

type appleSource struct {
	s       *applemail.Store
	maxBody int
}

func openAppleMail(opt OpenOptions) (Source, error) {
	s, err := applemail.Open(opt.Dir)
	if err != nil {
		return nil, appleErr(err)
	}
	return &appleSource{s: s, maxBody: opt.MaxBodyBytes}, nil
}

func appleErr(err error) error {
	switch {
	case errors.Is(err, applemail.ErrFullDiskAccess):
		return ErrAccess
	case errors.Is(err, applemail.ErrNotFound):
		return errors.Join(ErrNotFound, err)
	}
	return err
}

func (a *appleSource) Close() error { return a.s.Close() }

// Stale 见 applemail.Store.Stale：immutable 打开后 Mail.app 启动了，Hub 会重开。
func (a *appleSource) Stale() bool { return a.s.Stale() }

func (a *appleSource) Folders(ctx context.Context) ([]Folder, error) {
	boxes, err := a.s.ListMailboxes(ctx)
	if err != nil {
		return nil, appleErr(err)
	}
	out := make([]Folder, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, Folder{ID: b.ID, Path: b.Path, Account: b.Account, Total: b.Total, Unread: max(b.Unread, 0)})
	}
	return out, nil
}

func (a *appleSource) Search(ctx context.Context, q Query) (Page, error) {
	p, err := a.s.ListMessages(ctx, applemail.Query{
		MailboxID: q.FolderID, Since: q.Since, Until: q.Before,
		From: q.From, Subject: q.Subject, Limit: q.Limit, Offset: q.Offset,
	})
	if err != nil {
		return Page{}, appleErr(err)
	}
	out := Page{HasMore: p.HasMore, NextOffset: p.NextOffset, Messages: make([]Summary, 0, len(p.Messages))}
	for _, m := range p.Messages {
		out.Messages = append(out.Messages, appleSummary(m))
	}
	return out, nil
}

func appleSummary(m applemail.Summary) Summary {
	from := m.FromName
	if from == "" {
		from = m.From
	}
	return Summary{
		ID: m.ID, FolderID: m.MailboxID, Subject: m.Subject,
		From: from, FromAddress: m.From, Date: m.DateReceived,
		Read: m.Read, Preview: m.Snippet,
	}
}

func (a *appleSource) Get(ctx context.Context, id int64) (*Message, error) {
	m, err := a.s.GetMessage(ctx, id, a.maxBody)
	if err != nil {
		return nil, appleErr(err)
	}
	out := &Message{
		Summary: appleSummary(m.Summary), Cc: m.Cc, MessageID: m.MessageID,
		Body: m.Text, BodyTruncated: m.Truncated,
	}
	out.To = m.To
	out.HasAttachment = len(m.Attachments) > 0
	switch {
	case m.BodyError != "":
		out.BodyNote = "正文读不出：" + m.BodyError
	case m.Partial:
		out.BodyNote = "本机只存了部分内容（附件已被 Mail 剥离），正文可能不完整"
	}
	for _, at := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{Name: at.Name, Size: at.Size, ContentType: at.ContentType})
	}
	return out, nil
}
