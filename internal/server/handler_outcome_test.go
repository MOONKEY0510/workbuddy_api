package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/calls"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newScriptedUpstream 按「上游收到的请求体」决定响应，供"先拒后收"的自愈重试类用例断言
// 第二次出站载荷（newFakeUpstream 只看 Authorization 头，看不到 body）。
func newScriptedUpstream(bodyFn func(body []byte) (status int, resp string, isStream bool)) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			status, resp, isStream := bodyFn(raw)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
}

// lastCall 取「调用记录」最近一条（List 最新在前）。协议入口（/v1/responses 等）在
// 管道 goroutine 里记账，可能比响应返回晚一拍，故带短轮询（同步入口首次即命中）。
func lastCall(t *testing.T, ring *calls.Ring) calls.Entry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if list := ring.List(); len(list) > 0 {
			return list[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("调用记录为空")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestClientAbortNotCountedAsAccountFailure 客户端主动断开（ctx 取消）不得喂连败降权：
// 此前它走传输层分支无条件 NoteFailures——Codex/CC-Switch 每断一次就给被选中的账号记
// 一笔"未知失败"，几次之后整池账号逐个被降权（面板表现为「连败降权永远解不开」）。
func TestClientAbortNotCountedAsAccountFailure(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connection reset by client")
		})},
		ChatBaseCN: "https://fake.example",
	}
	ring := calls.NewRing(10)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Calls: ring})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // 客户端已断开
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))

	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("客户端断开不得喂连败计数: consecutive_fails=%d", st.ConsecutiveFails)
	}
	if !st.DegradeUntil.IsZero() {
		t.Fatalf("客户端断开不得触发降权: %v", st.DegradeUntil)
	}
	e := lastCall(t, ring)
	if e.Kind != "client_abort" || e.OK {
		t.Fatalf("调用记录应标 client_abort 失败: kind=%q ok=%v status=%d", e.Kind, e.OK, e.Status)
	}
	if e.Status != statusClientClosedRequest || e.Err == "" {
		t.Errorf("client_abort 应带 499 状态与原因: %+v", e)
	}
}

// TestTransportErrorStillFeedsDegrade 反向守卫：真正的传输层失败（客户端还在）照旧喂连败
// 计数——上一条修复不得把「不知道原因的失败」全部放过。
func TestTransportErrorStillFeedsDegrade(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: i/o timeout")
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	st, _ := p.Status("u1")
	if st.ConsecutiveFails == 0 {
		t.Fatal("真实传输层失败应喂连败计数")
	}
}

