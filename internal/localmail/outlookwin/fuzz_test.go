package outlookwin

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// FuzzPST：损坏或恶意的 .pst / .ost 只能让读取返回错误，不能 panic、死循环或爆内存。
// 在内存里解析（不经文件），走 Folders → 每个文件夹 List → 前几封 Get。
// 跑法：go test -run '^$' -fuzz FuzzPST -fuzztime 2m ./internal/localmail/outlookwin/
func FuzzPST(f *testing.F) {
	for _, lay := range []layout{layoutANSI, layoutUnicode, layoutUnicode4K} {
		for _, c := range []byte{cryptNone, cryptPermute} {
			b, err := os.ReadFile(buildFixture(f, lay, c))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := parseHeader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		s := &Store{path: "fuzz.pst", p: p, maxBody: 1 << 10, defaultCP: 1252, index: map[int64][]Summary{}}
		ctx := context.Background()
		fs, err := s.Folders(ctx)
		if err != nil {
			return
		}
		for _, fd := range fs {
			pg, err := s.List(ctx, Query{FolderID: fd.ID, Limit: 20})
			if err != nil {
				continue
			}
			for i, m := range pg.Items {
				if i >= 3 {
					break
				}
				s.Get(ctx, m.ID)
			}
		}
	})
}

// TestTreeLimits：B 树页头里的条目大小太小（读条目时越界）、子页指回自己（遍历指数爆炸）都只能报格式错误。
func TestTreeLimits(t *testing.T) {
	for _, lay := range []layout{layoutANSI, layoutUnicode, layoutUnicode4K} {
		orig, err := os.ReadFile(buildFixture(t, lay, cryptNone))
		if err != nil {
			t.Fatal(err)
		}
		p0, err := parseHeader(bytes.NewReader(orig), int64(len(orig)))
		if err != nil {
			t.Fatal(err)
		}
		cbEntOff := map[layout]int{layoutANSI: 498, layoutUnicode: 490, layoutUnicode4K: 4060}[lay]
		levelOff := map[layout]int{layoutANSI: 499, layoutUnicode: 491, layoutUnicode4K: 4061}[lay]
		for _, root := range []uint64{p0.nbtRoot, p0.bbtRoot} {
			for cb := 1; cb < 4*p0.idSize(); cb++ {
				b := append([]byte(nil), orig...)
				b[int(root)+cbEntOff] = byte(cb)
				p, err := parseHeader(bytes.NewReader(b), int64(len(b)))
				if err != nil {
					t.Fatal(err)
				}
				s := &Store{path: "x.pst", p: p, maxBody: 1 << 10, defaultCP: 1252, index: map[int64][]Summary{}}
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s 根页 %#x cbEnt=%d：panic %v", lay, root, cb, r)
						}
					}()
					s.Folders(context.Background())
					p.walkNodes(func(nbtEntry) error { return nil })
				}()
			}
		}
		// 根页改成中间页、唯一的子页指回自己：遍历必须很快报错，不能按层数指数展开
		b := append([]byte(nil), orig...)
		root := int(p0.nbtRoot)
		n := p0.idSize()
		b[root+levelOff] = 1
		cnt := map[layout]int{layoutANSI: 496, layoutUnicode: 488, layoutUnicode4K: 4056}[lay] / (3 * n)
		if lay == layoutUnicode4K {
			le.PutUint16(b[root+4056:], uint16(cnt))
		} else {
			b[root+cbEntOff-2] = byte(cnt)
		}
		b[root+cbEntOff] = byte(3 * n)
		for i := 0; i < cnt; i++ {
			e := b[root+i*3*n:]
			putID(e, n, 0)
			putID(e[2*n:], n, uint64(root))
		}
		p, _ := parseHeader(bytes.NewReader(b), int64(len(b)))
		calls := 0
		err = p.walkNodes(func(nbtEntry) error { calls++; return nil })
		if err == nil {
			t.Errorf("%s：自指的 B 树没有报错", lay)
		}
	}
}

func putID(b []byte, n int, v uint64) {
	if n == 4 {
		le.PutUint32(b, uint32(v))
	} else {
		le.PutUint64(b, v)
	}
}
