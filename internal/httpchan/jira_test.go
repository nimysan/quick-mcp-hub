package httpchan

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 仿真的 Jira Cloud REST API。两处是这份声明的要点，所以着重验：
//
//   - 认证是 Basic(邮箱:API Token)，不是 Bearer
//   - 搜索走 /rest/api/3/search/jql（旧的 /rest/api/3/search 已被 Atlassian
//     完全下线），而且它默认只回事项 ID，必须显式传 fields
//   - 读详情和写评论走 /rest/api/2/：v3 的富文本是 ADF 深层嵌套，v2 是纯
//     文本字符串，对模型来说只有 v2 可用
//
// 用的是目录里那份真声明（ByID("jira")），所以这组用例同时在验 jira.json。
type jiraMock struct {
	srv *httptest.Server

	mu         sync.Mutex
	email      string
	token      string
	gotAuth    string
	gotQuery   map[string]string
	gotPaths   []string
	comment    map[string]any
	created    map[string]any
	updated    map[string]any
	assignee   map[string]any
	transition map[string]any
}

func newJiraMock(t *testing.T, email, token string) *jiraMock {
	m := &jiraMock{email: email, token: token, gotQuery: map[string]string{}}
	mux := http.NewServeMux()

	// 认证检查：Basic(邮箱:Token)
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		m.mu.Lock()
		m.gotAuth = r.Header.Get("Authorization")
		m.gotPaths = append(m.gotPaths, r.URL.Path)
		for k := range r.URL.Query() {
			m.gotQuery[k] = r.URL.Query().Get(k)
		}
		m.mu.Unlock()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(m.email+":"+m.token))
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errorMessages":["Client must be authenticated to access this resource."],"errors":{}}`))
			return false
		}
		return true
	}

	mux.HandleFunc("/rest/api/3/myself", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"accountId": "5b10a2", "displayName": "叶工", "emailAddress": email,
			"timeZone": "Asia/Shanghai", "self": "https://x/rest/api/3/myself",
			"avatarUrls": map[string]any{"48x48": "https://noise"},
		})
	})

	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 真实接口在不传 fields 时只回 id —— 这里照着来，好让「默认值必须
		// 生效」这件事在测试里是可观测的。
		if r.URL.Query().Get("fields") == "" {
			json.NewEncoder(w).Encode(map[string]any{
				"issues": []any{map[string]any{"id": "10001"}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"nextPageToken": "tok2",
			"issues": []any{
				map[string]any{
					"id": "10001", "key": "ENG-123",
					"self":   "https://x/rest/api/3/issue/10001",
					"expand": "operations,versionedRepresentations,editmeta",
					"fields": map[string]any{
						"summary":  "登录偶发 500",
						"status":   map[string]any{"name": "In Progress"},
						"assignee": map[string]any{"displayName": "叶工"},
					},
				},
			},
		})
	})

	mux.HandleFunc("/rest/api/2/issue/ENG-123", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if r.Method == http.MethodPut {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			m.mu.Lock()
			m.updated = body
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "10001", "key": "ENG-123",
			"fields": map[string]any{
				// v2 的 description 是纯文本字符串；v3 这里会是 ADF 嵌套对象
				"description": "重现步骤：\n1. 连续登录 5 次\n2. 第 6 次返回 500",
				"summary":     "登录偶发 500",
				"status":      map[string]any{"name": "In Progress"},
				"assignee":    map[string]any{"displayName": "叶工"},
				"labels":      []any{"backend", "p1"},
				"created":     "2026-10-20T10:00:00.000+0800",
			},
		})
	})

	mux.HandleFunc("/rest/api/2/issue/ENG-123/comment", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			m.mu.Lock()
			m.comment = body
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "99", "created": "2026-10-23T16:40:00.000+0800",
				"author": map[string]any{"displayName": "叶工"},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"comments": []any{map[string]any{
				"id":      "88",
				"author":  map[string]any{"displayName": "小王", "accountId": "noise"},
				"body":    "我这边也复现了",
				"created": "2026-10-21T09:00:00.000+0800",
				"updated": "2026-10-21T09:00:00.000+0800",
			}},
		})
	})

	mux.HandleFunc("/rest/api/2/issue/createmeta/SCRUM/issuetypes", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":      2,
			"maxResults": 50,
			"issueTypes": []any{
				map[string]any{"id": "10004", "name": "Task", "subtask": false,
					"description": "Tasks track small, distinct pieces of work.",
					"iconUrl":     "https://noise/avatar"},
				map[string]any{"id": "10002", "name": "Subtask", "subtask": true,
					"description": "Subtasks track small pieces of work."},
			},
		})
	})

	// 建事项：POST /rest/api/2/issue
	mux.HandleFunc("/rest/api/2/issue", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.created = body
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "10050", "key": "ENG-456", "self": "https://x/rest/api/2/issue/10050",
		})
	})

	// 找人：这个接口的响应根就是一个数组，不是对象
	mux.HandleFunc("/rest/api/3/user/search", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{
			map[string]any{"accountId": "630db2", "displayName": "小王",
				"emailAddress": "wang@example.com", "active": true,
				"avatarUrls": map[string]any{"48x48": "https://noise"}},
		})
	})

	mux.HandleFunc("/rest/api/2/issue/ENG-123/transitions", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			m.mu.Lock()
			m.transition = body
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"transitions": []any{
			map[string]any{"id": "31", "name": "开始处理", "isAvailable": true,
				"to": map[string]any{"name": "In Progress", "id": "3"}},
			map[string]any{"id": "41", "name": "完成", "isAvailable": true,
				"to": map[string]any{"name": "Done", "id": "10001"}},
		}})
	})

	mux.HandleFunc("/rest/api/2/issue/ENG-123/assignee", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.assignee = body
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	m.srv = httptest.NewTLSServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *jiraMock) runtime(t *testing.T, token string, allowWrite bool) (Runtime, *Client) {
	t.Helper()
	spec, ok := ByID("jira")
	if !ok {
		t.Fatal("目录里没有 jira 声明")
	}
	settings := map[string]string{"site": m.srv.URL}
	if allowWrite {
		settings[AllowWriteField] = "true"
	}
	return Runtime{
		Spec:     spec,
		Account:  m.email,
		Settings: settings,
		Secret:   func() (string, error) { return token, nil },
	}, &Client{HTTP: m.srv.Client()}
}

