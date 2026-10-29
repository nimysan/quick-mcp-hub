package httpchan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 仿真的云之家网关。这组用例跑的是目录里那份真声明（ByID("yunzhijia")），
// 所以它同时在验 yunzhijia.json。
//
// 响应形状是对 www.yunzhijia.com 实测抄回来的，而实测里有三件事是照文档
// 或凭直觉都写不对的，这组用例就是钉住它们：
//
//   - 一个 GET 都没有：GET 直接回「认证失败」，所以读操作也得 POST，
//     必须走 postReads 那个「方法不能作证」的逃生口
//   - errorCode 完全不一致：成功时见过 0、100、200，所以绝不能用
//     errorCode != 0 判失败 —— 那会把每一次成功都报成失败
//   - 「检索不到用户」是 success:true 配一个 error 文案：所以也不能用
//     「error 字段非空」判失败 —— 那会把一次空结果报成错误
//     可靠的只有 success 这一个字段
type yzjMock struct {
	srv *httptest.Server

	mu       sync.Mutex
	gotAuth  string
	calls    []string
	lastBody map[string]any
}

func newYZJMock(t *testing.T) *yzjMock {
	t.Helper()
	m := &yzjMock{}
	mux := http.NewServeMux()

	record := func(r *http.Request) map[string]any {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var body map[string]any
		json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.gotAuth = r.Header.Get("Authorization")
		m.calls = append(m.calls, r.Method+" "+r.URL.Path)
		m.lastBody = body
		m.mu.Unlock()
		return body
	}
	// 云之家每个服务的信封都不一样，所以这里不做统一包装，各端点照实回。
	write := func(w http.ResponseWriter, payload map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}

	mux.HandleFunc("/gateway/openimport/contacts/getPersonInfos", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		// 注意：data 是数组（批量接口），而且成功时 errorCode 是 100。
		write(w, map[string]any{
			"data": []any{map[string]any{
				"openId": "oid-self", "name": "卢红霞", "department": "", "jobTitle": "", "isAdmin": 1,
			}},
			"success": true, "errorCode": 100, "error": "",
		})
	})
	mux.HandleFunc("/gateway/xtinterface/group/recentGroups", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		write(w, map[string]any{
			"data": map[string]any{
				"count": 2, "more": false, "unreadTotal": 0,
				"list": []any{map[string]any{
					"groupId": "g-1", "groupName": "财务组", "groupType": 2, "unreadCount": 3,
					"lastMsgSendTime": "2026-10-29 16:05:37",
					"lastMsg": map[string]any{
						"content": "报销单发你了", "fromOpenId": "oid-a", "msgType": "text",
					},
					"headerUrl": "https://example.invalid/noise.png",
				}},
			},
			"success": true, "errorCode": 0, "error": nil,
		})
	})
	mux.HandleFunc("/gateway/xtinterface/message/msgList", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		if body["groupId"] == nil {
			write(w, map[string]any{"data": nil, "success": false, "errorCode": 1110101, "error": "缺少必填参数，groupId"})
			return
		}
		write(w, map[string]any{
			"data": map[string]any{
				"groupId": body["groupId"], "count": 1, "more": false,
				"list": []any{map[string]any{
					"msgId": "m-1", "fromOpenId": "oid-a", "msgType": "text",
					"content": "报销单发你了", "sendTime": "2026-10-29 16:05:37",
					// param 是图文卡片的大块富载荷，声明里刻意不取它。
					"param": map[string]any{"list": []any{map[string]any{"title": "噪音"}}},
				}},
			},
			"success": true, "errorCode": 0, "error": nil,
		})
	})
	mux.HandleFunc("/gateway/openorg/contacts/searchUser", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		// 搜不到人：success 仍是 true，但 error 有文案、errorCode 非 0。
		write(w, map[string]any{
			"success": true, "errorCode": 2012, "error": "检索不到用户",
			"data": map[string]any{"list": []any{}, "numFound": 0, "hasMore": false, "recommend": false},
		})
	})
	mux.HandleFunc("/gateway/xtinterface/group/searchGroupOpen", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		// 这个端点的载荷键是 result，不是 data。
		write(w, map[string]any{
			"success": true,
			"result": map[string]any{
				"numFound": 1, "hasMore": false,
				"list": []any{map[string]any{"groupId": "g-1", "groupName": "财务组"}},
			},
		})
	})
	mux.HandleFunc("/gateway/xtinterface/message/send", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		write(w, map[string]any{
			"data": map[string]any{
				"groupId": "g-1", "msgId": "m-new", "sendTime": "2026-10-29 16:05:37",
			},
			"success": true, "errorCode": 0, "error": nil,
		})
	})

	mux.HandleFunc("/gateway/workflow/flow/viewFormInst", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		if body["formDefId"] == nil || body["formInstId"] == nil {
			write(w, map[string]any{"data": nil, "success": false, "errorCode": 1201001, "error": "formDefId不能为空"})
			return
		}
		write(w, map[string]any{
			"data": map[string]any{
				"formDefId": body["formDefId"], "formInstId": body["formInstId"],
				"formCodeId": "code-1", "flowInstId": "flow-1",
				"formStatus": "DOING", "flowType": "solid",
				"webLink": "https://www.yunzhijia.com/cloudflow/x",
				"formVo": map[string]any{
					"widgetMap": map[string]any{"金额": "1280.00", "事由": "差旅"},
					// widgetVos 是控件模板定义，不是填写的内容，刻意不取。
					"widgetVos": []any{map[string]any{"noise": true}},
				},
			},
			"success": true, "errorCode": 200, "error": nil,
		})
	})
	mux.HandleFunc("/gateway/workflow/flow/agree", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		// 真实接口要的是两个嵌套对象：flow 和 form，缺一层就报 "flow不能为空"
		// / "form不能为空" / "form.formCodeId不能为空"。
		flow, _ := body["flow"].(map[string]any)
		form, _ := body["form"].(map[string]any)
		switch {
		case flow == nil || flow["flowInstId"] == nil:
			write(w, map[string]any{"data": nil, "success": false, "errorCode": 1201001, "error": "flow不能为空"})
		case form == nil:
			write(w, map[string]any{"data": nil, "success": false, "errorCode": 1201001, "error": "form不能为空"})
		case form["formCodeId"] == nil:
			write(w, map[string]any{"data": nil, "success": false, "errorCode": 1201001, "error": "form.formCodeId不能为空"})
		default:
			write(w, map[string]any{"data": nil, "success": true, "errorCode": 200, "error": nil})
		}
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *yzjMock) runtime(t *testing.T, allowWrite bool) Runtime {
	t.Helper()
	spec, found := ByID("yunzhijia")
	if !found {
		t.Fatal("目录里没有 yunzhijia 声明")
	}
	spec.BaseURL = m.srv.URL
	settings := map[string]string{}
	if allowWrite {
		settings[AllowWriteField] = "true"
	}
	return Runtime{
		Spec:     spec,
		Account:  "卢红霞",
		Settings: settings,
		Secret:   func() (string, error) { return "at-1", nil },
	}
}

