package localmail

import (
	"context"
	"errors"
	"strings"

	"github.com/nimysan/mcp-hub/internal/localmail/olkwin"
)

var outlookNewWinInfo = SourceInfo{
	ID:   SourceOutlookNewWin,
	Name: "新版 Outlook for Windows",
	Help: "读新版 Outlook（olk）在这台电脑上的离线缓存。只含离线设置里选中的文件夹、离线天数内（默认 30 天）的邮件；" +
		"附件只有文件名和大小。新版 Outlook 开着也能读。经典版 Outlook 的 .ost / .pst 是另一个开关",
	DirLabel: "新版 Outlook 数据目录",
	DirHelp: "留空用当前登录用户的 %LOCALAPPDATA%\\Microsoft\\Olk\\EBWebView\\Default\\IndexedDB；" +
		"以别的用户身份运行 mcp-hub 时，填那个用户的这个目录的完整路径",
	ToolNote: "outlook_new_win 只覆盖新版 Outlook 离线缓存的天数和文件夹（默认最近 30 天、离线设置里选中的文件夹），" +
		"更早的或没缓存的文件夹里的信读不到；它的 folders 里 server_total 是服务器上的总数，total 是本地缓存的封数。",
	GOOS: "windows",
	Open: openOutlookNewWin,
	Fingerprint: func(opt OpenOptions) string {
		sig, err := olkSignature(strings.Trim(strings.TrimSpace(opt.Dir), `"`))
		if err != nil {
			return "-"
		}
		return sig
	},
}

// olkSignature 是 olkwin.Signature，测试里换成假的。
var olkSignature = olkwin.Signature

// olkStore 是 olkwin.Store 用到的方法，测试里换成假的。
type olkStore interface {
	Folders(ctx context.Context) ([]olkwin.Folder, error)
	List(ctx context.Context, q olkwin.Query) (olkwin.Page, error)
	Get(ctx context.Context, id int64) (*olkwin.Message, error)
	Stale() bool
	Close() error
}

var openOlkStore = func(dir string, opt olkwin.Options) (olkStore, error) {
	s, err := olkwin.Open(dir, opt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// outlookNewWinSource 包一个 olkwin 快照。快照打开后不持有任何文件句柄（olk 照常写、照常压实）；
// olk 同步写入后 Stale 为真，Hub 重开一次（这台测试机上约 80 ms）。
type outlookNewWinSource struct{ s olkStore }

func openOutlookNewWin(opt OpenOptions) (Source, error) {
	dir := strings.Trim(strings.TrimSpace(opt.Dir), `"`)
	s, err := openOlkStore(dir, olkwin.Options{MaxBodyBytes: opt.MaxBodyBytes})
	if err != nil {
		return nil, olkErr(err)
	}
	return &outlookNewWinSource{s: s}, nil
}

func olkErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, olkwin.ErrUnsupportedFormat):
		return errors.Join(ErrUnsupported, err)
	case errors.Is(err, olkwin.ErrLocked):
		return errors.Join(ErrLocked, err)
	case errors.Is(err, olkwin.ErrNotFound):
		return errors.Join(ErrNotFound, err)
	}
	return err
}

func (o *outlookNewWinSource) Close() error { return o.s.Close() }

// Stale 见 olkwin.Store.Stale：LevelDB 文件变了（olk 同步了新数据），Hub 会重开。
func (o *outlookNewWinSource) Stale() bool { return o.s.Stale() }

func (o *outlookNewWinSource) Folders(ctx context.Context) ([]Folder, error) {
	fs, err := o.s.Folders(ctx)
	if err != nil {
		return nil, olkErr(err)
	}
	out := make([]Folder, 0, len(fs))
	for _, f := range fs {
		if !f.Mail || f.Special == "msgfolderroot" {
			continue // 便笺等不是邮件文件夹；根文件夹本身没有信
		}
		out = append(out, Folder{
			ID: f.ID, Path: f.Path, Account: f.Account, Special: f.Special,
			Total: int64(f.MessageCount), Unread: int64(f.UnreadCount),
		})
	}
	return out, nil
}

func (o *outlookNewWinSource) Search(ctx context.Context, q Query) (Page, error) {
	p, err := o.s.List(ctx, olkwin.Query{
		FolderID: q.FolderID, Since: q.Since, Until: q.Before, From: q.From, Subject: q.Subject,
		Limit: q.Limit, Offset: q.Offset,
	})
	if err != nil {
		return Page{}, olkErr(err)
	}
	out := Page{Total: p.Total, Messages: make([]Summary, 0, len(p.Items))}
	if p.NextOffset >= 0 {
		out.HasMore, out.NextOffset = true, p.NextOffset
	}
	for _, m := range p.Items {
		out.Messages = append(out.Messages, olkSummary(m))
	}
	return out, nil
}

// olkIncomplete 把 olkwin 的缺失字段名换成本包的字段名；本包没有对应字段的（sent、preview 等）不报。
var olkIncomplete = map[string]string{
	"subject": "subject", "from": "from", "received": "date", "folder": "folder_id",
	"has_attachment": "has_attachment", "read": "read",
	"body": "body", "body_full": "body", "recipients": "recipients", "attachments": "attachments", "message_id": "message_id",
}

func olkSummary(m olkwin.Summary) Summary {
	from := m.From
	if from == "" {
		from = m.FromAddress
	}
	date := m.Received
	if date.IsZero() {
		date = m.Sent
	}
	s := Summary{
		ID: m.ID, FolderID: m.FolderID, Subject: m.Subject, From: from, FromAddress: m.FromAddress,
		To: m.To, Date: date, Read: m.Read, HasAttachment: m.HasAttachment, Preview: m.Preview,
	}
	s.Incomplete = mapIncomplete(m.Incomplete)
	if !date.IsZero() {
		s.Incomplete = dropField(s.Incomplete, "date") // 没有接收时间但有发送时间：用发送时间
	}
	return s
}

func mapIncomplete(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range in {
		if v, ok := olkIncomplete[k]; ok && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func dropField(v []string, f string) []string {
	out := v[:0]
	for _, x := range v {
		if x != f {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (o *outlookNewWinSource) Get(ctx context.Context, id int64) (*Message, error) {
	m, err := o.s.Get(ctx, id)
	if err != nil {
		return nil, olkErr(err)
	}
	out := &Message{
		Summary: olkSummary(m.Summary), Cc: m.Cc, MessageID: m.MessageID,
		Body: m.Body, BodyTruncated: m.BodyTruncated,
	}
	if len(m.Recipients) > 0 {
		var to, cc []string
		for _, r := range m.Recipients {
			v := r.Name
			if r.Address != "" && r.Address != r.Name {
				v = strings.TrimSpace(r.Name + " <" + r.Address + ">")
			}
			switch r.Type {
			case "to":
				to = append(to, v)
			case "cc":
				cc = append(cc, v)
			}
		}
		out.To, out.Cc = to, cc
	}
	switch {
	case containsStr(m.Incomplete, "body_full"):
		out.BodyNote = "新版 Outlook 只缓存了这封信正文的开头一段"
	case m.BodySource == "html":
		out.BodyNote = "正文由 HTML 转成纯文本，格式可能和原信不同"
	case m.BodySource == "preview":
		out.BodyNote = "新版 Outlook 的离线缓存里没有这封信的正文，这里只有摘要"
	}
	for _, at := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{Name: at.Name, Size: at.Size, ContentType: at.ContentType})
	}
	out.HasAttachment = out.HasAttachment || len(out.Attachments) > 0
	return out, nil
}

func containsStr(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
