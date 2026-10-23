package httpchan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 仿真的金蝶云星空 WebAPI：登录换 kdservice-sessionid cookie，
// 业务接口要带着它；业务失败回 HTTP 200 + 数组包着的 ResponseStatus。
//
// 用的是目录里那份真声明（ByType("kingdee")），不是为测试手搓的，
// 所以这组用例同时在验 kingdee.json 本身。
type k3mock struct {
	srv *httptest.Server

	mu        sync.Mutex
	logins    int              // 登录次数
	queries   int              // 查询次数
	bodies    []map[string]any // 收到的业务请求体
	password  string
	dropFirst bool // 第一次业务请求假装会话已失效
	failQuery bool // 查询回业务失败
}

func newK3Mock(t *testing.T, password string) *k3mock {
	m := &k3mock{password: password}
	mux := http.NewServeMux()

	mux.HandleFunc("/k3cloud/Kingdee.BOS.WebApi.ServicesStub.AuthService.ValidateUser.common.kdsvc",
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.logins++
			m.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if body["password"] != m.password {
				json.NewEncoder(w).Encode(map[string]any{
					"LoginResultType": -5, "Message": "用户名或密码错误",
				})
				return
			}
			// 账套、语言、账号都要按声明传过来
			if body["acctid"] != "6287abc" || body["username"] != "integration_ro" {
				json.NewEncoder(w).Encode(map[string]any{
					"LoginResultType": -1, "Message": "账套或账号不对: " + fmt.Sprint(body),
				})
				return
			}
			if lcid, _ := body["lcid"].(float64); lcid != 2052 {
				json.NewEncoder(w).Encode(map[string]any{"LoginResultType": -1, "Message": "lcid 不对"})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "kdservice-sessionid", Value: "sess-1", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{
				"LoginResultType": 1,
				"Context":         map[string]any{"UserName": body["username"]},
			})
		})

	bizFail := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Content-Type", "application/json")
		// 失败时金蝶回的是「数组包着一个对象」
		json.NewEncoder(w).Encode([]any{map[string]any{
			"Result": map[string]any{"ResponseStatus": map[string]any{
				"IsSuccess": false,
				"Errors":    []any{map[string]any{"Message": msg}},
			}},
		}})
	}

	mux.HandleFunc("/k3cloud/Kingdee.BOS.WebApi.ServicesStub.DynamicFormService.ExecuteBillQuery.common.kdsvc",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			m.mu.Lock()
			m.queries++
			m.bodies = append(m.bodies, body)
			drop := m.dropFirst && m.queries == 1
			fail := m.failQuery
			m.mu.Unlock()

			if _, err := r.Cookie("kdservice-sessionid"); err != nil || drop {
				bizFail(w, "会话信息已丢失，请重新登录")
				return
			}
			if fail {
				bizFail(w, "字段 FNoSuchField 不存在")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]any{
				[]any{"001", "物料甲", 12.5},
				[]any{"002", "物料乙", 8},
			})
		})

	mux.HandleFunc("/k3cloud/Kingdee.BOS.WebApi.ServicesStub.DynamicFormService.View.common.kdsvc",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			m.mu.Lock()
			m.bodies = append(m.bodies, body)
			m.mu.Unlock()
			if _, err := r.Cookie("kdservice-sessionid"); err != nil {
				bizFail(w, "会话信息已丢失，请重新登录")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
				"ResponseStatus": map[string]any{"IsSuccess": true},
				"Result":         map[string]any{"Number": "001", "Name": "物料甲"},
			}})
		})

	m.srv = httptest.NewTLSServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *k3mock) stats() (logins, queries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logins, m.queries
}

func (m *k3mock) lastBody() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return nil
	}
	return m.bodies[len(m.bodies)-1]
}

// runtime 按真声明装配一个运行态，站点指向仿真服务。
func (m *k3mock) runtime(t *testing.T, password string, allowWrite bool) (Runtime, *Client) {
	t.Helper()
	spec, ok := ByType("kingdee")
	if !ok {
		t.Fatal("目录里没有 kingdee 声明")
	}
	settings := map[string]string{"site": m.srv.URL + "/k3cloud", "acctId": "6287abc"}
	if allowWrite {
		settings[AllowWriteField] = "true"
	}
	rt := Runtime{
		Spec:     spec,
		Account:  "integration_ro",
		Settings: settings,
		Secret:   func() (string, error) { return password, nil },
	}
	return rt, &Client{HTTP: m.srv.Client()}
}

func TestKingdeeLoginThenQuery(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	rt, c := m.runtime(t, "pw-secret-1", false)

	out, err := c.Call(context.Background(), rt, "kingdee_query_bill", map[string]any{
		"formId":       "BD_MATERIAL",
		"fieldKeys":    "FNumber,FName,FPrice",
		"filterString": "FNumber like '00%'",
		"limit":        float64(10),
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	// body 必须是嵌套的 {"data":{...}}，不是平铺
	body := m.lastBody()
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("body 不是 {\"data\":{…}} 形状: %v", body)
	}
	if data["FormId"] != "BD_MATERIAL" || data["FieldKeys"] != "FNumber,FName,FPrice" {
		t.Errorf("data 内容不对: %v", data)
	}
	if data["FilterString"] != "FNumber like '00%'" {
		t.Errorf("过滤条件没传进去: %v", data["FilterString"])
	}
	if data["Limit"] != float64(10) || data["StartRow"] != float64(0) {
		t.Errorf("Limit/StartRow 不对（默认值也要生效）: %v %v", data["Limit"], data["StartRow"])
	}

	// 二维数组要按 fieldKeys 拼成对象
	rows, ok := out.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("结果应当是 2 行，实际 %T %v", out, out)
	}
	first := rows[0].(map[string]any)
	if first["FNumber"] != "001" || first["FName"] != "物料甲" || first["FPrice"] != 12.5 {
		t.Errorf("列名没对上: %v", first)
	}
}

