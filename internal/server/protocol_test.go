package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// captureUpstream 返回一个记录出站请求体、固定回 SSE 流的 upstream.Client。
func captureUpstream(t *testing.T, captured *[]byte, sse string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			if captured != nil {
				*captured = body
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestAnthropicMessagesStream 端到端：/v1/messages 流式请求 → Anthropic 事件流，
// 且出站 body 已是 OpenAI 格式（system/messages 转换正确）。
func TestAnthropicMessagesStream(t *testing.T) {
	var outbound []byte
	up := captureUpstream(t, &outbound, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-test"})

	reqBody := `{"model":"cn:glm-5.2","max_tokens":128,"stream":true,"system":"sys prompt",
		"messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%s", ct)
	}
	got := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start", "event: content_block_delta",
		`"type":"text_delta"`, "event: content_block_stop", "event: message_delta", "event: message_stop",
		`"text":"你好"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("响应缺少 %q:\n%s", want, got)
		}
	}
	// 出站请求体：system 转 system 消息、realm 前缀已剥离、强制 stream。
	var sent map[string]any
	if err := json.Unmarshal(outbound, &sent); err != nil {
		t.Fatalf("出站 body 非法: %v", err)
	}
	if sent["model"] != "glm-5.2" {
		t.Errorf("出站 model=%v", sent["model"])
	}
	msgs := sent["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "sys prompt" {
		t.Errorf("出站 messages=%v", msgs)
	}
	if sent["stream"] != true {
		t.Errorf("出站必须强制 stream:true")
	}
}

// TestAnthropicMessagesNonStream 非流式 /v1/messages → Anthropic Message 对象。
func TestAnthropicMessagesNonStream(t *testing.T) {
	up := captureUpstream(t, nil, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`))
	req.Header.Set("Authorization", "Bearer whatever") // 未配主密钥：不鉴权
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("信封=%v", msg)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	content := msg["content"].([]any)
	if len(content) == 0 || content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "你好" {
		t.Errorf("content=%v", content)
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) {
		t.Errorf("usage=%v", usage)
	}
}

// TestAnthropicAuthErrorShape 鉴权失败按 Anthropic 错误形状返回（x-api-key 与 Bearer 都要能通过）。
func TestAnthropicAuthErrorShape(t *testing.T) {
	up := captureUpstream(t, nil, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-test"})

	// 无凭证 → 401 Anthropic 形状
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)))
	if rec.Code != 401 {
		t.Fatalf("code=%d", rec.Code)
	}
	var errBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody["type"] != "error" {
		t.Errorf("错误体=%v", errBody)
	}
	if e := errBody["error"].(map[string]any); e["type"] != "authentication_error" || e["message"] == "" {
		t.Errorf("error=%v", e)
	}

	// x-api-key 正确 → 放行
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("x-api-key", "sk-test")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("x-api-key 必须通过鉴权: code=%d body=%s", rec2.Code, rec2.Body)
	}

	// Authorization: Bearer 也放行（Anthropic SDK 的官方写法之一）
	req3 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`))
	req3.Header.Set("Authorization", "Bearer sk-test")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("Bearer 必须通过鉴权: code=%d", rec3.Code)
	}
}

// TestResponsesEndToEnd /v1/responses 流式（Codex CLI 形态）。
func TestResponsesEndToEnd(t *testing.T) {
	var outbound []byte
	up := captureUpstream(t, &outbound, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	reqBody := `{"model":"glm-5.2","stream":true,"instructions":"be brief",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	got := rec.Body.String()
	for _, want := range []string{
		"event: response.created", "event: response.in_progress",
		"event: response.output_item.added", "event: response.output_text.delta",
		`"delta":"你好"`, "event: response.completed", `"status":"completed"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("响应缺少 %q:\n%s", want, got)
		}
	}
	var sent map[string]any
	_ = json.Unmarshal(outbound, &sent)
	msgs := sent["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "be brief" {
		t.Errorf("instructions 未转 system: %v", msgs)
	}
}