func (m *yzjMock) body() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBody
}

// 声明本身的形状：这个渠道没有 GET，所以 postReads 必须声明出来；
// 而 failWhen 只能挂在 success 上。
func TestYunzhijiaSpecShape(t *testing.T) {
	spec, found := ByID("yunzhijia")
	if !found {
		t.Fatal("目录里没有 yunzhijia 声明")
	}
	if !spec.PostReads {
		t.Error("读操作全是 POST，必须声明 postReads")
	}
	if spec.FailWhen == nil || spec.FailWhen.Path != "success" {
		t.Fatalf("failWhen 必须挂在 success 上（errorCode 成功时会是 0/100/200），实际 %+v", spec.FailWhen)
	}
	if spec.FailWhen.Equals != false {
		t.Errorf("failWhen.equals 应是布尔 false，实际 %#v", spec.FailWhen.Equals)
	}
	if spec.OAuth == nil || spec.OAuth.Dialect != "yunzhijia" {
		t.Fatalf("应声明云之家方言，实际 %+v", spec.OAuth)
	}
	if spec.OAuth.VerificationURI == "" {
		t.Error("云之家出码不返回验证地址，必须在声明里给出")
	}
	if spec.OAuth.ClientIDField != "" {
		t.Error("不该让用户填 Client ID —— 借用的是官方 CLI 那个公开的应用身份")
	}
	// 借用别人的应用身份这件事必须留在页面上看得见的文案里：用户在授权页
	// 上看到的申请方是「云之家 CLI」而不是本程序，这是他做安全决定时的
	// 事实前提。文案被删掉的时候要有用例拦住。
	if !strings.Contains(spec.OAuth.Help, "云之家 CLI") {
		t.Error("oauth.help 必须说明授权页显示的申请方是「云之家 CLI」")
	}
	// 发消息只能发纯文本：msgType 钉死在 bodyConst 里，不作为参数暴露，
	// 否则模型能改成 richText，而 richText 的正文会被当结构化元素解析
	// （@全员就是这么发出去的）。
	for _, name := range []string{"yunzhijia_send_group_msg", "yunzhijia_send_private_msg"} {
		tool, ok := spec.Tool(name)
		if !ok {
			t.Fatalf("少了工具 %s", name)
		}
		if tool.BodyConst["msgType"] != "text" {
			t.Errorf("%s 应把 msgType 钉死成 text，实际 %v", name, tool.BodyConst)
		}
		for _, p := range tool.Params {
			if p.Name == "msgType" {
				t.Errorf("%s 不该把 msgType 开放成参数", name)
			}
		}
		if tool.ReadOnly {
			t.Errorf("%s 是写操作，不能标 readOnly", name)
		}
	}
}