// 登录只能发生一次：MCP server 是常驻进程，每次调用都登一遍会被 ERP 当成攻击。
func TestKingdeeLoginOnlyOnce(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	rt, c := m.runtime(t, "pw-secret-1", false)
	args := map[string]any{"formId": "BD_MATERIAL", "fieldKeys": "FNumber"}

	for i := 0; i < 3; i++ {
		if _, err := c.Call(context.Background(), rt, "kingdee_query_bill", args); err != nil {
			t.Fatalf("第 %d 次查询失败: %v", i+1, err)
		}
	}
	logins, queries := m.stats()
	if logins != 1 || queries != 3 {
		t.Errorf("应当登录 1 次、查询 3 次，实际 %d / %d", logins, queries)
	}
}

// 会话过期要自动重登并重试一次，否则常驻进程「用一会儿就全挂」。
func TestKingdeeReloginOnSessionLost(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	m.dropFirst = true
	rt, c := m.runtime(t, "pw-secret-1", false)

	out, err := c.Call(context.Background(), rt, "kingdee_query_bill", map[string]any{
		"formId": "BD_MATERIAL", "fieldKeys": "FNumber,FName,FPrice",
	})
	if err != nil {
		t.Fatalf("应当重登后成功，实际: %v", err)
	}
	if rows, ok := out.([]any); !ok || len(rows) != 2 {
		t.Errorf("重试后的结果不对: %v", out)
	}
	logins, queries := m.stats()
	if logins != 2 || queries != 2 {
		t.Errorf("应当登录 2 次、查询 2 次，实际 %d / %d", logins, queries)
	}
}

// HTTP 200 + 业务失败必须报错：当成成功的话模型会拿着空结果编结论。
func TestKingdeeBusinessFailureIsError(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	m.failQuery = true
	rt, c := m.runtime(t, "pw-secret-1", false)

	_, err := c.Call(context.Background(), rt, "kingdee_query_bill", map[string]any{
		"formId": "BD_MATERIAL", "fieldKeys": "FNoSuchField",
	})
	if err == nil {
		t.Fatal("业务失败应当是错误")
	}
	if !strings.Contains(err.Error(), "FNoSuchField 不存在") {
		t.Errorf("错误信息里要带上金蝶的说法: %v", err)
	}
}

func TestKingdeeBadPassword(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	rt, c := m.runtime(t, "wrong-password", false)

	_, err := c.Call(context.Background(), rt, "kingdee_query_bill", map[string]any{
		"formId": "BD_MATERIAL", "fieldKeys": "FNumber",
	})
	if err == nil {
		t.Fatal("密码错应当报错")
	}
	if !strings.Contains(err.Error(), "LoginResultType") {
		t.Errorf("错误信息要点出登录判定字段: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Error("错误信息里泄漏了密码")
	}
	if _, queries := m.stats(); queries != 0 {
		t.Error("登录没过就不该发业务请求")
	}
}

// Save 是写入：写开关关着时必须在发请求之前就拒绝。
func TestKingdeeSaveNeedsWriteSwitch(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	rt, c := m.runtime(t, "pw-secret-1", false)

	_, err := c.Call(context.Background(), rt, "kingdee_save_bill", map[string]any{
		"formId": "BD_MATERIAL", "model": map[string]any{"FNumber": "003"},
	})
	if err == nil || !strings.Contains(err.Error(), "写开关") {
		t.Fatalf("应当被写开关拦下，实际: %v", err)
	}
	if logins, _ := m.stats(); logins != 0 {
		t.Error("被拒绝的调用连登录都不该发")
	}
}

// 测试连通对登录式渠道就是登录本身，并且每次都真登一遍。
func TestKingdeeTestIsLogin(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	rt, c := m.runtime(t, "pw-secret-1", false)

	for i := 0; i < 2; i++ {
		got, err := c.Test(context.Background(), rt)
		if err != nil {
			t.Fatalf("测试连通失败: %v", err)
		}
		if gm := got.(map[string]any); gm["loggedIn"] != true || gm["account"] != "integration_ro" {
			t.Errorf("返回的身份不对: %v", gm)
		}
	}
	if logins, _ := m.stats(); logins != 2 {
		t.Errorf("测试连通应当每次都真登一遍，实际登了 %d 次", logins)
	}
}

// View 的 formid 在 body 顶层、单据条件在 data 下 —— 和查询不是同一个形状。
func TestKingdeeViewBodyShape(t *testing.T) {
	m := newK3Mock(t, "pw-secret-1")
	rt, c := m.runtime(t, "pw-secret-1", false)

	if _, err := c.Call(context.Background(), rt, "kingdee_view_bill", map[string]any{
		"formId": "BD_MATERIAL", "number": "001",
	}); err != nil {
		t.Fatalf("读取单据失败: %v", err)
	}
	body := m.lastBody()
	if body["formid"] != "BD_MATERIAL" {
		t.Errorf("formid 应当在 body 顶层: %v", body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok || data["Number"] != "001" || data["CreateOrgId"] != float64(0) {
		t.Errorf("data 内容不对: %v", body["data"])
	}
	if _, leaked := data["FormId"]; leaked {
		t.Error("View 的 data 里不该有 FormId")
	}
}
