package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/localmail"
)

// localReadOnly 标注本机只读工具：和 readOnly() 的区别只在 OpenWorldHint ——
// 这些工具只读本机文件，不和任何外部系统交互。
func localReadOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no, DestructiveHint: &no}
}

const localMailTimeout = 30 * time.Second

type localFoldersIn struct {
	Source string `json:"source,omitempty" jsonschema:"哪个邮件客户端；不填则列出全部已启用的来源"`
}

type localFolderGroup struct {
	Source  string             `json:"source"`
	Error   string             `json:"error,omitempty"`
	Folders []localmail.Folder `json:"folders"`
}

type localFoldersOut struct {
	Sources []localFolderGroup `json:"sources"`
}

type localSearchIn struct {
	Source   string `json:"source,omitempty" jsonschema:"哪个邮件客户端；只启用了一个时可以不填"`
	FolderID int64  `json:"folder_id,omitempty" jsonschema:"只看这个文件夹，取值来自 localmail_list_folders 的 id；不填为全部文件夹"`
	From     string `json:"from,omitempty" jsonschema:"发件人显示名或地址包含这段文字"`
	Subject  string `json:"subject,omitempty" jsonschema:"主题包含这段文字"`
	Since    string `json:"since,omitempty" jsonschema:"起始日期（含），格式 YYYY-MM-DD，按这台电脑的时区"`
	Before   string `json:"before,omitempty" jsonschema:"截止日期（不含），格式 YYYY-MM-DD，按这台电脑的时区"`
	Limit    int    `json:"limit,omitempty" jsonschema:"最多返回几封，默认 20"`
	Offset   int    `json:"offset,omitempty" jsonschema:"翻页：上一次结果里的 next_offset"`
}

type localSearchOut struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
	localmail.Page
}

type localGetIn struct {
	Source string `json:"source,omitempty" jsonschema:"哪个邮件客户端；只启用了一个时可以不填"`
	ID     int64  `json:"id" jsonschema:"邮件 id，来自 localmail_search 的结果"`
}

type localGetOut struct {
	Source string `json:"source"`
	localmail.Message
}

// parseLocalDate 按本机时区解析 YYYY-MM-DD。
//
// 本机邮件比的是绝对时刻，而用户说的「11 月 20 日」是本地日历日。用 parseDate
// （UTC 零点）会让东八区每天 0–8 点收到的信落到前一天：since 漏掉它们，
// before 又把次日凌晨的信带进来。IMAP 的 SINCE/BEFORE 只认日期，不受影响。
func parseLocalDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation(dateLayout, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期 %q 格式不对，应为 YYYY-MM-DD", s)
	}
	return t, nil
}

