package hxstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

// 正文来源。
const (
	BodyHTML    = "html"    // 存储里缓存的 HTML 正文转成的纯文本
	BodyPreview = "preview" // 只有约 250 字的预览
	BodyNone    = "none"
)

// Message 是单封邮件详情。字段形状与 outlooklegacy.Message 一致（没有的字段留零值）。
type Message struct {
	Summary
	MessageID      string `json:"message_id,omitempty"`
	Class          string `json:"class,omitempty"`       // IPM.Note / IPM.Schedule.Meeting.Request ...
	Topic          string `json:"topic,omitempty"`       // 会话主题（去掉 RE:/FW:）
	FolderName     string `json:"folder_name,omitempty"` // 生成的标签时 Incomplete 含 folder_name
	BodyMatchesTip bool   `json:"body_matches_preview"`  // 正文开头与预览交叉校验一致
	Body           string `json:"body"`
	BodySource     string `json:"body_source"`
	BodyComplete   bool   `json:"body_complete"`  // 找到闭合的 HTML 正文且与预览一致才算完整
	BodyTruncated  bool   `json:"body_truncated"` // 超过 MaxBodyBytes 被截断
}

// Get 按对象 id 取单封邮件。正文从原文件只读重新解压；块已被改写时重建索引再试一次。
func (s *Store) Get(ctx context.Context, id int64) (*Message, error) {
	x, err := s.current()
	if err != nil {
		return nil, err
	}
	m := x.lookup(uint64(id))
	if m == nil {
		return nil, ErrNotFound
	}
	msg, stale := s.message(x, m)
	if stale && ctx.Err() == nil {
		if err := s.rebuild(); err == nil {
			s.mu.RLock()
			x = s.idx
			s.mu.RUnlock()
			if m = x.lookup(uint64(id)); m == nil {
				return nil, ErrNotFound
			}
			msg, _ = s.message(x, m)
		}
	}
	return msg, ctx.Err()
}

// lookup 按 id 找邮件；被去重掉的旧 id 映射到保留的版本。
func (x *index) lookup(id uint64) *rawMail {
	if m := x.byID[id]; m != nil {
		return m
	}
	for i := 0; i < 4; i++ { // 别名链很短，限次防环
		next, ok := x.alias[id]
		if !ok {
			return nil
		}
		if m := x.byID[next]; m != nil {
			return m
		}
		id = next
	}
	return nil
}

// message 组装详情。stale=true 表示索引里记录的块已经对不上（Outlook 改写过）。
func (s *Store) message(x *index, m *rawMail) (*Message, bool) {
	msg := &Message{
		Summary: x.summary(m), MessageID: strings.TrimSpace(m.MsgID),
		Class: m.Class, Topic: m.Topic,
	}
	if f := x.folder[m.FolderID]; f != nil {
		msg.FolderName = f.Name
	}
	html, closed, matched, stale := s.findHTML(x, m)
	if html != nil {
		text := htmlToText(strings.ToValidUTF8(string(html), ""))
		if text != "" {
			msg.Body, msg.BodyTruncated = truncate(text, s.maxBody)
			msg.BodySource = BodyHTML
			msg.BodyMatchesTip = matched
			msg.BodyComplete = closed && matched && !msg.BodyTruncated
		}
	}
	if msg.BodySource == "" {
		if msg.Preview != "" {
			msg.Body, msg.BodySource = msg.Preview, BodyPreview
		} else {
			msg.BodySource = BodyNone
		}
	}
	if !msg.BodyComplete {
		msg.Incomplete = append(msg.Incomplete, FieldBody)
	}
	return msg, stale
}

// findHTML 在正文对象块和邮件对象块里找 HTML。有预览时优先选与预览一致的那段。
func (s *Store) findHTML(x *index, m *rawMail) (html []byte, closed, matched, stale bool) {
	f, err := os.Open(s.path) // 只读
	if err != nil {
		return nil, false, false, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, false, false
	}
	key := alnumOnly(m.Preview, 16)
	var first []byte
	var firstClosed bool
	for _, oid := range append([]uint64{m.BodyObjID, m.ID}, m.alt...) {
		if oid == 0 {
			continue
		}
		offs := x.blocks[oid]
		for i := len(offs) - 1; i >= 0; i-- { // 文件里靠后的块通常更新
			raw, err := readBlock(f, fi.Size(), offs[i], oid)
			if err != nil {
				if !errors.Is(err, errBlockCRC) {
					stale = true
				}
				continue
			}
			for _, seg := range htmlSegments(raw) {
				if first == nil {
					first, firstClosed = seg.b, seg.closed
				}
				if len([]rune(key)) >= 16 && strings.Contains(alnumText(seg.b), key) {
					return seg.b, seg.closed, true, false
				}
			}
		}
	}
	if first != nil {
		// 没有可比对的预览（太短）也算一致；预览够长却对不上，就标不一致。
		return first, firstClosed, len([]rune(key)) < 16, false
	}
	return nil, false, false, stale
}

type htmlSeg struct {
	b      []byte
	closed bool
}

const maxHTMLSegs = 16

func htmlSegments(raw []byte) []htmlSeg {
	var out []htmlSeg
	lower := bytes.ToLower(raw)
	for p := 0; p < len(lower) && len(out) < maxHTMLSegs; {
		k := bytes.Index(lower[p:], []byte("<html"))
		if k < 0 {
			break
		}
		st := p + k
		e := bytes.Index(lower[st:], []byte("</html>"))
		seg := htmlSeg{}
		if e < 0 {
			seg.b = cloneBytes(raw[st:])
		} else {
			seg.b, seg.closed = cloneBytes(raw[st:st+e+7]), true
		}
		out = append(out, seg)
		p = st + 5
		if seg.closed {
			p = st + e + 7
		}
	}
	return out
}

func cloneBytes(b []byte) []byte { return append([]byte(nil), b...) }

// alnumText：HTML 去标签、去实体后只留字母数字并转小写（只用于和预览比对）。
func alnumText(b []byte) string {
	s := string(b)
	if k := strings.Index(strings.ToLower(s), "<body"); k > 0 {
		s = s[k:]
	}
	var sb strings.Builder
	in, ent := false, false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case in:
		case r == '&':
			ent = true
		case ent && r == ';':
			ent = false
		case ent:
		default:
			if r > 127 || r >= '0' && r <= '9' || r|0x20 >= 'a' && r|0x20 <= 'z' {
				if r >= 'A' && r <= 'Z' {
					r |= 0x20
				}
				sb.WriteRune(r)
			}
		}
		if sb.Len() > 1<<20 {
			break
		}
	}
	return sb.String()
}

func alnumOnly(s string, n int) string {
	var sb strings.Builder
	c := 0
	for _, r := range s {
		if r > 127 || r >= '0' && r <= '9' || r|0x20 >= 'a' && r|0x20 <= 'z' {
			if r >= 'A' && r <= 'Z' {
				r |= 0x20
			}
			if r == utf8.RuneError {
				continue
			}
			sb.WriteRune(r)
			if c++; c >= n {
				break
			}
		}
	}
	return sb.String()
}

func truncate(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
