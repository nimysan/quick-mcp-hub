package localmail

import (
	"context"
	"errors"
	"fmt"

	"github.com/nimysan/mcp-hub/internal/localmail/outlooklegacy"
)

var outlookLegacyInfo = SourceInfo{
	ID:   SourceOutlookLegacy,
	Name: "旧版 Outlook for Mac",
	Help: "读旧版 Outlook（Outlook 15 Profiles）在本机缓存的邮件，只含已经同步到这台 Mac 的部分。" +
		"新版 Outlook 用的是另一套存储，这个开关读不到",
	DirLabel: "旧版 Outlook 数据目录",
	DirHelp:  "留空用 Main Profile 的 Data 目录；用了别的配置文件时填 …/Outlook 15 Profiles/<配置文件名>/Data",
	Open:     openOutlookLegacy,
}

type outlookLegacySource struct{ s *outlooklegacy.Store }

func openOutlookLegacy(opt OpenOptions) (Source, error) {
	dir := opt.Dir
	if dir == "" {
		d, err := outlooklegacy.DefaultDataDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	s, err := outlooklegacy.Open(dir, outlooklegacy.Options{MaxBodyBytes: opt.MaxBodyBytes})
	if err != nil {
		return nil, err
	}
	return &outlookLegacySource{s: s}, nil
}

func (o *outlookLegacySource) Close() error { return o.s.Close() }

func (o *outlookLegacySource) Folders(ctx context.Context) ([]Folder, error) {
	fs, err := o.s.Folders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Folder, 0, len(fs))
	for _, f := range fs {
		if f.Placeholder {
			continue // Outlook 内部占位，对用户没有意义
		}
		out = append(out, Folder{
			ID: f.ID, Path: f.Path, Account: fmt.Sprint(f.AccountID),
			Special: f.Special, Total: int64(f.MessageCount),
		})
	}
	return out, nil
}

func (o *outlookLegacySource) Search(ctx context.Context, q Query) (Page, error) {
	p, err := o.s.List(ctx, outlooklegacy.Query{
		FolderID: q.FolderID, Since: q.Since, Until: q.Before,
		From: q.From, Subject: q.Subject, Limit: q.Limit, Offset: q.Offset,
	})
	if err != nil {
		return Page{}, err
	}
	out := Page{Total: p.Total, Messages: make([]Summary, 0, len(p.Items))}
	if p.NextOffset >= 0 {
		out.HasMore, out.NextOffset = true, p.NextOffset
	}
	for _, m := range p.Items {
		out.Messages = append(out.Messages, outlookSummary(m))
	}
	return out, nil
}

func outlookSummary(m outlooklegacy.Summary) Summary {
	from := m.From
	if from == "" {
		from = m.FromAddress
	}
	return Summary{
		ID: m.ID, FolderID: m.FolderID, Subject: m.Subject,
		From: from, FromAddress: m.FromAddress, To: m.To, Date: m.Received,
		Read: m.Read, HasAttachment: m.HasAttachment, Preview: m.Preview,
	}
}

func (o *outlookLegacySource) Get(ctx context.Context, id int64) (*Message, error) {
	m, err := o.s.Get(ctx, id)
	if errors.Is(err, outlooklegacy.ErrNotFound) {
		return nil, errors.Join(ErrNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	out := &Message{
		Summary: outlookSummary(m.Summary), Cc: m.Cc, MessageID: m.MessageID,
		Body: m.Body, BodyTruncated: m.BodyTruncated,
	}
	switch m.BodySource {
	case outlooklegacy.BodyCache:
		out.BodyNote = "正文来自 Outlook 的显示缓存（由 HTML 转成纯文本），格式可能和原信不同"
	case outlooklegacy.BodyPreview:
		out.BodyNote = "本机只有这封信的摘要，完整正文没有下载到这台 Mac"
	case outlooklegacy.BodyNone:
		out.BodyNote = "本机没有这封信的正文"
	}
	if !m.BodyComplete && out.BodyNote == "" && !m.BodyTruncated {
		out.BodyNote = "正文可能不完整（这封信只下载了一部分）"
	}
	for _, at := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{Name: at.Name, Size: at.Size, ContentType: at.ContentType})
	}
	return out, nil
}
