package httpchan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// 最近会话实测：一页固定 10 条，limit / pageSize / page / count / size 一律
	// 被忽略，只有 pageIndex 真的翻页。所以这里只认 pageIndex。
	mux.HandleFunc("/gateway/xtinterface/group/recentGroups", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		if n, ok := body["pageIndex"].(float64); ok && n >= 2 {
			write(w, map[string]any{
				"data": map[string]any{
					"count": 1, "more": false, "unreadTotal": 0,
					"list": []any{map[string]any{
						"groupId": "g-11", "groupName": "第二页的群", "groupType": 2,
					}},
				},
				"success": true, "errorCode": 0, "error": nil,
			})
			return
		}
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
	// 读历史实测：一次固定 10 条（limit / pageIndex 都被忽略），方向由 type 定，
	// 而且 type 不传等于 new —— 往更新的方向。所以「只给 msgId 想往回翻」
	// 在真实接口上是取不到更早记录的，这个 handler 照这个行为写。
	mux.HandleFunc("/gateway/xtinterface/message/msgList", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		if body["groupId"] == nil {
			write(w, map[string]any{"data": nil, "success": false, "errorCode": 1110101, "error": "缺少必填参数，groupId"})
			return
		}
		if body["type"] == "old" {
			if body["msgId"] == nil {
				write(w, map[string]any{"data": nil, "success": false, "errorCode": 1110101, "error": "缺少必填参数，msgId"})
				return
			}
			write(w, map[string]any{
				"data": map[string]any{
					"groupId": body["groupId"], "count": 1, "more": true,
					"list": []any{map[string]any{
						"msgId": "m-0", "fromOpenId": "oid-b", "msgType": "text",
						"content": "上周那张单子呢", "sendTime": "2026-10-22 09:14:02",
					}},
				},
				"success": true, "errorCode": 0, "error": nil,
			})
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

	// 审批列表。这个 handler 按真实接口实测的行为写：分页键只认
	// pageNumber / pageSize，别的键（包括官方 CLI 的 flag 名 limit）
	// 一律当没传、回服务端默认的 10 条 —— 这正是 pageNo 那版声明在
	// 线上的表现（HTTP 200、success:true、永远只有前 10 条，翻页毫无
	// 反应）。原来没有这个 handler，所以整组用例都没碰过这两个工具。
	//
	// 实测（张明星账号，www.yunzhijia.com）：
	//   不传分页          -> 10 条，total 22
	//   pageNumber=2      -> 另一批（更早的单子），说明页码生效
	//   pageSize=3        -> 3 条；pageSize=100 -> 22 条全拿到
	//   limit/size/rows/perPage/pageCount/maxCount -> 一律 10 条
	pageList := func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		totalRows := 23
		// 单号是模糊匹配，而且只有 serialNo 这个键认（serial_no /
		// serialNumber 实测被当成没传，照样回全量 —— 一个认不出的过滤键
		// 比报错更坏：模型会把「全部 323 条的第一页」当成命中结果）。
		serial, filtered := body["serialNo"].(string)
		if filtered && serial != "" {
			totalRows = 1
		}
		page, limit := 1, 10
		if n, ok := body["pageNumber"].(float64); ok && n >= 1 {
			page = int(n)
		}
		if n, ok := body["pageSize"].(float64); ok && n >= 1 && n <= 100 {
			limit = int(n)
		}
		rows := []any{}
		for i := (page - 1) * limit; i < page*limit && i < totalRows; i++ {
			row := map[string]any{
				"formDefId":  "def-1",
				"formInstId": fmt.Sprintf("inst-%d", i+1),
				"flowInstId": fmt.Sprintf("flow-%d", i+1),
				"title":      fmt.Sprintf("请款单 %d", i+1),
				"serialNo":   fmt.Sprintf("QKSQD-20261022-%03d", i+1),
			}
			if filtered && serial != "" {
				row["serialNo"] = serial
			}
			rows = append(rows, row)
		}
		write(w, map[string]any{
			"data":    map[string]any{"list": rows, "total": totalRows},
			"success": true, "errorCode": 0, "error": nil,
		})
	}
	mux.HandleFunc("/gateway/workflow/flow/todoPageList", pageList)
	mux.HandleFunc("/gateway/workflow/flow/myFlowPageList", pageList)
	mux.HandleFunc("/gateway/workflow/flow/donePageList", pageList)

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

// 审批列表的分页键必须是 pageNumber / limit，而且要以 JSON 数字下发。
//
// 这条用例是补上来的：原来 24 组用例里没有一组碰过审批列表，声明里写的
// pageNo / pageSize 这两个键在云之家那边根本不存在，于是调用照样成功、
// 照样只回前 10 条，翻页参数像是「失效」—— 真实情况是服务端把它们当成
// 没传过。能作证的只有「网关收到了什么」，不是「调用没报错」。
func TestYunzhijiaApprovalPaging(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	rt := m.runtime(t, false)

	for _, tool := range []string{"yunzhijia_approval_todo", "yunzhijia_approval_myflow", "yunzhijia_approval_done"} {
		// 不传任何参数也要带上声明里的默认值，否则服务端给 10 条。
		got, meta, err := c.Call(context.Background(), rt, tool, nil)
		if err != nil {
			t.Fatalf("%s 失败: %v", tool, err)
		}
		body := m.body()
		if body["pageNumber"] != float64(1) {
			t.Errorf("%s: pageNumber 要以数字 1 下发，实际 body=%#v", tool, body)
		}
		if body["pageSize"] != float64(20) {
			t.Errorf("%s: pageSize 要以数字 20 下发，实际 body=%#v", tool, body)
		}
		// 旧键和「看起来对」的 CLI flag 名留在 body 里，会让
		// 「到底哪个生效」看不清。
		for _, dead := range []string{"pageNo", "limit"} {
			if _, ok := body[dead]; ok {
				t.Errorf("%s: %s 是云之家不认的键，不该下发: %#v", tool, dead, body)
			}
		}
		if n := len(got.([]any)); n != 20 {
			t.Errorf("%s: 默认该拿到 20 条（服务端默认只给 10），实际 %d", tool, n)
		}
		if meta["total"] != float64(23) {
			t.Errorf("%s: meta 要带出 data.total，模型靠它判断还有没有下一页，实际 %#v", tool, meta)
		}

		// 第二页：拿到的是另一批，不是同一批。
		got, _, err = c.Call(context.Background(), rt, tool,
			map[string]any{"pageNumber": float64(2), "pageSize": float64(20)})
		if err != nil {
			t.Fatalf("%s 第二页失败: %v", tool, err)
		}
		rows := got.([]any)
		if len(rows) != 3 {
			t.Fatalf("%s: 23 条总量第二页该剩 3 条，实际 %d", tool, len(rows))
		}
		if first := rows[0].(map[string]any)["formInstId"]; first != "inst-21" {
			t.Errorf("%s: 第二页该从第 21 条开始，实际 %v", tool, first)
		}
	}
}

// 群消息的翻页：读更早的记录只能靠 type=old 加锚点，而 type 不传在真实
// 接口上等于 new（往更新的方向）—— 所以声明里必须有 type，而且默认值要是
// newest。没有它，模型拿 msgId 想往回翻，取回来的是更新的消息，看起来就是
// 「历史读不出来」。
//
// 实测（张明星账号的 SMT物料群，www.yunzhijia.com）：
//
//	不传 type            -> 16:16..16:26（更新的 9 条），more=false
//	type=old + 锚点      -> 10-29 10:57..10-30 15:11，more=true  <- 这才是往回翻
//	type=newest          -> 最新 10 条
//	limit=3 / limit=50 / pageIndex=2 -> 一律 10 条，参数无效
func TestYunzhijiaGroupHistoryPagesBackwards(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	var tool *Tool
	for i := range spec.Tools {
		if spec.Tools[i].Name == "yunzhijia_group_history" {
			tool = &spec.Tools[i]
		}
	}
	var typ *Param
	for i := range tool.Params {
		switch tool.Params[i].Name {
		case "type":
			typ = &tool.Params[i]
		case "limit", "pageIndex", "pageSize":
			t.Errorf("接口忽略 %s，声明里留着它等于给模型一个没有作用的旋钮", tool.Params[i].Name)
		}
	}
	if typ == nil {
		t.Fatal("读历史必须有 type 参数，否则只能往更新的方向翻")
	}
	if typ.Default != "newest" {
		t.Errorf("type 的默认值要是 newest（不传时服务端按 new 处理），实际 %q", typ.Default)
	}
	if !slices.Equal(typ.Enum, []string{"newest", "old", "new"}) {
		t.Errorf("type 的取值应是 newest/old/new，实际 %v", typ.Enum)
	}
	if !strings.Contains(tool.Description, "第一条") {
		t.Errorf("说明里要讲清往回翻的锚点取本页第一条（结果是从旧到新排的）：%s", tool.Description)
	}

	m := newYZJMock(t)
	c := &Client{}
	rt := m.runtime(t, false)

	// 不传 type 要带上声明的默认值，不能让服务端按 new 处理。
	if _, _, err := c.Call(context.Background(), rt, "yunzhijia_group_history",
		map[string]any{"groupId": "g-1"}); err != nil {
		t.Fatalf("取最新一页失败: %v", err)
	}
	if got := m.body()["type"]; got != "newest" {
		t.Errorf("不传 type 时要下发 newest，实际 body=%#v", m.body())
	}

	// type=old + 锚点才取到更早的消息。
	got, meta, err := c.Call(context.Background(), rt, "yunzhijia_group_history",
		map[string]any{"groupId": "g-1", "type": "old", "msgId": "m-1"})
	if err != nil {
		t.Fatalf("往回翻失败: %v", err)
	}
	row := got.([]any)[0].(map[string]any)
	if row["msgId"] != "m-0" {
		t.Errorf("type=old 该取到更早的那条，实际 %#v", row)
	}
	if meta["more"] != true {
		t.Errorf("meta 要带出 data.more，模型靠它判断还能不能继续往回翻：%#v", meta)
	}
}

// 最近会话只有 pageIndex 能翻页（limit / pageSize / page / count / size 实测
// 全部被忽略，一页固定 10 条）。留一个不起作用的 limit 比没有更糟：模型会以为
// 自己已经把会话全取回来了。
func TestYunzhijiaRecentGroupsPagesByPageIndex(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	for _, tl := range spec.Tools {
		if tl.Name != "yunzhijia_list_recent_groups" {
			continue
		}
		var names []string
		for _, p := range tl.Params {
			names = append(names, p.Name)
		}
		if !slices.Equal(names, []string{"pageIndex"}) {
			t.Errorf("最近会话的参数应只有 pageIndex，实际 %v", names)
		}
	}

	m := newYZJMock(t)
	c := &Client{}
	rt := m.runtime(t, false)
	if _, _, err := c.Call(context.Background(), rt, "yunzhijia_list_recent_groups", nil); err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if got := m.body()["pageIndex"]; got != float64(1) {
		t.Errorf("不传时要以数字 1 下发 pageIndex，实际 body=%#v", m.body())
	}
	got, _, err := c.Call(context.Background(), rt, "yunzhijia_list_recent_groups",
		map[string]any{"pageIndex": float64(2)})
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if id := got.([]any)[0].(map[string]any)["groupId"]; id != "g-11" {
		t.Errorf("第二页该是另一批会话，实际 %v", id)
	}
}

// 按单号找单据：键名只有 serialNo 认，而且三个列表都要有。
//
// 实测（张明星账号，我发起的 323 条）：
//
//	serialNo=QKSQD-20261022-005  -> total 1    完整单号
//	serialNo=QKSQD-2026          -> total 7    模糊匹配，记一截就行
//	serial_no / serialNumber     -> total 323  被当成没传
//	keyword=签卡 / title=签卡      -> total 63
//	status=FINISH                -> total 321
//
// 拼错的过滤键比报错更坏：接口照样回全量第一页，模型会把它当成命中结果
// 报给用户。
func TestYunzhijiaApprovalFindBySerialNo(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	lists := []string{"yunzhijia_approval_todo", "yunzhijia_approval_myflow", "yunzhijia_approval_done"}
	for _, want := range lists {
		var names []string
		for _, tl := range spec.Tools {
			if tl.Name != want {
				continue
			}
			for _, p := range tl.Params {
				names = append(names, p.Name)
			}
		}
		if names == nil {
			t.Fatalf("少了工具 %s", want)
		}
		for _, must := range []string{"serialNo", "keyword"} {
			if !slices.Contains(names, must) {
				t.Errorf("%s 要能按 %s 找单据，实际参数 %v", want, must, names)
			}
		}
	}

	m := newYZJMock(t)
	c := &Client{}
	rt := m.runtime(t, false)
	for _, tool := range lists {
		got, meta, err := c.Call(context.Background(), rt, tool,
			map[string]any{"serialNo": "QKSQD-20261022-005"})
		if err != nil {
			t.Fatalf("%s 按单号查失败: %v", tool, err)
		}
		if body := m.body(); body["serialNo"] != "QKSQD-20261022-005" {
			t.Errorf("%s: serialNo 要原样下发，实际 body=%#v", tool, body)
		}
		if n := len(got.([]any)); n != 1 {
			t.Errorf("%s: 按完整单号该只命中 1 条，实际 %d", tool, n)
		}
		if meta["total"] != float64(1) {
			t.Errorf("%s: 命中后 total 该是 1（不是全量），实际 %#v", tool, meta)
		}
	}
}

// 已办的 total 不加过滤时实测回 10000 这个整数 —— 是封顶值不是精确条数
// （第 900 页仍有数据，说明真实条数远不止一页，但也不是正好一万）。
// 说明里必须讲明，否则模型会告诉用户「你一共批了 10000 单」。
func TestYunzhijiaApprovalDoneTotalIsACap(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	for _, tl := range spec.Tools {
		if tl.Name != "yunzhijia_approval_done" {
			continue
		}
		for _, want := range []string{"10000", "封顶"} {
			if !strings.Contains(tl.Description, want) {
				t.Errorf("已办的说明要讲明 total 是封顶值，缺 %q：%s", want, tl.Description)
			}
		}
		// 单子走到哪一步、在谁手上，是「我批没批」之外最常被追问的两件事。
		for _, f := range []string{"currentActivityName", "currentApprovers"} {
			if !slices.Contains(tl.Result.Fields, f) {
				t.Errorf("已办的结果要保留 %s：%v", f, tl.Result.Fields)
			}
		}
	}
}

// 待办列表里混着外部系统接进来的单据，它们的 formDefId / formInstId 是
// 空串 —— 实测张明星账号 22 条待办里有 20 条是金蝶云星空的（webUrl 指向
// mkk3cloud.gotechcn.cn），四个 ID 全为空。detail / agree 对它们用不了，
// 所以列表的说明里必须写明这件事，否则模型会拿空 ID 去调，或者自己拼一个
// ID 去"同意"一张不知道是什么的单子。
func TestYunzhijiaApprovalTodoWarnsAboutExternalForms(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	var desc string
	var fields []string
	for _, tl := range spec.Tools {
		if tl.Name == "yunzhijia_approval_todo" {
			desc, fields = tl.Description, tl.Result.Fields
		}
	}
	for _, want := range []string{"formDefId", "为空", "webUrl"} {
		if !strings.Contains(desc, want) {
			t.Errorf("待办列表的说明里要讲清外部系统单据没有 ID、只能给 webUrl，缺 %q：%s", want, desc)
		}
	}
	// 判断「能不能 agree」靠的就是这几个 ID 在不在，裁掉任何一个，模型就
	// 没法分辨哪张单子能处理。webUrl 是那类单子唯一的出路，也必须留。
	for _, must := range []string{"formDefId", "formInstId", "flowInstId", "webUrl"} {
		if !slices.Contains(fields, must) {
			t.Errorf("result.fields 不能裁掉 %s：%v", must, fields)
		}
	}
	// mobileUrl 和 webUrl 实测是同一个地址，留两份只是把一页的体积翻倍。
	if slices.Contains(fields, "mobileUrl") {
		t.Errorf("mobileUrl 和 webUrl 重复，不该带出来：%v", fields)
	}
}

// 结果裁剪的上限不能低于接口允许的每页条数，否则模型要了 100 条、
// 接口也给了 100 条，却被宿主静默砍掉一半 —— 那是同一个「只能拿到前 N 个」
// 的坑换了个地方出现。
func TestYunzhijiaApprovalResultLimitMatchesAPICap(t *testing.T) {
	spec, _ := ByID("yunzhijia")
	for _, tl := range spec.Tools {
		if !strings.HasPrefix(tl.Name, "yunzhijia_approval_") || tl.Name == "yunzhijia_approval_detail" || tl.Name == "yunzhijia_approval_agree" {
			continue
		}
		if tl.Result.Limit < 100 {
			t.Errorf("%s 的 result.limit 不能小于接口上限 100，实际 %#v", tl.Name, tl.Result)
		}
	}
}

// 读历史消息要把 param 那块富载荷裁掉，只留能读的几个字段。
func TestYunzhijiaHistoryTrimsRichPayload(t *testing.T) {
	m := newYZJMock(t)
	c := &Client{}
	got, _, err := c.Call(context.Background(), m.runtime(t, false), "yunzhijia_group_history",
		map[string]any{"groupId": "g-1"})
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
		map[string]any{"type": "newest"})
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