// RegisterLocalMail 注册本机邮件的三个只读工具，返回注册数。
//
// 一套工具加 source 参数，而不是每个客户端一套：模型面对的工具越少越好选，
// 而三个客户端能做的事完全一样（列文件夹、按条件列信、读一封）。差异留在
// 适配器里，结果字段是统一的。
//
// 来源没授权时工具照常注册，调用时返回授权指引 —— 一个来源打不开不应该
// 让整个服务起不来，也不该连累其他来源。
func RegisterLocalMail(s *mcp.Server, hub *localmail.Hub) int {
	ids := hub.Enabled()
	if len(ids) == 0 {
		return 0
	}
	cfg := hub.Config()
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		info, _ := localmail.SourceByID(id)
		names = append(names, fmt.Sprintf("%s（%s）", id, info.Name))
	}
	suffix := fmt.Sprintf(" 当前启用的来源：%s。", strings.Join(names, "、"))
	for _, id := range ids {
		if info, _ := localmail.SourceByID(id); info.ToolNote != "" {
			suffix += info.ToolNote
		}
	}
	if len(ids) > 1 {
		suffix += "启用了多个来源，localmail_search 和 localmail_get 必须用 source 指定一个。"
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:  "localmail_list_folders",
		Title: "列出本机邮件文件夹",
		Description: "列出这台电脑上邮件客户端里的文件夹（收件箱、已发送、归档等）和每个文件夹的邮件数。" +
			"只读本机文件，不联网。要按文件夹查邮件时，把这里的 id 传给 localmail_search 的 folder_id。" + suffix,
		Annotations: localReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in localFoldersIn) (*mcp.CallToolResult, localFoldersOut, error) {
		ctx, cancel := context.WithTimeout(ctx, localMailTimeout)
		defer cancel()
		want := ids
		if strings.TrimSpace(in.Source) != "" {
			id, err := hub.Pick(in.Source)
			if err != nil {
				return nil, localFoldersOut{}, err
			}
			want = []string{id}
		}
		out := localFoldersOut{Sources: make([]localFolderGroup, 0, len(want))}
		for _, id := range want {
			f, err := hub.Folders(ctx, id)
			g := localFolderGroup{Source: id, Folders: f}
			if err != nil {
				if len(want) == 1 {
					return nil, localFoldersOut{}, err
				}
				// 一个来源没授权，不影响其他来源的结果。
				g.Error, g.Folders = err.Error(), []localmail.Folder{}
			}
			out.Sources = append(out.Sources, g)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "localmail_search",
		Title: "搜索本机邮件",
		Description: "在这台电脑上的邮件客户端数据里找邮件，按接收时间从新到旧。可按发件人、主题、日期范围、文件夹过滤，" +
			"多个条件取交集；都不填就是最近的邮件。返回发件人、主题、时间、是否已读、摘要和 id，不含正文 —— 要正文用 localmail_get。" +
			fmt.Sprintf("每次最多 %d 封，has_more 为 true 时把 next_offset 传给 offset 翻页。", cfg.MaxResults) +
			"只读本机文件，不联网，也不会把邮件标成已读。" +
			"某封信的 incomplete 里列出的字段没有读到，它的值不可信（例如 read=false 不代表未读），回答时要说明。" + suffix,
		Annotations: localReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in localSearchIn) (*mcp.CallToolResult, localSearchOut, error) {
		ctx, cancel := context.WithTimeout(ctx, localMailTimeout)
		defer cancel()
		id, err := hub.Pick(in.Source)
		if err != nil {
			return nil, localSearchOut{}, err
		}
		q := localmail.Query{
			FolderID: in.FolderID, From: strings.TrimSpace(in.From), Subject: strings.TrimSpace(in.Subject),
			Limit: in.Limit, Offset: in.Offset,
		}
		if q.Since, err = parseLocalDate(in.Since); err != nil {
			return nil, localSearchOut{}, err
		}
		if q.Before, err = parseLocalDate(in.Before); err != nil {
			return nil, localSearchOut{}, err
		}
		p, err := hub.Search(ctx, id, q)
		if err != nil {
			return nil, localSearchOut{}, err
		}
		return nil, localSearchOut{Source: id, Count: len(p.Messages), Page: p}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "localmail_get",
		Title: "读取本机邮件",
		Description: "读一封本机邮件的正文、收件人、抄送和附件文件名。需要 localmail_search 结果里的 id。" +
			fmt.Sprintf("正文最长约 %d 字节，超出截断（body_truncated 为 true）；", cfg.MaxBodyBytes) +
			"body_note 非空时说明正文不完整或读不出的原因。不读附件内容，不改动邮件。" + suffix,
		Annotations: localReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in localGetIn) (*mcp.CallToolResult, localGetOut, error) {
		ctx, cancel := context.WithTimeout(ctx, localMailTimeout)
		defer cancel()
		id, err := hub.Pick(in.Source)
		if err != nil {
			return nil, localGetOut{}, err
		}
		if in.ID <= 0 {
			return nil, localGetOut{}, fmt.Errorf("id 必须为正整数，来自 localmail_search 的结果")
		}
		m, err := hub.Get(ctx, id, in.ID)
		if err != nil {
			return nil, localGetOut{}, err
		}
		return nil, localGetOut{Source: id, Message: *m}, nil
	})
	return 3
}
