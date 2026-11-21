package olkwin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDump 是真机探测用的诊断输出：OLKWIN_DUMP=<IndexedDB 目录>。
// 只打印库名、仓库名、记录数和字段结构；字符串只给长度和前 12 个字符，正文不打印。
func TestDump(t *testing.T) {
	root := os.Getenv("OLKWIN_DUMP")
	if root == "" {
		t.Skip("设置 OLKWIN_DUMP=<...\\EBWebView\\Default\\IndexedDB> 才运行")
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), "indexeddb.leveldb") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		blob := strings.TrimSuffix(dir, ".leveldb") + ".blob"
		t0 := time.Now()
		s, err := openIDB(dir, blob)
		if err != nil {
			t.Logf("%s: %v", e.Name(), err)
			continue
		}
		fmt.Printf("== %s comparator=%q files=%v entries=%d load=%v\n", e.Name(), s.ldb.comparator, s.ldb.files, len(s.ldb.entries), time.Since(t0))
		var dbs []int64
		for id := range s.schema.dbs {
			dbs = append(dbs, id)
		}
		sort.Slice(dbs, func(i, j int) bool { return dbs[i] < dbs[j] })
		maxShow := 2
		if v := os.Getenv("OLKWIN_DUMP_N"); v != "" {
			fmt.Sscan(v, &maxShow)
		}
		filter := os.Getenv("OLKWIN_DUMP_STORE")
		for _, db := range dbs {
			fmt.Printf("db %d %q\n", db, s.schema.dbs[db])
			var sids []int64
			for sid := range s.schema.stores[db] {
				sids = append(sids, sid)
			}
			sort.Slice(sids, func(i, j int) bool { return sids[i] < sids[j] })
			for _, sid := range sids {
				name := s.schema.stores[db][sid]
				recs := s.records(db, sid)
				bad := 0
				var firstErr error
				for _, r := range recs {
					if _, err := s.decode(db, sid, r); err != nil {
						bad++
						if firstErr == nil {
							firstErr = err
						}
					}
				}
				fmt.Printf("  store %d %q records=%d undecodable=%d %v\n", sid, name, len(recs), bad, firstErr)
				if filter != "" && !strings.Contains(","+filter+",", ","+name+",") {
					continue
				}
				shown := 0
				for _, r := range recs {
					if shown >= maxShow {
						break
					}
					v, err := s.decode(db, sid, r)
					if err != nil {
						fmt.Printf("    key=%.40s ERR %v head=% x\n", r.key, err, head(r.val, 24))
						continue
					}
					if m := os.Getenv("OLKWIN_DUMP_MATCH"); m != "" {
						o, _ := v.(*jsObject)
						subj, _ := o.get("Subject").(string)
						if !strings.Contains(subj, m) {
							continue
						}
					}
					shown++
					fmt.Printf("    key=%.40s\n", r.key)
					shape(v, "      ", 0)
				}
			}
		}
	}
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func shape(v any, ind string, depth int) {
	if depth > 6 {
		fmt.Printf("%s...\n", ind)
		return
	}
	switch x := v.(type) {
	case *jsObject:
		for _, k := range x.keys {
			switch c := x.vals[k].(type) {
			case *jsObject, []any:
				fmt.Printf("%s%s:\n", ind, k)
				shape(c, ind+"  ", depth+1)
			default:
				fmt.Printf("%s%s = %s\n", ind, k, brief(c))
			}
		}
	case []any:
		fmt.Printf("%s[%d items]\n", ind, len(x))
		if len(x) > 0 {
			shape(x[0], ind+"  ", depth+1)
		}
	default:
		fmt.Printf("%s%s\n", ind, brief(v))
	}
}

func brief(v any) string {
	switch x := v.(type) {
	case string:
		r := []rune(x)
		if len(r) > 12 {
			return fmt.Sprintf("str(%d) %q…", len(r), string(r[:12]))
		}
		return fmt.Sprintf("str %q", x)
	case []byte:
		return fmt.Sprintf("bytes(%d) % x", len(x), head(x, 8))
	case time.Time:
		return "date " + x.Format(time.RFC3339)
	case nil:
		return "null"
	case jsUndefined:
		return "undefined"
	default:
		return fmt.Sprintf("%T %v", v, v)
	}
}
