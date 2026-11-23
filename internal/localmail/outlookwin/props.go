package outlookwin

import (
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// 用到的属性 ID（[MS-OXPROPS]）。
const (
	pidMessageClass          = 0x001A
	pidSubject               = 0x0037
	pidClientSubmitTime      = 0x0039
	pidSentRepresentingName  = 0x0042
	pidSentRepresentingEmail = 0x0065
	pidSentRepresentingType  = 0x0064
	pidTransportHeaders      = 0x007D
	pidSenderName            = 0x0C1A
	pidSenderAddrType        = 0x0C1E
	pidSenderEmail           = 0x0C1F
	pidRecipientType         = 0x0C15
	pidDisplayCc             = 0x0E03
	pidDisplayTo             = 0x0E04
	pidDeliveryTime          = 0x0E06
	pidMessageFlags          = 0x0E07
	pidMessageSize           = 0x0E08
	pidHasAttachments        = 0x0E1B
	pidAttachSize            = 0x0E20
	pidBody                  = 0x1000
	pidRtfCompressed         = 0x1009
	pidHTML                  = 0x1013
	pidInternetMessageID     = 0x1035
	pidDisplayName           = 0x3001
	pidAddrType              = 0x3002
	pidEmailAddress          = 0x3003
	pidIpmSubtreeEntryID     = 0x35E0
	pidIpmOutboxEntryID      = 0x35E2
	pidIpmWastebasketEntryID = 0x35E3
	pidIpmSentMailEntryID    = 0x35E4
	pidFinderEntryID         = 0x35E7
	pidContentCount          = 0x3602
	pidContentUnreadCount    = 0x3603
	pidSubfolders            = 0x360A
	pidContainerClass        = 0x3613
	pidIpmDraftsEntryID      = 0x36D7
	pidAttachFilename        = 0x3704
	pidAttachMethod          = 0x3705
	pidAttachLongFilename    = 0x3707
	pidAttachMimeTag         = 0x370E
	pidSmtpAddress           = 0x39FE
	pidInternetCodepage      = 0x3FDE
	pidMessageCodepage       = 0x3FFD
	pidSenderSmtpAddress     = 0x5D01
	pidSentReprSmtpAddress   = 0x5D02
	pidLtpRowID              = 0x67F2
)

const (
	msgFlagRead      = 0x01
	msgFlagHasAttach = 0x10
)

func (b propBag) has(id uint16) bool { _, ok := b[id]; return ok }

// str 取字符串属性。8 位字符串按 codepage 解码（0 表示未知，按 UTF-8/Windows-1252 猜）。
func (b propBag) str(id uint16, codepage int) string {
	p, ok := b[id]
	if !ok {
		return ""
	}
	switch p.typ {
	case ptUnicode:
		return utf16le(p.raw)
	case ptString8:
		return decode8(p.raw, codepage)
	}
	return ""
}

func (b propBag) int(id uint16) (int64, bool) {
	p, ok := b[id]
	if !ok {
		return 0, false
	}
	switch {
	case p.typ == ptInt16 && len(p.raw) >= 2:
		return int64(int16(le.Uint16(p.raw))), true
	case (p.typ == ptInt32 || p.typ == ptError) && len(p.raw) >= 4:
		return int64(int32(le.Uint32(p.raw))), true
	case p.typ == ptInt64 && len(p.raw) >= 8:
		return int64(le.Uint64(p.raw)), true
	}
	return 0, false
}

func (b propBag) bool(id uint16) (bool, bool) {
	p, ok := b[id]
	if !ok || p.typ != ptBool || len(p.raw) < 1 {
		return false, false
	}
	return p.raw[0] != 0, true
}

// time 取 PtypTime（FILETIME，1601 起的 100 纳秒）。
func (b propBag) time(id uint16) time.Time {
	p, ok := b[id]
	if !ok || p.typ != ptTime || len(p.raw) < 8 {
		return time.Time{}
	}
	return filetime(le.Uint64(p.raw))
}

func (b propBag) bin(id uint16) []byte {
	p, ok := b[id]
	if !ok || p.typ != ptBinary {
		return nil
	}
	return p.raw
}

// entryNID 取 EntryID 属性里的 NID（PST 的 EntryID 是 4 字节标志 + 16 字节存储 UID + 4 字节 NID）。
func (b propBag) entryNID(id uint16) uint32 {
	e := b.bin(id)
	if len(e) < 24 {
		return 0
	}
	return le.Uint32(e[20:])
}

func (b propBag) codepage() int {
	for _, id := range []uint16{pidMessageCodepage, pidInternetCodepage} {
		if v, ok := b.int(id); ok && v > 0 {
			return int(v)
		}
	}
	return 0
}

const filetimeEpochDiff = 116444736000000000 // 1601-01-01 到 1970-01-01 的 100ns 数

func filetime(v uint64) time.Time {
	if v <= filetimeEpochDiff || v >= 0x7FFFFFFFFFFFFFFF { // 0 和「永不」都当作没有日期
		return time.Time{}
	}
	d := v - filetimeEpochDiff
	return time.Unix(int64(d/1e7), int64(d%1e7)*100).UTC()
}

func utf16le(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = le.Uint16(b[2*i:])
	}
	for len(u) > 0 && u[len(u)-1] == 0 {
		u = u[:len(u)-1]
	}
	return string(utf16.Decode(u))
}

// decode8 解 8 位字符串。shortcut: 只认常见的几个代码页，其余按 Windows-1252，
// 遇到别的语言的 ANSI PST 再补表。
func decode8(b []byte, cp int) string {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	var enc encoding.Encoding
	switch cp {
	case 65001:
		return strings.ToValidUTF8(string(b), "\uFFFD")
	case 936, 54936:
		enc = simplifiedchinese.GB18030
	case 950:
		enc = traditionalchinese.Big5
	case 932:
		enc = japanese.ShiftJIS
	case 949:
		enc = korean.EUCKR
	case 1250:
		enc = charmap.Windows1250
	case 1251:
		enc = charmap.Windows1251
	case 0:
		if utf8.Valid(b) {
			return string(b)
		}
		enc = charmap.Windows1252
	default:
		enc = charmap.Windows1252
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	return string(out)
}

// normSubject 去掉 PST 主题前的「0x01 + 前缀长度」标记（[MS-PST] 2.5.3.1.1.1）。
func normSubject(s string) string {
	if len(s) >= 2 && s[0] == 0x01 {
		_, n := utf8.DecodeRuneInString(s[1:])
		return s[1+n:]
	}
	return s
}
