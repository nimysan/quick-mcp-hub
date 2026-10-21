// Package tools 把 mailbox 的能力注册成 MCP tool。
package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/mailbox"
)

const dateLayout = "2006-01-02"

// readOnly 标注只读 tool。客户端（如 Amazon Quick）靠 readOnlyHint 区分
// 只读查询和会改变外部状态的动作；不标注的 tool 会被一律当作未知动作，
// 每次调用都要人工确认。
var (
	yes = true
	no  = false
)

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes, DestructiveHint: &no}
}

type listRecentIn struct {
	Limit   int    `json:"limit,omitempty" jsonschema:"返回最近几封，默认 10，最大 50"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"邮箱文件夹，默认 INBOX"`
}

type listOut struct {
	Mailbox  string            `json:"mailbox"`
	Count    int               `json:"count"`
	Messages []mailbox.Summary `json:"messages"`
}

type searchIn struct {
	From    string `json:"from,omitempty" jsonschema:"按发件人过滤，可填地址片段"`
	Keyword string `json:"keyword,omitempty" jsonschema:"全文关键词，匹配主题与正文"`
	Since   string `json:"since,omitempty" jsonschema:"起始日期，格式 YYYY-MM-DD"`
	Before  string `json:"before,omitempty" jsonschema:"截止日期，格式 YYYY-MM-DD"`
	Unread  bool   `json:"unread,omitempty" jsonschema:"只看未读"`
	Limit   int    `json:"limit,omitempty" jsonschema:"最多返回几封，默认 20，最大 50"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"邮箱文件夹，默认 INBOX"`
}

type readIn struct {
	UID     int    `json:"uid" jsonschema:"邮件 UID，来自 list_recent 或 search_mail 的结果"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"邮箱文件夹，默认 INBOX"`
}

type sendIn struct {
	To      []string `json:"to" jsonschema:"收件人地址列表"`
	Cc      []string `json:"cc,omitempty" jsonschema:"抄送地址列表"`
	Subject string   `json:"subject" jsonschema:"邮件主题"`
	Body    string   `json:"body" jsonschema:"邮件正文"`
	HTML    bool     `json:"html,omitempty" jsonschema:"正文按 HTML 发送，默认纯文本"`
}

type sendOut struct {
	Sent    bool     `json:"sent"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
}

// Register 把全部 tool 挂到 server 上。
// send_mail 只在 cfg.AllowSend 为真时注册 —— 发信不可逆，默认不暴露给模型。
func Register(s *mcp.Server, cfg config.Config) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_recent",
		Title:       "列出最近邮件",
		Description: "列出邮箱里最近的若干封邮件，只返回 UID、发件人、主题、时间、大小等信头摘要，不含正文。要读正文用 read_mail。",
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listRecentIn) (*mcp.CallToolResult, listOut, error) {
		box := orDefault(in.Mailbox, "INBOX")
		msgs, err := mailbox.ListRecent(cfg, box, clampLimit(in.Limit, 10))
		if err != nil {
			return nil, listOut{}, err
		}
		return nil, listOut{Mailbox: box, Count: len(msgs), Messages: msgs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_mail",
		Title:       "检索邮件",
		Description: "按发件人、关键词、日期范围、未读状态检索邮件，返回信头摘要。多个条件之间是「且」关系。",
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, listOut, error) {
		q := mailbox.SearchQuery{From: in.From, Keyword: in.Keyword, Unread: in.Unread}
		var err error
		if q.Since, err = parseDate(in.Since); err != nil {
			return nil, listOut{}, err
		}
		if q.Before, err = parseDate(in.Before); err != nil {
			return nil, listOut{}, err
		}
		box := orDefault(in.Mailbox, "INBOX")
		msgs, err := mailbox.Search(cfg, box, q, clampLimit(in.Limit, 20))
		if err != nil {
			return nil, listOut{}, err
		}
		return nil, listOut{Mailbox: box, Count: len(msgs), Messages: msgs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_mail",
		Title:       "读取邮件正文",
		Description: "按 UID 读取一封邮件的纯文本正文（自动做 GB2312/GBK 等字符集解码），并列出附件文件名。用 BODY.PEEK 读取，不会把邮件标记为已读。",
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, *mailbox.Message, error) {
		if in.UID <= 0 {
			return nil, nil, fmt.Errorf("uid 必须为正整数")
		}
		msg, err := mailbox.Read(cfg, orDefault(in.Mailbox, "INBOX"), uint32(in.UID))
		if err != nil {
			return nil, nil, err
		}
		return nil, msg, nil
	})

	if !cfg.AllowSend {
		mailbox.Logf("send_mail 未注册：发信默认关闭，需设置 MCPHUB_MAIL_ALLOW_SEND=true 才启用")
		return
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_mail",
		Title:       "发送邮件",
		Description: "用当前账号发送一封邮件。这是不可逆动作：邮件一旦发出无法撤回。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, OpenWorldHint: &yes},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sendOut, error) {
		err := mailbox.Send(ctx, cfg, mailbox.SendRequest{
			To: in.To, Cc: in.Cc, Subject: in.Subject, Body: in.Body, HTML: in.HTML,
		})
		if err != nil {
			return nil, sendOut{}, err
		}
		return nil, sendOut{Sent: true, To: in.To, Subject: in.Subject}, nil
	})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func clampLimit(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > 50 {
		return 50
	}
	return v
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期 %q 格式不对，应为 YYYY-MM-DD", s)
	}
	return t, nil
}