// TestResponsesNonStream /v1/responses 非流式 → response 对象。
func TestResponsesNonStream(t *testing.T) {
	up := captureUpstream(t, nil, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"你好"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("信封=%v", resp)
	}
	items := resp["output"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["type"] != "message" {
		t.Fatalf("output=%v", items)
	}
	text := items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "你好" {
		t.Errorf("text=%v", text)
	}
}

// TestGeminiEndToEnd Gemini generateContent 非流式与 SSE 流式。
func TestGeminiEndToEnd(t *testing.T) {
	var outbound []byte
	up := captureUpstream(t, &outbound, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 非流式
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1beta/models/glm-5.2:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"你好"}]}]}`))
	req.Header.Set("x-goog-api-key", "any")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	cands := resp["candidates"].([]any)
	parts := cands[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "你好" {
		t.Errorf("parts=%v", parts)
	}
	if resp["usageMetadata"].(map[string]any)["totalTokenCount"] != float64(2) {
		t.Errorf("usageMetadata=%v", resp["usageMetadata"])
	}
	var sent map[string]any
	_ = json.Unmarshal(outbound, &sent)
	if sent["model"] != "glm-5.2" {
		t.Errorf("URL 模型名未注入: %v", sent["model"])
	}

	// 流式 alt=sse
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1beta/models/glm-5.2:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"你好"}]}]}`)))
	if rec2.Code != 200 {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%s", ct)
	}
	if !strings.Contains(rec2.Body.String(), `"finishReason":"STOP"`) {
		t.Errorf("流式缺少 finishReason: %s", rec2.Body.String())
	}

	// 流式默认（alt=json）→ JSON 数组
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1beta/models/glm-5.2:streamGenerateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"你好"}]}]}`)))
	if rec3.Code != 200 {
		t.Fatalf("code=%d body=%s", rec3.Code, rec3.Body)
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec3.Body.Bytes(), &arr); err != nil {
		t.Fatalf("JSON 数组非法: %v (%s)", err, rec3.Body.String())
	}
	if len(arr) == 0 {
		t.Error("JSON 数组为空")
	}

	// 不支持的方法 → 404
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, httptest.NewRequest("POST", "/v1beta/models/glm-5.2:embedContent", strings.NewReader(`{}`)))
	if rec4.Code != 404 {
		t.Errorf("未知方法 code=%d", rec4.Code)
	}
}

// TestProtoUpstreamErrorMapping 上游 400 → 各协议错误形状与状态码。
func TestProtoUpstreamErrorMapping(t *testing.T) {
	upErr := func() *upstream.Client {
		return &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":"11115","message":"prompt is too long"}`)),
				}, nil
			})},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		}
	}
	newH := func() *Handler {
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		return NewHandler(Config{Pool: p, Upstream: upErr()})
	}

	// Anthropic：400 + invalid_request/原文
	recA := httptest.NewRecorder()
	newH().ServeHTTP(recA, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)))
	if recA.Code != 400 {
		t.Errorf("anthropic code=%d body=%s", recA.Code, recA.Body)
	}
	if !strings.Contains(recA.Body.String(), "prompt is too long") {
		t.Errorf("上游原文必须透传: %s", recA.Body)
	}
	var ae map[string]any
	_ = json.Unmarshal(recA.Body.Bytes(), &ae)
	if ae["type"] != "error" {
		t.Errorf("anthropic 错误信封=%v", ae)
	}

	// Gemini：错误体带 google.rpc 状态名
	recG := httptest.NewRecorder()
	newH().ServeHTTP(recG, httptest.NewRequest("POST", "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`)))
	if recG.Code != 400 {
		t.Errorf("gemini code=%d body=%s", recG.Code, recG.Body)
	}
	var ge map[string]any
	_ = json.Unmarshal(recG.Body.Bytes(), &ge)
	if ge["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
		t.Errorf("gemini 错误体=%v", ge)
	}

	// Responses：status 原样
	recR := httptest.NewRecorder()
	newH().ServeHTTP(recR, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`)))
	if recR.Code != 400 {
		t.Errorf("responses code=%d", recR.Code)
	}
	if !strings.Contains(recR.Body.String(), `"error"`) {
		t.Errorf("responses 错误体=%s", recR.Body)
	}
}

// TestProtoEmptyUpstreamStream 上游 200 空流：网关兜底 error 帧必须转成各协议的
// 错误事件，客户端不能收到"假成功"的静默流。
func TestProtoEmptyUpstreamStream(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: captureUpstream(t, nil, "")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)))
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("Anthropic 空流应转 error 事件: %s", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":"x"}`)))
	if !strings.Contains(rec2.Body.String(), "event: response.failed") {
		t.Errorf("Responses 空流应转 response.failed: %s", rec2.Body.String())
	}

	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1beta/models/m:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`)))
	if !strings.Contains(rec3.Body.String(), `"error"`) {
		t.Errorf("Gemini 空流应转 error chunk: %s", rec3.Body.String())
	}
}

// TestProtoInvalidRequestShape 转换失败（非法入站请求）→ 各协议 400。
func TestProtoInvalidRequestShape(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: captureUpstream(t, nil, sseOK)})

	cases := []struct {
		path, body string
	}{
		{"/v1/messages", `{"model":"m","messages":[]}`},
		{"/v1/responses", `{"model":"m","input":[]}`},
		{"/v1beta/models/m:generateContent", `{"contents":[]}`},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, strings.NewReader(c.body)))
		if rec.Code != 400 {
			t.Errorf("%s code=%d body=%s", c.path, rec.Code, rec.Body)
		}
	}
}

// TestProtoBodyLimit 多协议入口同样受 server.max_body_mb 约束。
func TestProtoBodyLimit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: captureUpstream(t, nil, sseOK), MaxBodyMB: 1})

	pad := strings.Repeat("a", 2<<20)
	body := `{"model":"m","messages":[{"role":"user","content":"` + pad + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413", rec.Code)
	}
}

// TestCountTokensAnthropic /v1/messages/count_tokens 返回估算值。
func TestCountTokensAnthropic(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: captureUpstream(t, nil, sseOK)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 400)+`"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	n, _ := out["input_tokens"].(float64)
	if n < 50 || n > 200 {
		t.Errorf("估算 token=%v", n)
	}
}

// TestOpenAIPathUnaffected 既有 /v1/chat/completions 行为不因多协议入口改变。
func TestOpenAIPathUnaffected(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: captureUpstream(t, nil, sseOK), APIKey: "sk"})
	// chat/completions 不认 x-api-key（保持原语义）
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("x-api-key", "sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("chat/completions 不应接受 x-api-key: code=%d", rec.Code)
	}
}
