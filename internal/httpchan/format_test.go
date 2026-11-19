package httpchan

import (
	"strconv"
	"testing"
	"time"
)

func TestFormatTime(t *testing.T) {
	loc := time.Local
	want := time.Date(2026, 11, 20, 15, 0, 0, 0, loc)
	cases := map[string]string{
		"2026-11-20 15:00":                      strconv.FormatInt(want.Unix(), 10),
		"2026-11-20T15:00:00":                   strconv.FormatInt(want.Unix(), 10),
		want.Format(time.RFC3339):               strconv.FormatInt(want.Unix(), 10),
		strconv.FormatInt(want.Unix(), 10):      strconv.FormatInt(want.Unix(), 10),
		strconv.FormatInt(want.UnixMilli(), 10): strconv.FormatInt(want.Unix(), 10),
	}
	for in, out := range cases {
		got, err := formatTime(Param{Name: "t", Format: "unix"}, in)
		if err != nil || got != out {
			t.Errorf("%q → %v (%v)，期望 %s", in, got, err, out)
		}
	}
	day, _ := formatTime(Param{Name: "t", Format: "unix_ms"}, "2026-11-20")
	if day != strconv.FormatInt(time.Date(2026, 11, 20, 0, 0, 0, 0, loc).UnixMilli(), 10) {
		t.Errorf("日期 → %v", day)
	}
	r, _ := formatTime(Param{Name: "t", Format: "rfc3339"}, "2026-11-20 15:00")
	if r != want.Format(time.RFC3339) {
		t.Errorf("rfc3339 → %v", r)
	}
	if _, err := formatTime(Param{Name: "t", Format: "unix"}, "下周三"); err == nil {
		t.Error("认不出的时间应报错")
	}
}

func TestAsArrayAndNow(t *testing.T) {
	p := Param{Name: "ids", Type: "array", ItemObject: map[string]any{"type": "user", "user_id": "{{item}}"}}
	got, err := asArray(p, []any{"ou_a", " ou_b "})
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(got, []any{map[string]any{"type": "user", "user_id": "ou_a"}, map[string]any{"type": "user", "user_id": "ou_b"}}) {
		t.Fatalf("got %v", got)
	}
	if got, _ := asArray(Param{Name: "x", Type: "array"}, "a, b"); !sameJSON(got, []any{"a", "b"}) {
		t.Fatalf("逗号分隔 → %v", got)
	}
	if _, err := asArray(p, 3.0); err == nil {
		t.Fatal("非数组应报错")
	}
	now := time.UnixMilli(1794984236740)
	m := map[string]any{"task": map[string]any{"completed_at": "{{now_ms}}"}, "keep": "{{item}}"}
	fillNow(m, now)
	if m["task"].(map[string]any)["completed_at"] != "1794984236740" || m["keep"] != "{{item}}" {
		t.Fatalf("fillNow → %v", m)
	}
}
