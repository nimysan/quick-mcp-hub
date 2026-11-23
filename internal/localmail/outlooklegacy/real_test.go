package outlooklegacy

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestRealLibrary 只在本机手动跑，对真实库做只读统计，只输出数字，不输出任何邮件内容。
//
//	OUTLOOK_LEGACY_REAL=1 [OUTLOOK_LEGACY_DATA=<Data 目录>] [OUTLOOK_LEGACY_SAMPLE=N] \
//	  go test -run TestRealLibrary -v ./internal/localmail/outlooklegacy/
func TestRealLibrary(t *testing.T) {
	if os.Getenv("OUTLOOK_LEGACY_REAL") != "1" {
		t.Skip("设置 OUTLOOK_LEGACY_REAL=1 才跑真实库统计")
	}
	dir := os.Getenv("OUTLOOK_LEGACY_DATA")
	if dir == "" {
		var err error
		if dir, err = DefaultDataDir(); err != nil {
			t.Fatal(err)
		}
	}
	watch := []string{"Outlook.sqlite", "Outlook.sqlite-wal", "Outlook.sqlite-shm"}
	before := statAll(dir, watch)

	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	t.Logf("immutable=%v", s.Immutable())

	folders, err := s.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	real, nonEmpty := 0, 0
	for _, f := range folders {
		if !f.Placeholder {
			real++
		}
		if f.MessageCount > 0 {
			nonEmpty++
		}
	}
	t.Logf("邮件文件夹: %d（去掉占位 %d，非空 %d）", len(folders), real, nonEmpty)

	// 全量走一遍 List，统计字段抽取率
	var all []Summary
	tList := time.Now()
	for off := 0; off >= 0; {
		p, err := s.List(ctx, Query{Limit: 200, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p.Items...)
		off = p.NextOffset
	}
	t.Logf("邮件总数: %d（List 全量分页耗时 %v）", len(all), time.Since(tList).Round(time.Millisecond))
	n := float64(len(all))
	if n == 0 {
		t.Fatal("库为空")
	}
	var subj, from, fromAddr, to, recv, sent, prev, hasAtt, partial int
	var minR, maxR time.Time
	for _, m := range all {
		cnt(&subj, m.Subject != "")
		cnt(&from, m.From != "")
		cnt(&fromAddr, m.FromAddress != "")
		cnt(&to, len(m.To) > 0)
		cnt(&recv, !m.Received.IsZero())
		cnt(&sent, !m.Sent.IsZero())
		cnt(&prev, m.Preview != "")
		cnt(&hasAtt, m.HasAttachment)
		cnt(&partial, m.PartiallyDownloaded)
		if !m.Received.IsZero() {
			if minR.IsZero() || m.Received.Before(minR) {
				minR = m.Received
			}
			if m.Received.After(maxR) {
				maxR = m.Received
			}
		}
	}
	pct := func(k int) string { return strconv.FormatFloat(float64(k)*100/n, 'f', 1, 64) + "%" }
	t.Logf("字段抽取率: subject=%s from=%s from_address=%s to=%s received=%s sent=%s preview=%s",
		pct(subj), pct(from), pct(fromAddr), pct(to), pct(recv), pct(sent), pct(prev))
	t.Logf("has_attachment=%s partially_downloaded=%s", pct(hasAtt), pct(partial))
	t.Logf("接收日期范围: %s ~ %s", minR.Format("2006-01-02"), maxR.Format("2006-01-02"))

	// Get：默认全量，可用 OUTLOOK_LEGACY_SAMPLE 抽样
	ids := make([]int64, len(all))
	for i, m := range all {
		ids[i] = m.ID
	}
	if k, _ := strconv.Atoi(os.Getenv("OUTLOOK_LEGACY_SAMPLE")); k > 0 && k < len(ids) {
		rand.New(rand.NewSource(1)).Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		ids = ids[:k]
	}
	src := map[string]int{}
	var complete, truncated, getErr, attListed, attWanted, attNamed, attTotal, mimeHdr, empty int
	lat := make([]time.Duration, 0, len(ids))
	for _, id := range ids {
		st := time.Now()
		m, err := s.Get(ctx, id)
		lat = append(lat, time.Since(st))
		if err != nil {
			getErr++
			continue
		}
		src[m.BodySource]++
		cnt(&complete, m.BodyComplete)
		cnt(&truncated, m.BodyTruncated)
		cnt(&empty, m.Body == "")
		if m.BodySource == BodyMIME {
			cnt(&mimeHdr, m.OriginalSubject != "" || !m.Date.IsZero())
		}
		if m.HasAttachment {
			attWanted++
			cnt(&attListed, len(m.Attachments) > 0)
		}
		for _, a := range m.Attachments {
			attTotal++
			cnt(&attNamed, a.Name != "(未命名附件)")
		}
	}
	g := float64(len(ids))
	gp := func(k int) string { return strconv.FormatFloat(float64(k)*100/g, 'f', 1, 64) + "%" }
	t.Logf("Get 样本: %d，错误 %d", len(ids), getErr)
	t.Logf("正文来源: mime=%s cache=%s preview=%s none=%s", gp(src[BodyMIME]), gp(src[BodyCache]), gp(src[BodyPreview]), gp(src[BodyNone]))
	t.Logf("完整正文比例: %s；被截断 %s；正文为空 %s", gp(complete), gp(truncated), gp(empty))
	if src[BodyMIME] > 0 {
		t.Logf("MIME 头解析成功率: %.1f%%", float64(mimeHdr)*100/float64(src[BodyMIME]))
	}
	if attWanted > 0 {
		t.Logf("有附件标记的邮件中列出附件的比例: %.1f%%（%d/%d）；附件 %d 个，取到文件名 %.1f%%",
			float64(attListed)*100/float64(attWanted), attListed, attWanted, attTotal, float64(attNamed)*100/float64(max(attTotal, 1)))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	t.Logf("单封读取耗时: 平均 %v，p50 %v，p95 %v，最大 %v",
		(sum / time.Duration(len(lat))).Round(time.Microsecond), lat[len(lat)/2].Round(time.Microsecond),
		lat[len(lat)*95/100].Round(time.Microsecond), lat[len(lat)-1].Round(time.Millisecond))

	s.Close()
	after := statAll(dir, watch)
	for _, f := range watch {
		if before[f] != after[f] {
			t.Errorf("%s 的大小/修改时间变了：只读约束被破坏", f)
		}
	}
	t.Log("只读校验: Outlook.sqlite / -wal / -shm 大小与修改时间均未变化")
}

func cnt(k *int, ok bool) {
	if ok {
		*k++
	}
}

func statAll(dir string, names []string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil {
			out[n] = strconv.FormatInt(fi.Size(), 10) + "@" + fi.ModTime().String()
		} else {
			out[n] = "missing"
		}
	}
	return out
}
