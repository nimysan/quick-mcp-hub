package hxstore

// 解压后对象层的字段抽取（启发式，基于 16.115 实测槽位）。
//
// 对象头：06 00 70 05 <id u64> <type u16> <size u32>；邮件对象 type=0xC7。
// 字符串槽：u32 off + u32 (byteLen|0x80000000)，byteLen 含 2 字节 00 00 结尾，
// off 相对对象末尾的串堆基址；串为 UTF-16LE。时间是 .NET ticks（UTC）。
// 以 MessageClass 串 "IPM.*" 的槽位 j 为锚点，其他字段的相对偏移见 slot* 常量。

import (
	"bytes"
	"encoding/binary"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	slotMsgID    = -8
	slotTopic    = 24
	slotPreview  = 68
	slotFromName = 152
	slotFromAddr = 160
	slotSubject  = 196
	slotRecv     = -948
	slotSent     = -508
	slotModified = -860
	slotFolder   = -328
	slotBodyObj  = -1196

	typeMail   = 0xC7
	typeTo     = 0xCC // 推测，未经人工核对
	typeCc     = 0xCD // 推测，未经人工核对
	anchorBack = 6000 // 在 class 串前多远内找它的槽位
	hdrBack    = 1600 // 在槽位前多远内找邮件对象头
	maxStrLen  = 1 << 20
)

var (
	clsPat    = []byte{'I', 0, 'P', 0, 'M', 0, '.', 0}
	objHdrPat = []byte{0x06, 0x00, 0x70, 0x05}
)

// rawMail 是从一个块里抽到的一封邮件（一个版本）。
type rawMail struct {
	ID        uint64
	BlockOff  int64
	Class     string
	Subject   string
	Topic     string
	FromName  string
	FromAddr  string
	MsgID     string
	Preview   string
	Received  time.Time
	Sent      time.Time
	Modified  time.Time
	FolderID  uint64
	BodyObjID uint64
	To, Cc    []string
	RecipSeen bool     // 找到了收件人子对象（即使列表为空）
	alt       []uint64 // 同一封信其他对象的 id（找正文的备选）
}

func u16str(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// getStr 读 j 处的字符串槽；任何越界或格式不符都返回 ok=false。
func getStr(b []byte, j, base int) (string, bool) {
	if j < 0 || j+8 > len(b) {
		return "", false
	}
	off := int(binary.LittleEndian.Uint32(b[j:]))
	ln := binary.LittleEndian.Uint32(b[j+4:])
	if ln&0x80000000 == 0 {
		return "", false
	}
	L := int(ln & 0x7fffffff)
	s := base + off
	if L < 2 || L > maxStrLen || L%2 != 0 || off < 0 || s < 0 || s > len(b)-L || b[s+L-2] != 0 || b[s+L-1] != 0 {
		return "", false
	}
	return u16str(b[s : s+L-2]), true
}

var dotnetEpoch = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	ticks1990 = 627667488000000000
	ticks2100 = 662380416000000000
)

func ticksAt(b []byte, j int) time.Time {
	if j < 0 || j+8 > len(b) {
		return time.Time{}
	}
	v := int64(binary.LittleEndian.Uint64(b[j:]))
	if v < ticks1990 || v > ticks2100 {
		return time.Time{}
	}
	sec := v / 10_000_000
	ns := (v % 10_000_000) * 100
	// 分两步避免 time.Duration 溢出
	return dotnetEpoch.AddDate(0, 0, int(sec/86400)).Add(time.Duration(sec%86400)*time.Second + time.Duration(ns))
}

func u64At(b []byte, j int) uint64 {
	if j < 0 || j+8 > len(b) {
		return 0
	}
	return binary.LittleEndian.Uint64(b[j:])
}

// findAnchors 找所有 class 串的槽位 j 与串堆基址 base。
func findAnchors(b []byte) [][2]int {
	var out [][2]int
	seen := map[int]bool{}
	key := make([]byte, 4)
	for p := 0; p < len(b); {
		k := bytes.Index(b[p:], clsPat)
		if k < 0 {
			break
		}
		cp := p + k
		p = cp + 2
		e := cp
		for e+1 < len(b) && !(b[e] == 0 && b[e+1] == 0) {
			e += 2
		}
		if e+1 >= len(b) {
			continue
		}
		L := e - cp + 2
		if L > maxStrLen {
			continue
		}
		binary.LittleEndian.PutUint32(key, uint32(L)|0x80000000)
		lo := cp - anchorBack
		if lo < 0 {
			lo = 0
		}
		win := b[lo:cp]
		for q := len(win); q > 0; {
			k2 := bytes.LastIndex(win[:q], key)
			if k2 < 4 {
				break
			}
			j := lo + k2 - 4
			off := int(binary.LittleEndian.Uint32(b[j:]))
			base := cp - off
			if off >= 0 && base > j && base <= cp {
				if !seen[j] {
					seen[j] = true
					out = append(out, [2]int{j, base})
				}
				break
			}
			q = k2
		}
	}
	return out
}