// 读操作走 POST、带 Bearer，而且 data 是数组的身份接口也要能裁出字段。
func TestYunzhijiaReadsArePostWithBearer(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	rt := m.runtime(t, false)

	got, _, err := c.Call(context.Background(), rt, "yunzhijia_whoami", nil)
	if err != nil {
		t.Fatalf("whoami 失败: %v", err)
	}
	m.mu.Lock()
	auth, calls := m.gotAuth, append([]string(nil), m.calls...)
	m.mu.Unlock()
	if auth != "Bearer at-1" {
		t.Errorf("Authorization = %q, 想要 Bearer at-1", auth)
	}
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "POST ") {
		t.Errorf("读操作应是 POST，实际 %v", calls)
	}
	list, ok := got.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("身份接口返回数组，应原样裁出一条，实际 %#v", got)
	}
	rec := list[0].(map[string]any)
	if rec["name"] != "卢红霞" || rec["openId"] != "oid-self" {
		t.Errorf("身份字段裁错了: %#v", rec)
	}
}

// 列表的载荷路径各不相同：recentGroups 在 data.list，searchGroupOpen 在
// result.list。写错一个，模型拿到的是「响应里没有 X」或者一个空结果。
func TestYunzhijiaResultPaths(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	rt := m.runtime(t, false)

	got, meta, err := c.Call(context.Background(), rt, "yunzhijia_list_recent_groups", nil)
	if err != nil {
		t.Fatalf("最近会话失败: %v", err)
	}
	rows := got.([]any)
	if len(rows) != 1 {
		t.Fatalf("应有 1 条会话，实际 %#v", got)
	}
	row := rows[0].(map[string]any)
	if row["groupId"] != "g-1" {
		t.Errorf("groupId 裁错了: %#v", row)
	}
	// 嵌套字段要取到，而 headerUrl 这类噪音不该带出来。
	if row["lastMsg.content"] != "报销单发你了" {
		t.Errorf("lastMsg.content 没取到: %#v", row)
	}
	if _, noisy := row["headerUrl"]; noisy {
		t.Errorf("headerUrl 是噪音，不该带出来: %#v", row)
	}
	if meta["count"] != float64(2) {
		t.Errorf("meta 应带出 data.count，实际 %#v", meta)
	}

	got, meta, err = c.Call(context.Background(), rt, "yunzhijia_search_group", map[string]any{"keyword": "财务"})
	if err != nil {
		t.Fatalf("搜群失败（载荷键是 result 不是 data）: %v", err)
	}
	if len(got.([]any)) != 1 {
		t.Fatalf("搜群应有 1 条，实际 %#v", got)
	}
	if meta["numFound"] != float64(1) {
		t.Errorf("meta 应带出 result.numFound，实际 %#v", meta)
	}
}

// 读历史消息要把 param 那块富载荷裁掉，只留能读的几个字段。
func TestYunzhijiaHistoryTrimsRichPayload(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	got, _, err := c.Call(context.Background(), m.runtime(t, false), "yunzhijia_group_history",
		map[string]any{"groupId": "g-1", "limit": 20})
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	msg := got.([]any)[0].(map[string]any)
	if msg["content"] != "报销单发你了" || msg["msgId"] != "m-1" {
		t.Errorf("消息字段裁错了: %#v", msg)
	}
	if _, kept := msg["param"]; kept {
		t.Errorf("param 是大块富载荷，不该带给模型: %#v", msg)
	}
	if m.body()["groupId"] != "g-1" {
		t.Errorf("groupId 没进 body: %v", m.body())
	}
}

