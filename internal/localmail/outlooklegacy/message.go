package outlooklegacy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Blocks / Mail_OwnedBlocks 里的 BlockTag 是大端 FourCC。
const (
	tagMsgSource  = 0x4D537263 // 'MSrc' -> Message Sources/*.olk15MsgSource
	tagAttachment = 0x41747463 // 'Attc' -> Message Attachments/*.olk15MsgAttachment
)

// 正文来源。
const (
	BodyMIME    = "mime"    // olk15MsgSource 里的完整 RFC822
	BodyCache   = "cache"   // olk15Message 里缓存的 HTML（UTF-16LE）转成的纯文本
	BodyPreview = "preview" // 只有 Mail.Message_Preview 的摘要
	BodyNone    = "none"
)

// ErrNotFound 表示 id 不存在。
var ErrNotFound = errors.New("邮件不存在")

// Attachment 只给名字和大小，不读内容。
type Attachment struct {
	Name          string `json:"name"`
	ContentType   string `json:"content_type,omitempty"`
	Size          int64  `json:"size"`
	SizeEstimated bool   `json:"size_estimated,omitempty"` // 由 base64 文件长度估算
}

// Message 是单封邮件详情。
type Message struct {
	Summary
	MessageID string `json:"message_id,omitempty"`
	DisplayTo string `json:"display_to,omitempty"`
	// 以下来自 MIME 头（仅 BodySource=mime 时有）
	OriginalSubject string    `json:"original_subject,omitempty"`
	Date            time.Time `json:"date,omitempty"`

	Body          string       `json:"body"`
	BodySource    string       `json:"body_source"`
	BodyComplete  bool         `json:"body_complete"`  // 正文是否完整（预览/半下载/截断都算不完整）
	BodyTruncated bool         `json:"body_truncated"` // 超过 MaxBodyBytes 被截断
	Attachments   []Attachment `json:"attachments,omitempty"`
}

// Get 按 Record_RecordID 取单封邮件。
func (s *Store) Get(ctx context.Context, id int64) (*Message, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+summaryCols+
		", PathToDataFile, Message_MessageID, Message_DisplayTo FROM Mail WHERE Record_RecordID = ?", id)
	var dataPath, msgID, displayTo sql.NullString
	sm, err := scanSummary(row, &dataPath, &msgID, &displayTo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m := &Message{Summary: sm, MessageID: strings.TrimSpace(msgID.String), DisplayTo: strings.TrimSpace(displayTo.String)}

	var srcPaths, attPaths []string
	rows, err := s.db.QueryContext(ctx, `SELECT mo.BlockTag, b.PathToDataFile FROM Mail_OwnedBlocks mo
 JOIN Blocks b ON b.BlockID = mo.BlockID WHERE mo.Record_RecordID = ? AND mo.BlockTag IN (?, ?)`,
		id, tagMsgSource, tagAttachment)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tag int64
		var p sql.NullString
		if err := rows.Scan(&tag, &p); err != nil {
			rows.Close()
			return nil, err
		}
		if !p.Valid || p.String == "" {
			continue
		}
		if tag == tagMsgSource {
			srcPaths = append(srcPaths, p.String)
		} else {
			attPaths = append(attPaths, p.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 1) 完整 MIME
	for _, rel := range srcPaths {
		p, err := s.resolve(rel)
		if err != nil {
			continue
		}
		mm, err := readSource(p, s.maxBody)
		if err != nil {
			continue // 文件缺失或损坏，退到缓存
		}
		m.Body, m.BodyTruncated = mm.text, mm.truncated
		m.BodySource = BodyMIME
		m.BodyComplete = !mm.truncated
		m.OriginalSubject, m.Date = mm.subject, mm.date
		if m.MessageID == "" {
			m.MessageID = mm.messageID
		}
		m.Attachments = mm.attachments
		break
	}
	// 2) olk15Message 缓存
	if m.BodySource == "" && dataPath.String != "" {
		if p, err := s.resolve(dataPath.String); err == nil {
			if text, closed, err := readCachedBody(p); err == nil && text != "" {
				m.Body, m.BodyTruncated = truncate(text, s.maxBody)
				m.BodySource = BodyCache
				m.BodyComplete = closed && !sm.PartiallyDownloaded && !m.BodyTruncated
			}
		}
	}
	// 3) 预览
	if m.BodySource == "" {
		if sm.Preview != "" {
			m.Body, m.BodySource = sm.Preview, BodyPreview
		} else {
			m.BodySource = BodyNone
		}
	}
	if m.Attachments == nil {
		for _, rel := range attPaths {
			p, err := s.resolve(rel)
			if err != nil {
				continue
			}
			if a, err := readAttachmentMeta(p); err == nil {
				m.Attachments = append(m.Attachments, a)
			}
		}
	}
	return m, nil
}

func truncate(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// fileSize 供估算附件大小。
func fileSize(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("不是普通文件")
	}
	return fi.Size(), nil
}