// TestToolMismatchSelfHealsByStrippingToolHistory 上游 11148（对话工具记录损坏）：
// 网关剥离工具历史自愈重试一次 → 客户端拿到 200；账号不受任何惩罚（不冷却/不熔断/
// 不喂连败）——11148 是请求内容的问题，一条坏会话不该把整池账号拖下水。
func TestToolMismatchSelfHealsByStrippingToolHistory(t *testing.T) {
	mismatch := `{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry",` +
		`"displayTips":{"zh":"对话里的工具调用记录对不上，内容已损坏，直接重试无效。请新建任务重新开始。"}}`
	var bodies []string
	up := newScriptedUpstream(func(body []byte) (int, string, bool) {
		bodies = append(bodies, string(body))
		if strings.Contains(string(body), "tool_calls") || strings.Contains(string(body), `"tool"`) {
			return 400, mismatch, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"glm-5.2","stream":true,"messages":[` +
		`{"role":"user","content":"run"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("自愈重试后应成功: code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream calls=%d want 2 (首次 + 剥离工具历史重试)", len(bodies))
	}
	if !strings.Contains(bodies[0], "tool_calls") {
		t.Errorf("首次出站应带原始工具记录: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "tool_calls") || strings.Contains(bodies[1], `"role":"tool"`) {
		t.Errorf("重试出站不应含工具记录: %s", bodies[1])
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 {
		t.Errorf("11148 不得罚账号（请求内容问题）: %+v", st)
	}
}

// TestToolMismatchWithoutToolHistoryReturns400 请求本身没有工具记录时无从自愈：
// 直接 400 透传原文 + gateway_hint（新建会话），且不轮转、不罚号。
func TestToolMismatchWithoutToolHistoryReturns400(t *testing.T) {
	mismatch := `{"code":11148,"msg":"tool calls and tool results do not match"}`
	up := newScriptedUpstream(func(body []byte) (int, string, bool) { return 400, mismatch, false })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "tool_call_mismatch") || !strings.Contains(out, "11148") {
		t.Errorf("应透传上游原文 + 分类码: %s", out)
	}
	if !strings.Contains(out, "gateway_hint") {
		t.Errorf("应带 gateway_hint（新建会话指向）: %s", out)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 || st.ConsecutiveFails != 0 {
		t.Errorf("11148 不得罚账号: %+v", st)
	}
}

// TestToolMismatchRetriesOnlyOnce 自愈重试只做一次：剥离工具历史后仍被拒（坏会话对上游
// 而言已不可修）→ 直接 400，不再用同一 body 反复打上游；上游总调用数恒为 2。
func TestToolMismatchRetriesOnlyOnce(t *testing.T) {
	mismatch := `{"code":11148,"msg":"tool calls and tool results do not match"}`
	var n int
	up := newScriptedUpstream(func([]byte) (int, string, bool) {
		n++
		return 400, mismatch, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"user","content":"run"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if n != 2 {
		t.Fatalf("upstream calls=%d want 2（首次 + 一次自愈重试）", n)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("失败响应应带 gateway_hint: %s", rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ConsecutiveFails != 0 {
		t.Errorf("11148 不得罚账号: %+v", st)
	}
}

// TestResponsesCodexHistoryCanonicalized Codex 会话存档里的坏历史（工具调用没有对应结果、
// 参数只流了一半、同一 call id 跨轮复用）经 Responses→OpenAI 转换后，出站载荷必须是
// 规范形态——半截配对 / 残缺参数 / 重复 id 都是上游 11148（tool calls and tool results
// do not match，「内容已损坏，直接重试无效」）的触发形态。
func TestResponsesCodexHistoryCanonicalized(t *testing.T) {
	var outbound []byte
	up := captureUpstream(t, &outbound, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"kimi-k3","stream":true,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},` +
		// ① 工具调用没有结果（用户中途打断工具执行）
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]},` +
		// ② 参数残缺（上游断流留下的半截 arguments）+ 结果
		`{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"cmd\":\"ls"},` +
		`{"type":"function_call_output","call_id":"c2","output":"failed to parse"},` +
		// ③ 正常的一对，但 call id 与 ① 重复（模型按轮复用序号 id）
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	if err := json.Unmarshal(outbound, &sent); err != nil {
		t.Fatalf("出站 body 不是 JSON: %v (%s)", err, outbound)
	}
	callIDs := map[string]bool{}
	for _, m := range sent["messages"].([]any) {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		for _, tci := range asAnySliceServer(msg["tool_calls"]) {
			tc, _ := tci.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if args, _ := fn["arguments"].(string); !json.Valid([]byte(args)) {
				t.Errorf("残缺参数的调用被送出: %v", tc)
			}
			id, _ := tc["id"].(string)
			if callIDs[id] {
				t.Errorf("出站载荷出现重复 call id: %q", id)
			}
			callIDs[id] = true
		}
	}
	for _, m := range sent["messages"].([]any) {
		msg, _ := m.(map[string]any)
		if msg == nil || msg["role"] != "tool" {
			continue
		}
		id, _ := msg["tool_call_id"].(string)
		if !callIDs[id] {
			t.Errorf("孤儿 tool 结果被送出（id=%q 无对应调用）", id)
		}
	}
	shape := upstream.DiagnoseToolRecords(outbound)
	if !strings.Contains(shape, "bad_args=0") || !strings.Contains(shape, "unpaired_calls=0") ||
		!strings.Contains(shape, "orphan_results=0") || !strings.Contains(shape, "dup_ids=0") {
		t.Errorf("出站工具记录形状不规范: %s", shape)
	}
	// 良构的那一对必须保住（不能因为邻居坏就把工具上下文全丢）。
	if !strings.Contains(shape, "calls=1") || !strings.Contains(shape, "results=1") {
		t.Errorf("良构调用被误伤: %s", shape)
	}
}

// asAnySliceServer 断言 []any（nil 安全），测试内小工具。
func asAnySliceServer(v any) []any {
	s, _ := v.([]any)
	return s
}

// TestStreamWithoutDoneButWithUsageIsSuccess 上游漏发 [DONE] 但按规范给了 usage（正常收尾）：
// 记成功——否则每一条这样的正常响应都会变成一条"未知失败"（Codex 场景实测大量噪音）。
func TestStreamWithoutDoneButWithUsageIsSuccess(t *testing.T) {
	noDone := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, noDone, true })
	ring := calls.NewRing(10)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Calls:    ring,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	e := lastCall(t, ring)
	if !e.OK || e.Kind != "" {
		t.Fatalf("正常收尾（有 usage、无 [DONE]）应记成功: %+v", e)
	}
	if e.CompletionTokens != 1 {
		t.Errorf("usage 应被采纳: %+v", e)
	}
}

// TestStreamEndedWithoutDoneOrUsageMarkedTruncated 既没 [DONE] 也没 usage：上游把流掐在半路，
// 记 upstream_truncated（有原因、有分类）——而不是过去那条"未知 / HTTP 状态 —"的空失败行。
func TestStreamEndedWithoutDoneOrUsageMarkedTruncated(t *testing.T) {
	truncated := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, truncated, true })
	ring := calls.NewRing(10)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Calls:    ring,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	e := lastCall(t, ring)
	if e.OK || e.Kind != "upstream_truncated" || e.Err == "" || e.Status != http.StatusBadGateway {
		t.Fatalf("断流应记 upstream_truncated 且带原因: %+v", e)
	}
}

// TestResponsesTruncatedUpstreamEndsWithFailure 上游断流时 Codex 侧必须收到
// response.failed，而不是"完成但空输出"：后者正是「有输入、没有返回」——客户端拿不到
// 任何输出也看不到报错，只会静默失败/自动重试。终态事件（response.failed）之后不得再补
// response.completed。
func TestResponsesTruncatedUpstreamEndsWithFailure(t *testing.T) {
	truncated := "data: {\"choices\":[{\"delta\":{\"content\":\"半截\"}}]}\n\n" // 无 [DONE]、无收尾 usage
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, truncated, true })
	ring := calls.NewRing(10)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Calls:    ring,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"kimi-k3","stream":true,"input":"hi"}`)))

	got := rec.Body.String()
	if !strings.Contains(got, "event: response.failed") {
		t.Fatalf("断流应回 response.failed: %s", got)
	}
	if strings.Contains(got, "event: response.completed") {
		t.Errorf("response.failed 之后不得补 response.completed（客户端会读成完成但空输出）: %s", got)
	}
	e := lastCall(t, ring)
	if e.OK || e.Kind != "upstream_truncated" {
		t.Errorf("断流应在调用记录里可归因: %+v", e)
	}
}

// TestPromptOnlyUsageDoesNotMaskTruncation 上游只报了一份 prompt_tokens 就断流：
// 输入有值、输出为空，过去会被当成"拿到了 usage = 成功"而彻底无迹可寻；现在必须
// 判为断流（输入保留、失败可归因），并给客户端补一帧 error。
func TestPromptOnlyUsageDoesNotMaskTruncation(t *testing.T) {
	// 只有 prompt_tokens 的 usage 帧，随后 EOF（无正文、无 [DONE]）。
	promptOnly := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7}}\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, promptOnly, true })
	ring := calls.NewRing(10)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Calls:    ring,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	e := lastCall(t, ring)
	if e.OK || e.Kind != "upstream_truncated" {
		t.Fatalf("只报输入的断流应判失败并归因: %+v", e)
	}
	if e.PromptTokens != 7 {
		t.Errorf("输入用量应保留: %+v", e)
	}
	if e.CompletionTokens != 0 {
		t.Errorf("输出应为空: %+v", e)
	}
	if !strings.Contains(rec.Body.String(), "upstream_truncated") {
		t.Errorf("客户端应收到断流 error 帧（不再静默补 [DONE]）: %s", rec.Body)
	}
}

// TestStreamErrorFrameAttributed 流内 error 帧（200 开流后的失败）：
// 分类 + 原文入库——这是 Codex 长会话撞 11148 的流式形态，面板可直接读出原因。
func TestStreamErrorFrameAttributed(t *testing.T) {
	frame := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"code\":11148,\"message\":\"tool calls and tool results do not match\"}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, frame, true })
	ring := calls.NewRing(10)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Calls:    ring,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	e := lastCall(t, ring)
	if e.OK || e.Kind != "tool_mismatch" {
		t.Fatalf("流内 error 帧应按上游 code 归类: %+v", e)
	}
	if !strings.Contains(e.Err, "tool calls and tool results do not match") {
		t.Errorf("error 帧原文应入库: %q", e.Err)
	}
	if e.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", e.Status)
	}
}
