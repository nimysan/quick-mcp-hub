package tools

import (
	"strings"
	"testing"

	"github.com/nimysan/mcp-hub/internal/httpchan"
)

func rt(account string, write bool) httpchan.Runtime {
	settings := map[string]string{}
	if write {
		settings[httpchan.AllowWriteField] = "true"
	}
	return httpchan.Runtime{
		Spec:     httpchan.Spec{Type: "demo", Name: "Demo"},
		Account:  account,
		Settings: settings,
	}
}

func index(rts []httpchan.Runtime) (map[string]httpchan.Runtime, []string) {
	m := map[string]httpchan.Runtime{}
	var accs []string
	for _, r := range rts {
		m[r.Account] = r
		accs = append(accs, r.Account)
	}
	return m, accs
}

// 单实例时不该强迫模型传账号参数。
func TestPickRuntimeSingleInstanceNeedsNoAccount(t *testing.T) {
	m, accs := index([]httpchan.Runtime{rt("a@x.com", false)})
	got, err := pickRuntime(m, accs, nil)
	if err != nil {
		t.Fatalf("单实例不该要求账号参数: %v", err)
	}
	if got.Account != "a@x.com" {
		t.Errorf("选错实例: %s", got.Account)
	}
}

// 多实例时必须明确指定 —— 替模型猜一个默认账号，在写入类工具上就是选错对象。
func TestPickRuntimeMultiInstance(t *testing.T) {
	m, accs := index([]httpchan.Runtime{rt("a", false), rt("b", true)})

	if _, err := pickRuntime(m, accs, nil); err == nil ||
		!strings.Contains(err.Error(), accountParam) {
		t.Fatalf("多实例缺账号参数应当报错并点出参数名，实际: %v", err)
	}
	if _, err := pickRuntime(m, accs, map[string]any{accountParam: "  "}); err == nil {
		t.Error("空白账号应当被拒")
	}
	if _, err := pickRuntime(m, accs, map[string]any{accountParam: "c"}); err == nil ||
		!strings.Contains(err.Error(), "a / b") {
		t.Fatalf("未知账号的报错里要列出可选项，实际: %v", err)
	}
	got, err := pickRuntime(m, accs, map[string]any{accountParam: "b"})
	if err != nil || got.Account != "b" {
		t.Fatalf("指定 b 应当选到 b: %v %q", err, got.Account)
	}
}

func TestInputSchemaAccountParam(t *testing.T) {
	tool := httpchan.Tool{
		Name: "demo_get", Method: "GET", Path: "/x",
		Params: []httpchan.Param{
			{Name: "id", In: httpchan.InQuery, Required: true},
			{Name: "state", In: httpchan.InQuery, Enum: []string{"open", "closed"}},
		},
	}

	single := inputSchema(tool, []string{"a"})
	if _, ok := single["properties"].(map[string]any)[accountParam]; ok {
		t.Error("单实例不该出现账号参数")
	}
	if req := single["required"].([]string); len(req) != 1 || req[0] != "id" {
		t.Errorf("单实例必填项不对: %v", req)
	}

	multi := inputSchema(tool, []string{"a", "b"})
	props := multi["properties"].(map[string]any)
	acc, ok := props[accountParam].(map[string]any)
	if !ok {
		t.Fatal("多实例应当加上账号参数")
	}
	if enum := acc["enum"].([]any); len(enum) != 2 || enum[0] != "a" {
		t.Errorf("账号枚举不对: %v", enum)
	}
	req := multi["required"].([]string)
	if len(req) != 2 || req[0] != accountParam {
		t.Errorf("账号参数应当必填且排在前面: %v", req)
	}
	// 业务参数的枚举不能被账号参数的处理顺手覆盖掉。
	if enum := props["state"].(map[string]any)["enum"].([]any); len(enum) != 2 {
		t.Errorf("业务参数枚举丢了: %v", enum)
	}
}

// 「A 账号可写、B 账号只读」要在描述里讲清楚，否则模型选错账号才知道被拒。
func TestDescribeNamesWritableAccounts(t *testing.T) {
	spec := httpchan.Spec{Type: "demo", Name: "Demo"}
	tool := httpchan.Tool{Name: "demo_create", Description: "建一条。"}
	got := describe(spec, tool, []string{"a", "b"}, []string{"b"})
	if !strings.Contains(got, "只有这些账号打开了写入开关：b") {
		t.Errorf("描述里没点出可写账号: %s", got)
	}
	ro := describe(spec, httpchan.Tool{Description: "查一条。", ReadOnly: true}, []string{"a"}, nil)
	if strings.Contains(ro, "写入开关") {
		t.Errorf("只读工具不该提写入开关: %s", ro)
	}
}
