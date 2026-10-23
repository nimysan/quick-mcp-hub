// catalog try：拿真实凭据把一份声明跑一遍，逐条报告声明里的字段路径在真实
// 响应里到底存不存在。
//
// 这是 -catalog-validate 管不了的那一半。validate 只看声明的形状；try 回答的
// 是「接口真的这么返回吗」—— Jira 的 /search/jql 默认只回 id、金蝶的 View
// 成功时数据到底在哪一层，这类问题只有真跑一次才暴露，而不跑的后果是声明
// 看起来成功、只是「少几个字段」，因为取不到的路径会被静默丢掉。
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// fieldFlags 收集可重复的 -field k=v。
type fieldFlags map[string]string

func (f fieldFlags) String() string { return "" }

func (f fieldFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("要写成 名字=值 的形式，收到 %q", v)
	}
	f[strings.TrimSpace(k)] = val
	return nil
}

type tryOptions struct {
	path    string
	account string
	fields  fieldFlags
	tool    string
	args    fieldFlags
	// write 为真时也跑写工具。默认只跑只读工具 —— 一份声明的作者自测不该
	// 顺手在别人的系统里建单据。
	write bool
}

func runCatalogTry(store *credstore.Store, o tryOptions) int {
	data, err := os.ReadFile(o.path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		return 2
	}
	spec, err := httpchan.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "声明不合法: %v\n", err)
		return 1
	}

	account := strings.TrimSpace(o.account)
	if account == "" {
		account = o.fields[spec.Account.Name]
	}
	if account == "" {
		fmt.Fprintf(os.Stderr, "需要账号：用 -account 指定，或 -field %s=…\n", spec.Account.Name)
		return 2
	}

	// 凭据优先从密码管理器取，取不到再从标准输入读 —— 绝不从命令行参数接收，
	// argv 对同机其他进程可见，也会进 shell 历史。
	ref := credstore.Ref{Integration: spec.Integration, Account: account}
	secret := store.Secret(ref)
	if _, err := secret.Value(); err != nil {
		typed, rerr := readSecret(fmt.Sprintf("%s/%s 的凭据（不回显）: ", spec.Integration, account))
		if rerr != nil || typed == "" {
			fmt.Fprintf(os.Stderr, "没有可用凭据：%v\n", err)
			return 2
		}
		secret = credstore.Literal(ref, typed)
	}

	settings := map[string]string{}
	for k, v := range o.fields {
		if k != spec.Account.Name && k != spec.Secret.Name {
			settings[k] = v
		}
	}
	// 没给的附加字段用声明里的默认值，缺必填的要当场说清
	var missingFields []string
	for _, f := range spec.Fields {
		if settings[f.Name] == "" {
			settings[f.Name] = f.Default
		}
		if f.Required && settings[f.Name] == "" {
			missingFields = append(missingFields, f.Name)
		}
	}
	if len(missingFields) > 0 {
		fmt.Fprintf(os.Stderr, "缺必填字段，用 -field 补上：%s\n", strings.Join(missingFields, ", "))
		return 2
	}

	rt := httpchan.Runtime{
		Spec:     spec,
		Account:  account,
		Settings: settings,
		Secret:   secret.Value,
	}
	base, err := rt.BaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "接入点不可用: %v\n", err)
		return 2
	}

	fmt.Printf("声明   : %s v%d（%s）\n", spec.ID, spec.Version, spec.Name)
	fmt.Printf("接入点 : %s\n", base)
	fmt.Printf("账号   : %s（凭据来自 %s）\n", account, secretSource(secret))
	fmt.Println()

	client := &httpchan.Client{Trace: &httpchan.Trace{}}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 第一步：连通性。它不通，后面全是噪音。
	fmt.Print("连通性测试 … ")
	id, err := client.Test(ctx, rt)
	if err != nil {
		fmt.Println("❌")
		fmt.Fprintf(os.Stderr, "  %v\n", err)
		return 1
	}
	fmt.Printf("✅ %v\n\n", id)

	tools := pickTools(spec, o)
	if len(tools) == 0 {
		fmt.Println("没有可跑的工具（只读工具都需要必填参数时，用 -try-tool 和 -try-arg 指定一个）")
		return 0
	}

	problems := 0
	for _, t := range tools {
		args := map[string]any{}
		if t.Name == o.tool {
			for k, v := range o.args {
				args[k] = v
			}
		}
		if miss := requiredMissing(t, args); len(miss) > 0 {
			fmt.Printf("· %-26s ⏭  跳过：需要参数 %s（用 -try-tool %s -try-arg k=v）\n",
				t.Name, strings.Join(miss, ","), t.Name)
			continue
		}

		out, meta, err := client.Call(ctx, rt, t.Name, args)
		tr := client.Trace
		if err != nil {
			fmt.Printf("· %-26s ❌ %v\n", t.Name, err)
			problems++
			continue
		}

		status := "✅"
		var notes []string
		if tr.Items == 0 {
			status = "⚠"
			notes = append(notes, "返回空列表，字段路径没验到（换个有数据的参数再跑）")
		}
		if len(tr.Missing) > 0 {
			status = "❌"
			notes = append(notes, "响应里没有这些键："+strings.Join(tr.Missing, "，")+
				" —— 声明可能写错了，或该字段在这个项目/实例上没启用")
			problems++
		}
		if len(tr.Nulls) > 0 {
			// 字段在、值是空：这是数据状态，不是声明的问题。
			notes = append(notes, "值为空（数据如此，非声明问题）："+strings.Join(tr.Nulls, "，"))
		}
		if len(tr.MetaMissing) > 0 {
			// meta 里的分页字段常常只在「有下一页」时才出现，缺失往往是正常的，
			// 所以这不算问题，只作提示 —— 把正常情况标成警告会让真问题被淹掉。
			notes = append(notes, "本次没返回 "+strings.Join(tr.MetaMissing, " ")+
				"（分页字段常常只在有下一页时出现）")
		}

		count := ""
		if tr.Items >= 0 {
			count = fmt.Sprintf("%d 条", tr.Items)
		}
		fmt.Printf("· %-26s %s %-6s %s\n", t.Name, status, count, firstLine(notes))
		for _, n := range notes[min(1, len(notes)):] {
			fmt.Printf("  %-28s %s\n", "", n)
		}
		if len(meta) > 0 {
			fmt.Printf("  %-28s meta: %v\n", "", sortedKeys(meta))
		}
		_ = out
	}

	fmt.Println()
	if problems > 0 {
		fmt.Printf("有 %d 个工具的声明和真实响应不一致，上面标 ❌ 的要改声明。\n", problems)
		return 1
	}
	fmt.Println("全部通过。")
	return 0
}

// pickTools 决定跑哪些工具：默认所有只读工具，-try-tool 指定单个，
// -try-write 才带上写工具。
func pickTools(spec httpchan.Spec, o tryOptions) []httpchan.Tool {
	var out []httpchan.Tool
	for _, t := range spec.Tools {
		switch {
		case o.tool != "":
			if t.Name == o.tool {
				out = append(out, t)
			}
		case t.ReadOnly || o.write:
			out = append(out, t)
		}
	}
	return out
}

func requiredMissing(t httpchan.Tool, args map[string]any) []string {
	var miss []string
	for _, p := range t.Params {
		if !p.Required || p.Default != "" {
			continue
		}
		if v, ok := args[p.Name]; !ok || v == "" {
			miss = append(miss, p.Name)
		}
	}
	return miss
}

func secretSource(s *credstore.Secret) string {
	if src := s.Source(); src != "" {
		return src
	}
	return "未知"
}

func firstLine(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return notes[0]
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
