package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesRequestToOpenAI(t *testing.T) {
	body := []byte(`{
	  "model": "gpt-5-codex",
	  "instructions": "be brief",
	  "max_output_tokens": 2048,
	  "stream": true,
	  "reasoning": {"effort": "medium"},
	  "tools": [{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
	  "tool_choice": {"type":"function","name":"shell"},
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"执行命令"},{"type":"input_image","image_url":"data:image/png;base64,BBB"}]},
	    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"先看目录"}]},
	    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"file.txt"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"然后呢"}]}
	  ]
	}`)
	out, stream, err := ResponsesToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Error("stream 必须保留")
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req["model"] != "gpt-5-codex" || req["max_tokens"] != float64(2048) || req["reasoning_effort"] != "medium" {
		t.Errorf("顶层字段=%v", req)
	}
	msgs := req["messages"].([]any)
	// system + user(富) + assistant(reasoning+tool_call) + tool + user
	if len(msgs) != 5 {
		t.Fatalf("messages=%d want 5: %s", len(msgs), string(out))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("instructions 应转 system: %v", msgs[0])
	}
	userParts := msgs[1].(map[string]any)["content"].([]any)
	if len(userParts) != 2 || userParts[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("user 富内容=%v", userParts)
	}
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "先看目录" {
		t.Errorf("reasoning 应并入 assistant: %v", asst)
	}
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 1 || tcs[0].(map[string]any)["id"] != "call_1" {
		t.Errorf("tool_calls=%v", tcs)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "file.txt" {
		t.Errorf("tool 消息=%v", toolMsg)
	}
	if msgs[4].(map[string]any)["content"] != "然后呢" {
		t.Errorf("后继 user=%v", msgs[4])
	}
	if req["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "shell" {
		t.Errorf("tool_choice=%v", req["tool_choice"])
	}
}

func TestResponsesRequestStringInputAndJSONSchema(t *testing.T) {
	out, stream, err := ResponsesToOpenAI([]byte(`{"model":"m","input":"你好","text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Error("默认非流式")
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	msgs := req["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "你好" {
		t.Errorf("messages=%v", msgs)
	}
	rf := req["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["name"] != "out" {
		t.Errorf("response_format=%v", rf)
	}
	if _, _, err := ResponsesToOpenAI([]byte(`{"model":"m","input":[]}`)); err == nil {
		t.Error("空 input 必须报错")
	}
}

func TestResponsesResponse(t *testing.T) {
	resp := map[string]any{
		"id": "chatcmpl-zzz", "model": "glm-5.2", "created": float64(1700000000),
		"choices": []any{map[string]any{
			"index": float64(0),
			"message": map[string]any{
				"role": "assistant", "content": "结果",
				"tool_calls": []any{map[string]any{
					"id": "call_9", "type": "function",
					"function": map[string]any{"name": "shell", "arguments": `{"cmd":"pwd"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(4), "total_tokens": float64(7),
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(2)}},
	}
	out := ResponsesResponse(resp, "")
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("信封=%v", out)
	}
	if out["id"] != "resp_zzz" {
		t.Errorf("id=%v", out["id"])
	}
	items := out["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output=%v", items)
	}
	msg := items[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("message item=%v", msg)
	}
	if msg["content"].([]any)[0].(map[string]any)["text"] != "结果" {
		t.Errorf("content=%v", msg["content"])
	}
	fc := items[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" || fc["name"] != "shell" {
		t.Errorf("function_call item=%v", fc)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 3 || usage["output_tokens"] != 4 || usage["total_tokens"] != 7 {
		t.Errorf("usage=%v", usage)
	}
	if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 2 {
		t.Errorf("reasoning tokens 缺失: %v", usage)
	}
}

func TestResponsesStreamTextAndFunctionCall(t *testing.T) {
	var sb strings.Builder
	st := NewResponsesStream(&sb, nil, "gpt-5-codex")
	frames := []map[string]any{
		{"id": "chatcmpl-1", "model": "gpt-5-codex", "choices": []any{map[string]any{
			"index": float64(0), "delta": map[string]any{"role": "assistant", "reasoning_content": "思考"}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "你好"}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": float64(0), "id": "call_5", "type": "function",
				"function": map[string]any{"name": "shell", "arguments": `{"cmd"`}}}}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": float64(0), "function": map[string]any{"arguments": `:"ls"}`}}}}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(6), "total_tokens": float64(11)}},
	}
	for _, f := range frames {
		if err := st.Frame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Finish(); err != nil {
		t.Fatal(err)
	}
	events := parseSSEEvents(t, sb.String())
	names := eventNames(events)
	if names[0] != "response.created" || names[1] != "response.in_progress" {
		t.Fatalf("起始事件=%v", names)
	}
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("收尾=%v", names)
	}
	// sequence_number 必须严格递增
	for i, e := range events {
		if int(e.data["sequence_number"].(float64)) != i {
			t.Fatalf("sequence_number 不连续 at %d: %v", i, e.data["sequence_number"])
		}
	}
	var fcDone map[string]any
	for _, e := range events {
		if e.event == "response.output_item.done" {
			item := e.data["item"].(map[string]any)
			if item["type"] == "function_call" {
				fcDone = item
			}
		}
	}
	if fcDone == nil || fcDone["call_id"] != "call_5" || fcDone["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("function_call item 不完整: %v", fcDone)
	}
	completed := events[len(events)-1].data["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Errorf("completed 状态=%v", completed["status"])
	}
	items := completed["output"].([]any)
	if len(items) != 3 { // reasoning + message + function_call
		t.Fatalf("output 条目=%v", items)
	}
	if items[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("首条目=%v", items[0])
	}
	if items[1].(map[string]any)["type"] != "message" {
		t.Errorf("次条目=%v", items[1])
	}
	// 事件经 SSE 文本解析 → JSON number 为 float64
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(6) || usage["total_tokens"] != float64(11) {
		t.Errorf("usage=%v", usage)
	}
}

func TestResponsesStreamIncompleteOnLength(t *testing.T) {
	var sb strings.Builder
	st := NewResponsesStream(&sb, nil, "m")
	_ = st.Frame(map[string]any{"choices": []any{map[string]any{
		"index": float64(0), "delta": map[string]any{"content": "截断"}, "finish_reason": "length"}}})
	_ = st.Finish()
	events := parseSSEEvents(t, sb.String())
	last := events[len(events)-1]
	if last.event != "response.completed" {
		t.Fatalf("收尾=%v", last.event)
	}
	resp := last.data["response"].(map[string]any)
	if resp["status"] != "incomplete" {
		t.Errorf("status=%v", resp["status"])
	}
	if resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details=%v", resp["incomplete_details"])
	}
}