// mailObjID 在槽位 j 之前找最近的邮件对象头 "06 00 70 05 <id> c7 00"。
func mailObjID(b []byte, j int) uint64 {
	lo := j - hdrBack
	if lo < 0 {
		lo = 0
	}
	for p := j; p >= lo; p-- {
		if p+14 > len(b) {
			continue
		}
		// 与原型一致：只校验 06 ?? 70 05 和 type 两字节
		if b[p] == 0x06 && b[p+2] == 0x70 && b[p+3] == 0x05 && b[p+12] == typeMail && b[p+13] == 0 {
			return binary.LittleEndian.Uint64(b[p+4:])
		}
	}
	return 0
}

// extractMails 抽取一个块里的所有邮件版本。
func extractMails(b []byte, blockOff int64) []rawMail {
	var out []rawMail
	for _, a := range findAnchors(b) {
		j, base := a[0], a[1]
		var m rawMail
		m.BlockOff = blockOff
		m.Class, _ = getStr(b, j, base)
		m.MsgID, _ = getStr(b, j+slotMsgID, base)
		m.Received = ticksAt(b, j+slotRecv)
		m.ID = mailObjID(b, j)
		if m.ID == 0 || !strings.HasPrefix(m.Class, "IPM.") || m.MsgID == "" || m.Received.IsZero() {
			continue // 多为索引副本
		}
		m.Subject, _ = getStr(b, j+slotSubject, base)
		m.Topic, _ = getStr(b, j+slotTopic, base)
		m.FromName, _ = getStr(b, j+slotFromName, base)
		m.FromAddr, _ = getStr(b, j+slotFromAddr, base)
		m.Preview, _ = getStr(b, j+slotPreview, base)
		m.Sent = ticksAt(b, j+slotSent)
		m.Modified = ticksAt(b, j+slotModified)
		m.FolderID = u64At(b, j+slotFolder)
		m.BodyObjID = u64At(b, j+slotBodyObj)
		if r := recipObjects(b, m.ID); r.seen {
			m.To, m.Cc, m.RecipSeen = r.to, r.cc, true
		}
		out = append(out, m)
	}
	return out
}

type recips struct {
	to, cc []string
	seen   bool
}

// recipObjects 在 b 里找属于 id 的收件人子对象，抽其中的 UTF-16 地址。
func recipObjects(b []byte, id uint64) recips {
	var r recips
	pat := make([]byte, 12)
	copy(pat, objHdrPat)
	binary.LittleEndian.PutUint64(pat[4:], id)
	for p := 0; p < len(b); {
		k := bytes.Index(b[p:], pat)
		if k < 0 {
			break
		}
		s := p + k
		p = s + 12
		if s+18 > len(b) {
			break
		}
		t := binary.LittleEndian.Uint16(b[s+12:])
		if t != typeTo && t != typeCc {
			continue
		}
		sz := int(binary.LittleEndian.Uint32(b[s+14:]))
		end := s + 2 + sz
		if sz <= 0 || end > len(b) || end < s {
			end = len(b)
		}
		addrs := u16Emails(b[s:end])
		r.seen = true
		if t == typeTo {
			r.to = mergeAddrs(r.to, addrs)
		} else {
			r.cc = mergeAddrs(r.cc, addrs)
		}
	}
	return r
}

// scanRecipObjects 找 b 里所有收件人子对象，按邮件 id 汇总（用于跨块兜底）。
func scanRecipObjects(b []byte, want func(uint64) bool, fn func(id uint64, t uint16, addrs []string)) {
	for p := 0; p < len(b); {
		k := bytes.Index(b[p:], objHdrPat)
		if k < 0 {
			break
		}
		s := p + k
		p = s + 4
		if s+18 > len(b) {
			break
		}
		t := binary.LittleEndian.Uint16(b[s+12:])
		if t != typeTo && t != typeCc {
			continue
		}
		id := binary.LittleEndian.Uint64(b[s+4:])
		if !want(id) {
			continue
		}
		sz := int(binary.LittleEndian.Uint32(b[s+14:]))
		end := s + 2 + sz
		if sz <= 0 || end > len(b) || end < s {
			end = len(b)
		}
		fn(id, t, u16Emails(b[s:end]))
	}
}

func mergeAddrs(dst, src []string) []string {
	for _, a := range src {
		dup := false
		for _, d := range dst {
			if d == a {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, a)
		}
	}
	return dst
}

