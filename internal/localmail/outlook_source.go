package localmail

import (
	"context"
	"errors"

	"github.com/nimysan/mcp-hub/internal/localmail/hxstore"
)

var outlookInfo = SourceInfo{
	ID:   SourceOutlook,
	Name: "新版 Outlook for Mac",
	Help: "读新版 Outlook 在本机缓存的邮件（HxStore.hxd），只含已经同步到这台 Mac 的部分。" +
		"已读状态和附件读不出来，部分文件夹名和收件人可能缺失，工具结果里会标成 incomplete。" +
		"Outlook 升级后格式变了时，这个来源会自动停用并提示，不影响其他来源",
	DirLabel: "新版 Outlook 数据目录",
	DirHelp:  "留空用 Main Profile；用了别的配置文件时填 …/Outlook 15 Profiles/<配置文件名>，或直接填 HxStore.hxd 的路径",
	Open:     openOutlook,
}

// hxstore 始终读不出的字段（见 hxstore.UnsupportedFields），每封信都要标出来。
var (
	outlookSummaryGaps = []string{"read", "has_attachment"}
	outlookMessageGaps = []string{"read", "has_attachment", "attachments"}
)

type outlookSource struct{ s *hxstore.Store }

func openOutlook(opt OpenOptions) (Source, error) {
	dir := opt.Dir
	if dir == "" {
		d, err := hxstore.DefaultDataDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	s, err := hxstore.Open(dir, hxstore.Options{MaxBodyBytes: opt.MaxBodyBytes})
	if err != nil {
		return nil, outlookErr(err)
	}
	return &outlookSource{s: s}, nil
}

func outlookErr(err error) error {
	switch {
	case errors.Is(err, hxstore.ErrUnsupportedFormat):
		return errors.Join(ErrUnsupported, err)
	case errors.Is(err, hxstore.ErrNotFound):
		return errors.Join(ErrNotFound, err)
	}
	return err
}

func (o *outlookSource) Close() error { return o.s.Close() }

func (o *outlookSource) Folders(ctx context.Context) ([]Folder, error) {
	fs, err := o.s.Folders(ctx)
	if err != nil {
		return nil, outlookErr(err)
	}
	out := make([]Folder, 0, len(fs))
	for _, f := range fs {
		if f.Placeholder {
			continue
		}
		// AccountID 未解析（恒为 0），不填，免得模型以为所有文件夹属于同一个账户。
		out = append(out, Folder{
			ID: f.ID, Path: f.Path, Special: f.Special, Total: int64(f.MessageCount),
			NameIncomplete: f.NameIncomplete,
		})
	}
	return out, nil
}

func (o *outlookSource) Search(ctx context.Context, q Query) (Page, error) {
	p, err := o.s.List(ctx, hxstore.Query{
		FolderID: q.FolderID, Since: q.Since, Until: q.Before,
		From: q.From, Subject: q.Subject, Limit: q.Limit, Offset: q.Offset,
	})
	if err != nil {
		return Page{}, outlookErr(err)
	}
	out := Page{Total: p.Total, Messages: make([]Summary, 0, len(p.Items))}
	if p.NextOffset >= 0 {
		out.HasMore, out.NextOffset = true, p.NextOffset
	}
	for _, m := range p.Items {
		out.Messages = append(out.Messages, outlookSummaryOf(m, outlookSummaryGaps))
	}
	return out, nil
}

func outlookSummaryOf(m hxstore.Summary, gaps []string) Summary {
	from := m.From
	if from == "" {
		from = m.FromAddress
	}
	inc := append(append([]string{}, m.Incomplete...), gaps...)
	return Summary{
		ID: m.ID, FolderID: m.FolderID, Subject: m.Subject,
		From: from, FromAddress: m.FromAddress, To: m.To, Date: m.Received,
		Preview: m.Preview, Incomplete: inc,
		// Read / HasAttachment 未解析，保持零值，靠 Incomplete 说明。
	}
}

func (o *outlookSource) Get(ctx context.Context, id int64) (*Message, error) {
	m, err := o.s.Get(ctx, id)
	if err != nil {
		return nil, outlookErr(err)
	}
	out := &Message{
		Summary: outlookSummaryOf(m.Summary, outlookMessageGaps),
		Cc:      m.Cc, MessageID: m.MessageID,
		Body: m.Body, BodyTruncated: m.BodyTruncated,
	}
	switch {
	case m.BodySource == hxstore.BodyPreview:
		out.BodyNote = "本机只有这封信的预览（约 250 字），完整正文没有找到"
	case m.BodySource == hxstore.BodyNone:
		out.BodyNote = "本机没有这封信的正文"
	case !m.BodyComplete && !m.BodyTruncated:
		out.BodyNote = "正文可能不完整（没找到完整的 HTML 正文，或与预览对不上）"
	}
	return out, nil
}
