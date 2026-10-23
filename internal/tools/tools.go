// Package tools 把 mailbox 的能力注册成 MCP tool。
package tools

import (
	"context"
	"fmt"
	"strings"
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
	Account string `json:"channel_account,omitempty" jsonschema:"用哪个邮箱，填完整邮箱地址；只配了一个邮箱时可以不填"`
	Limit   int    `json:"limit,omitempty" jsonschema:"返回最近几封，默认 10，最大 50"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"邮箱文件夹，默认 INBOX"`
}

type listOut struct {
	Mailbox  string            `json:"mailbox"`
	Count    int               `json:"count"`
	Messages []mailbox.Summary `json:"messages"`
}

type searchIn struct {
	Account string `json:"channel_account,omitempty" jsonschema:"用哪个邮箱，填完整邮箱地址；只配了一个邮箱时可以不填"`
	From    string `json:"from,omitempty" jsonschema:"按发件人过滤，可填地址片段"`
	Keyword string `json:"keyword,omitempty" jsonschema:"全文关键词，匹配主题与正文"`
	Since   string `json:"since,omitempty" jsonschema:"起始日期，格式 YYYY-MM-DD"`
	Before  string `json:"before,omitempty" jsonschema:"截止日期，格式 YYYY-MM-DD"`
	Unread  bool   `json:"unread,omitempty" jsonschema:"只看未读"`
	Limit   int    `json:"limit,omitempty" jsonschema:"最多返回几封，默认 20，最大 50"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"邮箱文件夹，默认 INBOX"`
}

type readIn struct {
	Account string `json:"channel_account,omitempty" jsonschema:"用哪个邮箱，填完整邮箱地址；只配了一个邮箱时可以不填"`
	UID     int    `json:"uid" jsonschema:"邮件 UID，来自 list_recent 或 search_mail 的结果"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"邮箱文件夹，默认 INBOX"`
}

type sendIn struct {
	Account string   `json:"channel_account,omitempty" jsonschema:"用哪个邮箱发，填完整邮箱地址；只配了一个邮箱时可以不填"`
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

// RegisterMail 把邮件工具挂到 server 上，返回注册的工具数。
//
// 多账号的处理方式和声明式渠道一致：仍然只注册一套工具，由 channel_account
// 参数选账号。只配了一个邮箱时这个参数可以不填；配了多个就必须明确给出 ——
// 替模型猜一个默认邮箱，在发信上就是发错人。
//
// send_mail 只在至少一个账号打开发信开关时注册（发信不可逆，默认不暴露给
// 模型），调用时再按选中的账号校一次。
func RegisterMail(s *mcp.Server, cfgs []config.Config) int {
	if len(cfgs) == 0 {
		return 0
	}
	byAccount := make(map[string]config.Config, len(cfgs))
	accounts := make([]string, 0, len(cfgs))
	var sendable []string
	for _, c := range cfgs {
		byAccount[c.User] = c
		accounts = append(accounts, c.User)
		if c.AllowSend {
			sendable = append(sendable, c.User)
		}
	}
	pick := func(account string) (config.Config, error) {
		return pickMail(byAccount, accounts, account)
	}
	// 描述里点明账号情况。写成一句能据以行动的话，而不是括号里的备注：
	// 模型要能直接读出「要不要传账号、可选哪几个」。
	suffix := func() string {
		if len(accounts) == 1 {
			return fmt.Sprintf(" 当前邮箱是 %s。", accounts[0])
		}
		return fmt.Sprintf(" 配了 %d 个邮箱，必须用 channel_account 指定一个：%s。",
			len(accounts), strings.Join(accounts, "、"))
	}()

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_recent",
		Title:       "列出最近邮件",
		Description: "查看邮箱里最近收到的邮件，按时间从新到旧。每封返回发件人、主题、时间、是否未读和一个 UID，不含正文；要看某封写了什么，拿这个 UID 调 read_mail。用它回答「我最近有什么邮件」这类问题；已经知道要找谁或找什么内容时用 search_mail 更快。" + suffix,
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listRecentIn) (*mcp.CallToolResult, listOut, error) {
		cfg, err := pick(in.Account)
		if err != nil {
			return nil, listOut{}, err
		}
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
		Description: "在邮箱里找特定的邮件。可以按发件人、关键词（同时匹配主题和正文）、日期范围、是否未读来筛，给多个条件时取交集。返回的是邮件列表（含 UID），不是正文——要正文再用 read_mail。" + suffix,
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, listOut, error) {
		cfg, err := pick(in.Account)
		if err != nil {
			return nil, listOut{}, err
		}
		q := mailbox.SearchQuery{From: in.From, Keyword: in.Keyword, Unread: in.Unread}
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
		Description: "读取一封邮件的正文内容和附件文件名。需要 UID，从 list_recent 或 search_mail 的结果里取。只读取不下载附件，也不会把邮件变成已读。正文过长会被截断。" + suffix,
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, *mailbox.Message, error) {
		cfg, err := pick(in.Account)
		if err != nil {
			return nil, nil, err
		}
		if in.UID <= 0 {
			return nil, nil, fmt.Errorf("uid 必须为正整数")
		}
		msg, err := mailbox.Read(cfg, orDefault(in.Mailbox, "INBOX"), uint32(in.UID))
		if err != nil {
			return nil, nil, err
		}
		return nil, msg, nil
	})

	if len(sendable) == 0 {
		mailbox.Logf("send_mail 未注册：所有邮箱都没打开发信开关")
		return 3
	}

	desc := "发一封邮件出去。这是不可逆动作：发出后无法撤回、无法修改，所以发送前应当先把收件人、主题和正文给用户确认。" + suffix
	if len(sendable) < len(accounts) {
		desc += fmt.Sprintf(" 其中只有 %s 允许发信，其他邮箱会被拒绝。", strings.Join(sendable, "、"))
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_mail",
		Title:       "发送邮件",
		Description: desc,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, OpenWorldHint: &yes},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sendOut, error) {
		cfg, err := pick(in.Account)
		if err != nil {
			return nil, sendOut{}, err
		}
		if !cfg.AllowSend {
			return nil, sendOut{}, fmt.Errorf("邮箱 %s 的发信开关未打开，可用的是：%s",
				cfg.User, strings.Join(sendable, " / "))
		}
		err = mailbox.Send(ctx, cfg, mailbox.SendRequest{
			To: in.To, Cc: in.Cc, Subject: in.Subject, Body: in.Body, HTML: in.HTML,
		})
		if err != nil {
			return nil, sendOut{}, err
		}
		return nil, sendOut{Sent: true, To: in.To, Subject: in.Subject}, nil
	})
	return 4
}

// pickMail 按 channel_account 选邮箱。只有一个时不需要这个参数。
func pickMail(byAccount map[string]config.Config, accounts []string, account string) (config.Config, error) {
	if len(accounts) == 1 {
		return byAccount[accounts[0]], nil
	}
	acc := strings.TrimSpace(account)
	if acc == "" {
		return config.Config{}, fmt.Errorf("配了多个邮箱，必须用 channel_account 指明用哪个：%s",
			strings.Join(accounts, " / "))
	}
	cfg, ok := byAccount[acc]
	if !ok {
		return config.Config{}, fmt.Errorf("没有邮箱 %q，可用的是：%s", acc, strings.Join(accounts, " / "))
	}
	return cfg, nil
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