// 这条是整份声明里最容易写错的一处：「检索不到用户」是 success:true 配
// 一个 error 文案和非 0 的 errorCode。把它判成失败，一次正常的空结果就
// 会变成报错；而把 errorCode != 0 判成失败，每一次成功都会变成报错。
func TestYunzhijiaEmptySearchIsNotAFailure(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	got, meta, err := c.Call(context.Background(), m.runtime(t, false),
		"yunzhijia_search_user", map[string]any{"keyword": "不存在的人"})
	if err != nil {
		t.Fatalf("搜不到人是空结果，不是失败: %v", err)
	}
	if rows, ok := got.([]any); !ok || len(rows) != 0 {
		t.Errorf("应是空列表，实际 %#v", got)
	}
	if meta["numFound"] != float64(0) {
		t.Errorf("meta 应带出 numFound=0，实际 %#v", meta)
	}
}

// 真正的业务失败（success:false）要报出来，并且带上服务端给的原因。
func TestYunzhijiaBusinessFailureIsReported(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	// 不传 groupId，让仿真网关回 success:false。
	_, _, err := c.Call(context.Background(), m.runtime(t, false), "yunzhijia_group_history",
		map[string]any{"limit": 5})
	if err == nil {
		t.Fatal("缺必填参数应该失败")
	}
	// 宿主侧先拦下来也算对（而且更好）：要么报缺参数，要么把服务端的原因带回。
	if !strings.Contains(err.Error(), "groupId") {
		t.Errorf("报错应点名 groupId，实际 %v", err)
	}
}

// 发消息必须被写开关挡住，而且 msgType 由声明钉死。
func TestYunzhijiaSendNeedsWriteSwitch(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}

	_, _, err := c.Call(context.Background(), m.runtime(t, false), "yunzhijia_send_group_msg",
		map[string]any{"groupId": "g-1", "content": "你好"})
	if err == nil {
		t.Fatal("写开关没开时不该发出去")
	}
	if !strings.Contains(err.Error(), "写开关") {
		t.Errorf("报错应说明是写开关，实际 %v", err)
	}
	m.mu.Lock()
	calls := len(m.calls)
	m.mu.Unlock()
	if calls != 0 {
		t.Errorf("被开关挡住时不该发出任何请求，实际发了 %d 次", calls)
	}

	got, _, err := c.Call(context.Background(), m.runtime(t, true), "yunzhijia_send_group_msg",
		map[string]any{"groupId": "g-1", "content": "你好"})
	if err != nil {
		t.Fatalf("开了写开关应能发: %v", err)
	}
	if m.body()["msgType"] != "text" {
		t.Errorf("msgType 应由 bodyConst 钉成 text，实际 %v", m.body())
	}
	if m.body()["content"] != "你好" {
		t.Errorf("正文没带上: %v", m.body())
	}
	res := got.(map[string]any)
	if res["msgId"] != "m-new" {
		t.Errorf("发送结果应带 msgId，实际 %#v", res)
	}
}

// 看单据详情：把填写的内容（formVo.widgetMap）取出来，把控件模板定义
// （widgetVos）那类噪音留下。同意一张单子之前要靠它给用户复述。
func TestYunzhijiaApprovalDetail(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	got, _, err := c.Call(context.Background(), m.runtime(t, false), "yunzhijia_approval_detail",
		map[string]any{"formDefId": "def-1", "formInstId": "inst-1"})
	if err != nil {
		t.Fatalf("看详情失败: %v", err)
	}
	d := got.(map[string]any)
	// formCodeId 必须带出来：同意那一步要它，而待办列表里可能没有。
	if d["formCodeId"] != "code-1" {
		t.Errorf("formCodeId 没取到（同意时要用）: %#v", d)
	}
	if d["formStatus"] != "DOING" {
		t.Errorf("formStatus 没取到: %#v", d)
	}
	wm, ok := d["formVo.widgetMap"].(map[string]any)
	if !ok || wm["金额"] != "1280.00" {
		t.Errorf("填写的内容应从 formVo.widgetMap 取出来，实际 %#v", d)
	}
	if _, noisy := d["formVo.widgetVos"]; noisy {
		t.Errorf("控件模板定义是噪音，不该带出来: %#v", d)
	}
	if tool, _ := m.runtime(t, false).Spec.Tool("yunzhijia_approval_detail"); !tool.ReadOnly {
		t.Error("看详情是只读的")
	}
}

