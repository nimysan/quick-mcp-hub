package hxstore

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestRealStore 只在本机手动跑，对真实 HxStore.hxd 做只读统计，只输出数字，不输出任何邮件内容。
//
//	HXSTORE_REAL=1 [HXSTORE_DIR=<配置文件目录或 .hxd 路径>] [HXSTORE_SAMPLE=N] [HXSTORE_SNAPDIR=<克隆目录>] \
//	  go test -run TestRealStore -v ./internal/localmail/hxstore/
func TestRealStore(t *testing.T) {
	if os.Getenv("HXSTORE_REAL") != "1" {
		t.Skip("设置 HXSTORE_REAL=1 才跑真实文件统计")
	}
	dir := os.Getenv("HXSTORE_DIR")
	if dir == "" {
		var err error
		if dir, err = DefaultDataDir(); err != nil {
			t.Fatal(err)
		}
	}
	profile := dir
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		profile = filepath.Dir(dir)
	}
	before := listDir(t, profile)

	t0 := time.Now()
	s, err := Open(dir, Options{SnapshotDir: os.Getenv("HXSTORE_SNAPDIR")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st := s.Stats()
	t.Logf("Open 耗时 %v；版本 %q；快照(clonefile) %v", time.Since(t0).Round(time.Millisecond), st.Version, st.Snapshot)
	t.Logf("块：候选 %d，有效 %d，CRC 失败 %d，头非法/截断 %d，解压失败 %d，解压后 %.0f MB",
		st.Candidates, st.BlocksOK, st.BadCRC, st.BadHeader, st.BadDecode, float64(st.RawBytes)/1e6)
	t.Logf("邮件版本 %d → 同对象去重 %d，同 Message-ID 去重 %d → %d 封；收件人跨块兜底 %d",
		st.Versions, st.DupByObject, st.DupByMsgID, st.Messages, st.RecipFromScan)
	t.Logf("自检：主题 %.1f%%，发件人地址 %.1f%%，发送≤接收 %.1f%%", 100*st.SubjectRate, 100*st.FromAddrRate, 100*st.SentRecvRate)
	logMem(t, "Open 后")

	ctx := context.Background()
	folders, err := s.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, f := range folders {
		if !f.NameIncomplete {
			named++
		}
	}
	inNamed := 0
	for _, f := range folders {
		if !f.NameIncomplete {
			inNamed += f.MessageCount
		}
	}
	t.Logf("文件夹 %d 个，有真名 %d 个（覆盖 %d / %d 封）", len(folders), named, inNamed, st.Messages)

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
	t.Logf("List 全量 %d 封，耗时 %v", len(all), time.Since(tList).Round(time.Millisecond))
	n := len(all)
	if n == 0 {
		t.Fatal("没有邮件")
	}
	var subj, from, addr, recip, to, cc, sent, folder, prev int
	var times []time.Time
	for _, m := range all {
		cnt := func(c bool, p *int) {
			if c {
				*p++
			}
		}
		cnt(m.Subject != "", &subj)
		cnt(m.From != "", &from)
		cnt(m.FromAddress != "", &addr)
		cnt(len(m.To)+len(m.Cc) > 0, &recip)
		cnt(len(m.To) > 0, &to)
		cnt(len(m.Cc) > 0, &cc)
		cnt(!m.Sent.IsZero(), &sent)
		cnt(m.FolderID != 0, &folder)
		cnt(m.Preview != "", &prev)
		times = append(times, m.Received)
	}
	pct := func(a int) string {
		return strconv.Itoa(a) + " (" + strconv.FormatFloat(100*float64(a)/float64(n), 'f', 1, 64) + "%)"
	}
	t.Logf("主题 %s；发件人名 %s；发件人地址 %s；预览 %s", pct(subj), pct(from), pct(addr), pct(prev))
	t.Logf("收件人(任一) %s；To %s；Cc %s；发送时间 %s；文件夹 id %s", pct(recip), pct(to), pct(cc), pct(sent), pct(folder))
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	t.Logf("接收时间范围(UTC) %s → %s", times[0].Format(time.RFC3339), times[n-1].Format(time.RFC3339))

	// 按 id 抽样取正文，只统计来源与完整性
	sample := 300
	if v, err := strconv.Atoi(os.Getenv("HXSTORE_SAMPLE")); err == nil && v > 0 {
		sample = v
	}
	if sample > n {
		sample = n
	}
	rng := rand.New(rand.NewSource(1))
	src := map[string]int{}
	var complete, matched, trunc int
	var gets time.Duration
	for _, i := range rng.Perm(n)[:sample] {
		tg := time.Now()
		m, err := s.Get(ctx, all[i].ID)
		gets += time.Since(tg)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		src[m.BodySource]++
		if m.BodyComplete {
			complete++
		}
		if m.BodyMatchesTip {
			matched++
		}
		if m.BodyTruncated {
			trunc++
		}
	}
	t.Logf("Get 抽样 %d 封：来源 %v，完整 %d，与预览一致 %d，截断 %d，平均 %v/封",
		sample, src, complete, matched, trunc, (gets / time.Duration(sample)).Round(time.Microsecond))
	logMem(t, "结束")

	after := listDir(t, profile)
	if len(before) != len(after) {
		t.Errorf("配置文件目录条目数变化 %d → %d（应由 Outlook 自身引起，本包不建文件）", len(before), len(after))
	}
	for name := range after {
		if !before[name] {
			t.Logf("新出现的条目: %s", name)
		}
	}
}

func listDir(t *testing.T, dir string) map[string]bool {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range es {
		out[e.Name()] = true
	}
	return out
}

func logMem(t *testing.T, at string) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("内存（%s）：HeapInuse %.0f MB，Sys %.0f MB，累计分配 %.0f MB", at,
		float64(ms.HeapInuse)/1e6, float64(ms.Sys)/1e6, float64(ms.TotalAlloc)/1e6)
}