func TestJiraBasicAuthAndTest(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	got, err := c.Test(context.Background(), rt)
	if err != nil {
		t.Fatalf("测试连通失败: %v", err)
	}
	id := got.(map[string]any)
	if id["displayName"] != "叶工" || id["emailAddress"] != "you@example.com" {
		t.Errorf("身份信息不对: %v", id)
	}
	// 测试响应只留身份三项，头像之类的噪音不该带出来
	if _, leaked := id["avatarUrls"]; leaked {
		t.Error("avatarUrls 应当被裁掉")
	}
	if !strings.HasPrefix(m.gotAuth, "Basic ") {
		t.Errorf("Jira 用 Basic(邮箱:Token)，不是 Bearer: %q", m.gotAuth)
	}
}

func TestJiraWrongToken(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "wrong-token-value", false)

	_, err := c.Test(context.Background(), rt)
	if err == nil {
		t.Fatal("Token 错应当报错")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "authenticated") {
		t.Errorf("错误信息要带上 Jira 的说法: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-token-value") {
		t.Error("错误信息里泄漏了 Token")
	}
}

// fields 默认值必须生效：/search/jql 不传 fields 时只回事项 ID，
// 模型会拿到一堆没有标题的空壳。
func TestJiraSearchSendsDefaultFields(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	out, _, err := c.Call(context.Background(), rt, "jira_search_issues", map[string]any{
		"jql": `project = ENG AND status != Done ORDER BY updated DESC`,
	})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if f := m.gotQuery["fields"]; !strings.Contains(f, "summary") || !strings.Contains(f, "status") {
		t.Errorf("fields 默认值没发出去: %q", f)
	}
	if m.gotQuery["maxResults"] != "25" {
		t.Errorf("maxResults 默认值不对: %q", m.gotQuery["maxResults"])
	}
	// JQL 里有空格、= 和 !=，必须被正确编码后又被服务端还原
	if want := `project = ENG AND status != Done ORDER BY updated DESC`; m.gotQuery["jql"] != want {
		t.Errorf("JQL 传输后不一致:\n  收到 %q\n  期望 %q", m.gotQuery["jql"], want)
	}

	rows := out.([]any)
	if len(rows) != 1 {
		t.Fatalf("应当返回 1 条，实际 %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["key"] != "ENG-123" {
		t.Errorf("事项键丢了: %v", first)
	}
	// fields 整棵子树保留（这样调用方自定义 fields 也不会被裁掉），
	// 但 self / expand / id 这些噪音要去掉
	fields := first["fields"].(map[string]any)
	if fields["summary"] != "登录偶发 500" {
		t.Errorf("fields 子树不完整: %v", fields)
	}
	for _, noise := range []string{"self", "expand", "id"} {
		if _, leaked := first[noise]; leaked {
			t.Errorf("噪音字段 %s 应当被裁掉: %v", noise, first)
		}
	}
}

// 读详情走 v2：v3 的 description 是 ADF 嵌套对象，模型读不了。
func TestJiraGetIssueUsesV2PlainText(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	out, _, err := c.Call(context.Background(), rt, "jira_get_issue", map[string]any{"issueKey": "ENG-123"})
	if err != nil {
		t.Fatalf("读取详情失败: %v", err)
	}
	if last := m.gotPaths[len(m.gotPaths)-1]; last != "/rest/api/2/issue/ENG-123" {
		t.Errorf("读详情应当走 v2（纯文本），实际打到 %s", last)
	}
	d := out.(map[string]any)
	desc, ok := d["fields.description"].(string)
	if !ok || !strings.Contains(desc, "重现步骤") {
		t.Errorf("描述应当是纯文本字符串: %#v", d["fields.description"])
	}
	if d["fields.status.name"] != "In Progress" || d["fields.assignee.displayName"] != "叶工" {
		t.Errorf("嵌套字段没取对: %v", d)
	}
}

func TestJiraListComments(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	out, _, err := c.Call(context.Background(), rt, "jira_list_comments", map[string]any{"issueKey": "ENG-123"})
	if err != nil {
		t.Fatalf("读评论失败: %v", err)
	}
	rows := out.([]any)
	first := rows[0].(map[string]any)
	if first["author.displayName"] != "小王" || first["body"] != "我这边也复现了" {
		t.Errorf("评论内容不对: %v", first)
	}
	if m.gotQuery["orderBy"] != "-created" {
		t.Errorf("orderBy 默认值没生效: %q", m.gotQuery["orderBy"])
	}
}

// 加评论是写操作：开关关着时必须在发请求之前就拒绝。
func TestJiraAddCommentNeedsWriteSwitch(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	_, _, err := c.Call(context.Background(), rt, "jira_add_comment", map[string]any{
		"issueKey": "ENG-123", "body": "收到",
	})
	if err == nil || !strings.Contains(err.Error(), "写开关") {
		t.Fatalf("应当被写开关拦下，实际: %v", err)
	}
	if m.comment != nil {
		t.Fatal("被拒绝的调用不该真的发出请求")
	}

	rt.Settings[AllowWriteField] = "true"
	out, _, err := c.Call(context.Background(), rt, "jira_add_comment", map[string]any{
		"issueKey": "ENG-123", "body": "我来看一下",
	})
	if err != nil {
		t.Fatalf("打开写开关后应当放行: %v", err)
	}
	// v2 的评论 body 是纯文本字符串，不是 ADF 文档对象
	if got, ok := m.comment["body"].(string); !ok || got != "我来看一下" {
		t.Errorf("评论 body 应当是纯文本字符串: %#v", m.comment["body"])
	}
	res := out.(map[string]any)
	if res["id"] != "99" || res["author.displayName"] != "叶工" {
		t.Errorf("返回的评论信息不对: %v", res)
	}
}

// 站点地址必须是 https，而且不带 /rest 前缀时才能拼出正确路径。
func TestJiraSiteURL(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, _ := m.runtime(t, "tok-abc-123456", false)

	rt.Settings["site"] = "http://insecure.atlassian.net"
	if _, err := rt.BaseURL(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("明文 http 站点应当被拒: %v", err)
	}
	rt.Settings["site"] = "https://demo.atlassian.net/"
	got, err := rt.BaseURL()
	if err != nil || got != "https://demo.atlassian.net" {
		t.Fatalf("站点地址处理不对: %q %v", got, err)
	}
}

func TestJiraListIssueTypes(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	out, meta, err := c.Call(context.Background(), rt, "jira_list_issue_types",
		map[string]any{"projectKey": "SCRUM"})
	if err != nil {
		t.Fatalf("读类型失败: %v", err)
	}
	rows := out.([]any)
	if len(rows) != 2 {
		t.Fatalf("应当返回 2 个类型，实际 %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "Task" || first["subtask"] != false {
		t.Errorf("类型内容不对: %v", first)
	}
	if _, leaked := first["iconUrl"]; leaked {
		t.Error("iconUrl 这类噪音应当被裁掉")
	}
	if meta["total"] != float64(2) {
		t.Errorf("total 没带出来: %v", meta)
	}
}

// 建事项的 body 是三层嵌套（fields.project.key / fields.issuetype.name），
// 拼错一层 Jira 就会拒；而没填的可选字段绝不能变成空对象 —— Jira 会把
// {"priority":{}} 当成非法值。
func TestJiraCreateIssueBodyShape(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", true)

	out, _, err := c.Call(context.Background(), rt, "jira_create_issue", map[string]any{
		"projectKey":  "SCRUM",
		"summary":     "登录偶发 500",
		"description": "重现步骤：\n1. 连续登录 5 次",
	})
	if err != nil {
		t.Fatalf("建事项失败: %v", err)
	}

	fields, ok := m.created["fields"].(map[string]any)
	if !ok {
		t.Fatalf("body 应当是 {\"fields\":{…}}: %v", m.created)
	}
	proj, ok := fields["project"].(map[string]any)
	if !ok || proj["key"] != "SCRUM" {
		t.Errorf("project.key 没拼对: %#v", fields["project"])
	}
	it, ok := fields["issuetype"].(map[string]any)
	if !ok || it["name"] != "Task" {
		t.Errorf("issuetype.name 应当落到默认值 Task: %#v", fields["issuetype"])
	}
	if fields["summary"] != "登录偶发 500" {
		t.Errorf("summary 不对: %v", fields["summary"])
	}
	// v2 的 description 是纯文本字符串，不是 ADF 文档对象
	if _, isStr := fields["description"].(string); !isStr {
		t.Errorf("description 应当是纯文本字符串: %#v", fields["description"])
	}
	// 没填的可选字段不该出现，更不该是空对象
	for _, k := range []string{"priority", "assignee"} {
		if v, present := fields[k]; present {
			t.Errorf("没填的 %s 不该出现在 body 里: %#v", k, v)
		}
	}
	res := out.(map[string]any)
	if res["key"] != "ENG-456" {
		t.Errorf("返回的事项键不对: %v", res)
	}
	if _, leaked := res["self"]; leaked {
		t.Error("self 应当被裁掉")
	}
}

func TestJiraCreateIssueNeedsWriteSwitch(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	_, _, err := c.Call(context.Background(), rt, "jira_create_issue", map[string]any{
		"projectKey": "SCRUM", "summary": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "写开关") {
		t.Fatalf("应当被写开关拦下，实际: %v", err)
	}
	if m.created != nil {
		t.Fatal("被拒绝的调用不该真的发出请求")
	}
}

// /rest/api/3/user/search 的响应根就是一个数组。没有 Path 声明时整个响应
// 就是列表，裁剪规则照样生效。
func TestJiraSearchUsersTopLevelArray(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	out, _, err := c.Call(context.Background(), rt, "jira_search_users", map[string]any{"query": "wang"})
	if err != nil {
		t.Fatalf("找人失败: %v", err)
	}
	rows, ok := out.([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("顶层数组响应应当直接变成列表: %#v", out)
	}
	u := rows[0].(map[string]any)
	if u["accountId"] != "630db2" || u["displayName"] != "小王" {
		t.Errorf("用户内容不对: %v", u)
	}
	if _, leaked := u["avatarUrls"]; leaked {
		t.Error("avatarUrls 应当被裁掉")
	}
	if m.gotQuery["maxResults"] != "20" {
		t.Errorf("maxResults 默认值没发出去: %q", m.gotQuery["maxResults"])
	}
}

// 可用流转取决于当前状态和项目工作流，所以必须先查 id 再流转；
// 目标状态名要跟着出来，否则模型只看到一串 id 不知道哪个是「完成」。
func TestJiraListTransitions(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	out, _, err := c.Call(context.Background(), rt, "jira_list_transitions",
		map[string]any{"issueKey": "ENG-123"})
	if err != nil {
		t.Fatalf("查流转失败: %v", err)
	}
	rows := out.([]any)
	if len(rows) != 2 {
		t.Fatalf("应当有 2 个流转，实际 %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["id"] != "31" || first["name"] != "开始处理" || first["to.name"] != "In Progress" {
		t.Errorf("流转内容不对（目标状态名必须在）: %v", first)
	}
}

func TestJiraTransitionIssue(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", true)

	out, _, err := c.Call(context.Background(), rt, "jira_transition_issue", map[string]any{
		"issueKey": "ENG-123", "transitionId": "41",
	})
	if err != nil {
		t.Fatalf("流转失败: %v", err)
	}
	tr, ok := m.transition["transition"].(map[string]any)
	if !ok || tr["id"] != "41" {
		t.Errorf(`body 应当是 {"transition":{"id":"41"}}: %v`, m.transition)
	}
	// 204 无响应体时要给一个明确的成功结果，不能是 null
	if res, ok := out.(map[string]any); !ok || res["ok"] != true {
		t.Errorf("204 应当回一个成功对象: %#v", out)
	}
}

// 改事项只发传了的字段：把没传的也发出去（哪怕是空值）会把原有内容清掉。
func TestJiraUpdateIssueOnlySendsGivenFields(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", true)

	if _, _, err := c.Call(context.Background(), rt, "jira_update_issue", map[string]any{
		"issueKey": "ENG-123", "summary": "改过的标题",
	}); err != nil {
		t.Fatalf("改事项失败: %v", err)
	}
	fields := m.updated["fields"].(map[string]any)
	if fields["summary"] != "改过的标题" {
		t.Errorf("summary 没发出去: %v", fields)
	}
	for _, k := range []string{"description", "priority"} {
		if v, present := fields[k]; present {
			t.Errorf("没传的 %s 不该出现（会清掉原内容）: %#v", k, v)
		}
	}
}

func TestJiraAssignIssue(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", true)

	if _, _, err := c.Call(context.Background(), rt, "jira_assign_issue", map[string]any{
		"issueKey": "ENG-123", "accountId": "630db2",
	}); err != nil {
		t.Fatalf("指派失败: %v", err)
	}
	if m.assignee["accountId"] != "630db2" {
		t.Errorf(`body 应当是 {"accountId":"630db2"}: %v`, m.assignee)
	}
}

// 五个新工具里三个是写操作，写开关关着时一个都不能发出去。
func TestJiraWriteToolsAllGated(t *testing.T) {
	m := newJiraMock(t, "you@example.com", "tok-abc-123456")
	rt, c := m.runtime(t, "tok-abc-123456", false)

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"jira_update_issue", map[string]any{"issueKey": "ENG-123", "summary": "x"}},
		{"jira_assign_issue", map[string]any{"issueKey": "ENG-123", "accountId": "630db2"}},
		{"jira_transition_issue", map[string]any{"issueKey": "ENG-123", "transitionId": "31"}},
	}
	for _, tc := range cases {
		if _, _, err := c.Call(context.Background(), rt, tc.tool, tc.args); err == nil ||
			!strings.Contains(err.Error(), "写开关") {
			t.Errorf("%s 应当被写开关拦下，实际: %v", tc.tool, err)
		}
	}
	if m.updated != nil || m.assignee != nil || m.transition != nil {
		t.Error("被拒绝的调用一个都不该发出请求")
	}
}