// 同意审批是写操作：写开关关着时一个请求都不该发出去。
// 这条比发消息更要紧 —— 它推进的是真实公司流程，不可逆。
func TestYunzhijiaApprovalAgreeNeedsWriteSwitch(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	args := map[string]any{
		"formDefId": "def-1", "formInstId": "inst-1",
		"formCodeId": "code-1", "flowInstId": "flow-1",
	}

	_, _, err := c.Call(context.Background(), m.runtime(t, false), "yunzhijia_approval_agree", args)
	if err == nil {
		t.Fatal("写开关没开时不该同意任何单据")
	}
	if !strings.Contains(err.Error(), "写开关") {
		t.Errorf("报错应说明是写开关，实际 %v", err)
	}
	m.mu.Lock()
	calls := len(m.calls)
	m.mu.Unlock()
	if calls != 0 {
		t.Fatalf("被开关挡住时不该发出任何请求，实际发了 %d 次", calls)
	}
}

// 四个 ID 要落到两个嵌套对象里：flow.flowInstId 和 form.{formDefId,
// formInstId, formCodeId}。平铺发过去真实接口只回「flow不能为空」。
func TestYunzhijiaApprovalAgreeNestsBody(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	_, _, err := c.Call(context.Background(), m.runtime(t, true), "yunzhijia_approval_agree",
		map[string]any{
			"formDefId": "def-1", "formInstId": "inst-1",
			"formCodeId": "code-1", "flowInstId": "flow-1",
		})
	if err != nil {
		t.Fatalf("同意失败: %v", err)
	}
	body := m.body()
	flow, ok := body["flow"].(map[string]any)
	if !ok || flow["flowInstId"] != "flow-1" {
		t.Errorf("flowInstId 应落在 flow 对象里，实际 body=%#v", body)
	}
	form, ok := body["form"].(map[string]any)
	if !ok {
		t.Fatalf("form 对象没拼出来，实际 body=%#v", body)
	}
	for k, want := range map[string]string{
		"formDefId": "def-1", "formInstId": "inst-1", "formCodeId": "code-1",
	} {
		if form[k] != want {
			t.Errorf("form.%s = %v, 想要 %s（body=%#v）", k, form[k], want, body)
		}
	}
	// 平铺的键不该同时出现 —— 真实接口只认嵌套那份，留着平铺的只会让
	// 「到底发出去的是什么」看不清。
	for _, k := range []string{"formDefId", "formInstId", "formCodeId", "flowInstId"} {
		if _, flat := body[k]; flat {
			t.Errorf("%s 不该平铺在顶层: %#v", k, body)
		}
	}
}

// 缺任何一个 ID 都要在宿主侧拦下来并点名缺哪个：对一个不可逆的操作，
// 「参数错误」这种笼统报错等于让模型去猜。
func TestYunzhijiaApprovalAgreeRequiresAllIDs(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	full := map[string]any{
		"formDefId": "def-1", "formInstId": "inst-1",
		"formCodeId": "code-1", "flowInstId": "flow-1",
	}
	for _, missing := range []string{"formDefId", "formInstId", "formCodeId", "flowInstId"} {
		args := map[string]any{}
		for k, v := range full {
			if k != missing {
				args[k] = v
			}
		}
		_, _, err := c.Call(context.Background(), m.runtime(t, true), "yunzhijia_approval_agree", args)
		if err == nil {
			t.Errorf("缺 %s 时不该成功", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("缺 %s 的报错应点名它，实际 %v", missing, err)
		}
	}
}

// 云之家只开放了「同意」：不能有拒绝/退回/撤回/转交的工具，否则就是
// 声明了一个打不通的接口 —— 模型会调它、失败、然后编一个理由。
func TestYunzhijiaHasNoRejectTool(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	for _, tool := range spec.Tools {
		for _, banned := range []string{"reject", "rollback", "cancel", "transfer"} {
			if strings.Contains(tool.Name, banned) {
				t.Errorf("云之家没有开放 %s，不该声明工具 %s", banned, tool.Name)
			}
		}
	}
	// 同意这条要在描述里把这个不对称讲清楚，否则模型会以为能拒绝。
	tool, ok := spec.Tool("yunzhijia_approval_agree")
	if !ok {
		t.Fatal("少了同意审批的工具")
	}
	if !strings.Contains(tool.Description, "拒绝") {
		t.Error("同意工具的描述要说明拒绝/退回没有对应工具，该把链接给用户")
	}
	if tool.ReadOnly {
		t.Error("同意审批绝不能标 readOnly —— 那会让它绕过写开关并被免确认直接调")
	}
}