func isAddrCh(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._%+-'", c) >= 0
}

func isAddr(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-3 && strings.Contains(s[at:], ".") && !strings.ContainsAny(s, " <>")
}

// u16Emails 抽取区间内的 UTF-16LE 邮箱地址（偶/奇对齐都试），小写去重。
func u16Emails(b []byte) []string {
	var out []string
	cur := make([]uint16, 0, 64)
	for al := 0; al < 2; al++ {
		flush := func() {
			if len(cur) > 5 && len(cur) < 320 {
				s := strings.Trim(string(utf16.Decode(cur)), ".-'")
				if isAddr(s) {
					out = mergeAddrs(out, []string{strings.ToLower(s)})
				}
			}
			cur = cur[:0]
		}
		for i := al; i+1 < len(b); i += 2 {
			c := binary.LittleEndian.Uint16(b[i:])
			if c < 128 && (isAddrCh(byte(c)) || c == '@') {
				cur = append(cur, c)
			} else {
				flush()
			}
		}
		flush()
	}
	return out
}

// ---------- 文件夹名 ----------
//
// 实测两种对象里有「文件夹 id、20 字节后同一 id 再出现一次、固定偏移处是 UTF-16 名称」的条目：
//   - 0x1E4（疑似文件夹树/导航列表）：名称在 id 后 46 字节，是界面显示名（会本地化，如「收件箱」）
//   - 0x190（疑似账户常用文件夹表）：名称在 id 后 211 字节，是英文规范名（Inbox / Sent Items / Drafts）
// 两处都没有的文件夹（实测 14 个里有 7 个）名称留空，由上层生成标签并标 incomplete。

const (
	typeFolderNav  = 0x1E4
	typeFolderAcct = 0x190
	navNameOff     = 46
	acctNameOff    = 211
	pairGap        = 20
	maxNameRunes   = 256
)

type folderName struct {
	display   string // 优先 0x1E4 的显示名
	canonical string // 0x190 的英文规范名
	rank      int    // 2 = 有显示名，1 = 只有规范名
}

var specialNames = map[string]string{
	"Inbox": "inbox", "Sent Items": "sent", "Drafts": "drafts", "Deleted Items": "deleted",
	"Junk Email": "junk", "Archive": "archive", "Outbox": "outbox",
}

func scanFolderNames(b []byte, fn func(id uint64, n folderName)) {
	for p := 0; p < len(b); {
		k := bytes.Index(b[p:], objHdrPat[2:]) // 70 05
		if k < 0 {
			return
		}
		h := p + k - 2
		p += k + 2
		if h < 0 || h+18 > len(b) || b[h] != 0x06 {
			continue
		}
		t := binary.LittleEndian.Uint16(b[h+12:])
		var nameOff int
		switch t {
		case typeFolderNav:
			nameOff = navNameOff
		case typeFolderAcct:
			nameOff = acctNameOff
		default:
			continue
		}
		sz := int(binary.LittleEndian.Uint32(b[h+14:]))
		end := h + sz
		if sz <= 18 || end > len(b) || end < h {
			end = len(b)
		}
		obj := b[h:end]
		for q := 18; q+pairGap+8 <= len(obj); q++ {
			id := binary.LittleEndian.Uint64(obj[q:])
			if id == 0 || id>>48 != 0 || binary.LittleEndian.Uint64(obj[q+pairGap:]) != id {
				continue
			}
			name, ok := u16z(obj, q+nameOff)
			if !ok {
				continue
			}
			var n folderName
			if t == typeFolderNav {
				n = folderName{display: name, rank: 2}
			} else {
				n = folderName{display: name, canonical: name, rank: 1}
			}
			// 不跳过：错位读出的 id 也可能成对相等，真 id 由调用方按已知文件夹 id 过滤。
			fn(id, n)
		}
		p = end - 2
		if p < h+4 {
			p = h + 4
		}
	}
}

// u16z 读 b[at:] 处 00 00 结尾的 UTF-16LE 串，要求可打印、不超过 maxNameRunes。
func u16z(b []byte, at int) (string, bool) {
	if at < 0 || at+2 > len(b) {
		return "", false
	}
	e := at
	for e+1 < len(b) && !(b[e] == 0 && b[e+1] == 0) {
		if e-at >= 2*maxNameRunes {
			return "", false
		}
		e += 2
	}
	if e+1 >= len(b) || e == at {
		return "", false
	}
	s := u16str(b[at:e])
	for _, r := range s {
		if r < 0x20 || r == 0xFFFD || r >= 0xD800 && r <= 0xDFFF {
			return "", false
		}
	}
	return strings.TrimSpace(s), strings.TrimSpace(s) != ""
}
