package feishuweb

import (
	"encoding/json"
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "重写 testdata 里的期望输出")

func loadBlock(t *testing.T, name string) *Block {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var b Block
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return &b
}

// 造的块树覆盖 CDC 支持的主要块类型，期望输出逐字比对。
func TestConvertGolden(t *testing.T) {
	got := Convert(loadBlock(t, "doc.json"), Limits{})
	if got.Truncated {
		t.Fatal("不该截断")
	}
	const golden = "testdata/doc.md"
	if *update {
		if err := os.WriteFile(golden, []byte(got.Markdown+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.Markdown+"\n" != string(want) {
		t.Errorf("输出和 %s 不一致：\n%s", golden, got.Markdown)
	}
	if got.Counts.Skipped != 1 { // 只有内嵌电子表格被跳过
		t.Errorf("skipped = %d, want 1", got.Counts.Skipped)
	}
}

// 文档内容是不可信的：不能造出 HTML、可点的 javascript: 链接、标题或图片。
func TestConvertEscapesHostileContent(t *testing.T) {
	md := Convert(loadBlock(t, "hostile.json"), Limits{}).Markdown
	// 没被转义的 < 才会被渲染成 HTML
	if regexp.MustCompile(`(^|[^\\])<`).MatchString(md) {
		t.Errorf("输出里有没转义的尖括号：\n%s", md)
	}
	for _, bad := range []string{"data:text", "onload=\"", "\n# "} {
		if strings.Contains(md, bad) {
			t.Errorf("输出里出现了 %q：\n%s", bad, md)
		}
	}
	// 没被转义的 ]( 后面跟 javascript: 才是真链接
	if regexp.MustCompile(`(^|[^\\])\]\(javascript:`).MatchString(md) {
		t.Errorf("输出里有可点的 javascript: 链接：\n%s", md)
	}
	for _, want := range []string{
		`\<script\>`,                  // 文字里的尖括号被转义
		`\[点我\](javascript:alert(1))`, // 文字造不出链接
		"坏链接",                         // 坏链接只留文字
		"[括号链接](https://example.com/a%29%28b)", // 括号被编码，不会提前闭合
		`\# 伪装成标题`,                             // 行首 # 被转义
		"```alert(5)",                          // 不合法的语言名被丢掉
	} {
		if !strings.Contains(strings.ReplaceAll(md, "\n", ""), strings.ReplaceAll(want, "\n", "")) {
			t.Errorf("输出里缺少 %q：\n%s", want, md)
		}
	}
}

func TestConvertLimits(t *testing.T) {
	root := &Block{Type: "page"}
	for i := 0; i < 50; i++ {
		root.Children = append(root.Children, &Block{Type: "text", Ops: []Op{{Insert: strings.Repeat("字", 100)}}})
	}
	got := Convert(root, Limits{MaxRunes: 1000})
	if !got.Truncated || !strings.Contains(got.Markdown, "已截断") {
		t.Fatalf("应当截断：%+v", got.Counts)
	}
	if n := got.Counts.OutputRunes; n > 1100 {
		t.Errorf("截断后还有 %d 字", n)
	}

	got = Convert(root, Limits{MaxBlocks: 10})
	if !got.Truncated || got.Counts.Blocks != 10 {
		t.Errorf("块数上限没生效：%+v", got.Counts)
	}

	// 层数上限：一条 200 层的引用链不能把栈打爆。
	deep := &Block{Type: "page"}
	cur := deep
	for i := 0; i < 200; i++ {
		next := &Block{Type: "quote_container"}
		cur.Children = []*Block{next}
		cur = next
	}
	if got := Convert(deep, Limits{}); !got.Truncated {
		t.Error("层数上限没生效")
	}
}

func TestSafeURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com/x":        "https://example.com/x",
		"http://example.com/a b":       "http://example.com/a%20b",
		"mailto:a@example.com":         "mailto:a@example.com",
		"javascript:alert(1)":          "",
		"JaVaScRiPt:alert(1)":          "",
		"data:text/html,x":             "",
		"vbscript:x":                   "",
		"/relative":                    "",
		"https://":                     "",
		"https://example.com/\x00evil": "",
	}
	for in, want := range cases {
		if got := safeURL(in); got != want {
			t.Errorf("safeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanTitle(t *testing.T) {
	if got := CleanTitle("\u200b标题\n第二行\x07"); got != "标题 第二行" {
		t.Errorf("CleanTitle = %q", got)
	}
	if got := []rune(CleanTitle(strings.Repeat("长", 500))); len(got) != 201 {
		t.Errorf("标题没截断：%d", len(got))
	}
}
