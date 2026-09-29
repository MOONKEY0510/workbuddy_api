package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// sseEvents 把 SSE 文本解析为 (event, data) 序列，供事件流断言。
type sseEvent struct {
	event string
	data  map[string]any
}

func parseSSEEvents(t *testing.T, s string) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(payload), &cur.data); err != nil {
				t.Fatalf("bad SSE data %q: %v", payload, err)
			}
		case line == "" && cur.data != nil:
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func eventNames(events []sseEvent) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.event)
	}
	return names
}

func TestAnthropicRequestToOpenAI(t *testing.T) {
	body := []byte(`{
	  "model": "global:claude-sonnet-4",
	  "max_tokens": 1024,
	  "system": [{"type":"text","text":"you are helpful"}],
	  "temperature": 0.3,
	  "stop_sequences": ["\n\n"],
	  "stream": true,
	  "tools": [{"name":"shell","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
	  "tool_choice": {"type":"tool","name":"shell"},
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	    {"role":"assistant","content":[{"type":"text","text":"好的"},{"type":"tool_use","id":"toolu_1","name":"shell","input":{"cmd":"ls"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file.txt"},{"type":"text","text":"继续"}]}
	  ]
	}`)
	out, stream, err := AnthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Error("stream flag must be preserved")
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req["model"] != "global:claude-sonnet-4" {
		t.Errorf("model=%v", req["model"])
	}
	if req["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens=%v", req["max_tokens"])
	}
	if req["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "shell" {
		t.Errorf("tool_choice=%v", req["tool_choice"])
	}
	if len(req["tools"].([]any)) != 1 {
		t.Errorf("tools=%v", req["tools"])
	}
	msgs := req["messages"].([]any)
	// system + user(富内容) + assistant + tool + user
	if len(msgs) != 5 {
		t.Fatalf("messages=%d want 5: %s", len(msgs), string(out))
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "you are helpful" {
		t.Errorf("system 消息=%v", msgs[0])
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("user 富内容=%v", parts)
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if img != "data:image/png;base64,AAA" {
		t.Errorf("image url=%s", img)
	}
	asst := msgs[2].(map[string]any)
	if asst["content"] != "好的" {
		t.Errorf("assistant content=%v", asst["content"])
	}
	tcs := asst["tool_calls"].([]any)
	if tcs[0].(map[string]any)["id"] != "toolu_1" {
		t.Errorf("tool_calls=%v", tcs)
	}
	if fn := tcs[0].(map[string]any)["function"].(map[string]any); fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments=%v", fn["arguments"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "file.txt" {
		t.Errorf("tool 消息=%v", toolMsg)
	}
	if msgs[4].(map[string]any)["content"] != "继续" {
		t.Errorf("后继 user 消息=%v", msgs[4])
	}
}

func TestAnthropicRequestSystemStringAndEmptyMessages(t *testing.T) {
	out, stream, err := AnthropicToOpenAI([]byte(`{"model":"m","system":"hi","messages":[{"role":"user","content":"yo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Error("default stream must be false")
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	msgs := req["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "hi" {
		t.Errorf("messages=%v", msgs)
	}
	if _, _, err := AnthropicToOpenAI([]byte(`{"model":"m","messages":[]}`)); err == nil {
		t.Error("empty messages must error")
	}
}

func TestAnthropicResponse(t *testing.T) {
	resp := map[string]any{
		"id":      "chatcmpl-abc",
		"model":   "glm-5.2",
		"created": float64(1),
		"choices": []any{map[string]any{
			"index": float64(0),
			"message": map[string]any{
				"role":              "assistant",
				"content":           "答案",
				"reasoning_content": "想一下",
				"tool_calls": []any{map[string]any{
					"id":       "call_1",
					"type":     "function",
					"function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(11), "completion_tokens": float64(7), "total_tokens": float64(18),
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(3)},
		},
	}
	out := AnthropicResponse(resp, "req-model")
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("信封=%v", out)
	}
	if out["id"] != "msg_abc" {
		t.Errorf("id=%v", out["id"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", out["stop_reason"])
	}
	content := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content=%v", content)
	}
	if content[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("首块应为 thinking: %v", content[0])
	}
	if content[1].(map[string]any)["text"] != "答案" {
		t.Errorf("文本块=%v", content[1])
	}
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "shell" {
		t.Errorf("tool_use=%v", tu)
	}
	if tu["input"].(map[string]any)["cmd"] != "ls" {
		t.Errorf("input=%v", tu["input"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 11 || usage["output_tokens"] != 7 {
		t.Errorf("usage=%v", usage)
	}
	if usage["cache_read_input_tokens"] != 3 {
		t.Errorf("cache 口径缺失: %v", usage)
	}
}

func TestAnthropicStreamTextThenTool(t *testing.T) {
	var sb strings.Builder
	st := NewAnthropicStream(&sb, nil, "glm-5.2")
	frames := []map[string]any{
		{"id": "chatcmpl-1", "model": "glm-5.2", "choices": []any{map[string]any{
			"index": float64(0), "delta": map[string]any{"role": "assistant", "content": "你好"}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": float64(0), "id": "call_1", "type": "function",
				"function": map[string]any{"name": "shell", "arguments": `{"cmd"`}}}}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": float64(0), "function": map[string]any{"arguments": `:"ls"}`}}}}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{"prompt_tokens": float64(4), "completion_tokens": float64(9), "total_tokens": float64(13)}},
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
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	got := eventNames(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列:\n got=%v\nwant=%v\nraw=%s", got, want, sb.String())
	}
	if events[0].data["message"].(map[string]any)["model"] != "glm-5.2" {
		t.Errorf("message_start=%v", events[0].data)
	}
	if events[1].data["content_block"].(map[string]any)["type"] != "text" {
		t.Errorf("首块应为 text: %v", events[1].data)
	}
	if events[1].data["index"] != float64(0) {
		t.Errorf("content_block_start index 应从 0 开始: %v", events[1].data["index"])
	}
	if events[4].data["index"] != float64(1) {
		t.Errorf("tool 块 index 应为 1: %v", events[4].data["index"])
	}
	if events[2].data["delta"].(map[string]any)["type"] != "text_delta" {
		t.Errorf("delta=%v", events[2].data)
	}
	toolStart := events[4].data["content_block"].(map[string]any)
	if toolStart["type"] != "tool_use" || toolStart["id"] != "call_1" || toolStart["name"] != "shell" {
		t.Errorf("tool 块=%v", toolStart)
	}
	if events[5].data["delta"].(map[string]any)["partial_json"] != `{"cmd"` {
		t.Errorf("首片参数=%v", events[5].data)
	}
	if events[6].data["delta"].(map[string]any)["partial_json"] != `:"ls"}` {
		t.Errorf("次片参数=%v", events[6].data)
	}
	md := events[8].data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", md["delta"])
	}
	if md["usage"].(map[string]any)["output_tokens"] != float64(9) {
		t.Errorf("usage=%v", md["usage"])
	}
}

func TestAnthropicStreamThinkingAndError(t *testing.T) {
	var sb strings.Builder
	st := NewAnthropicStream(&sb, nil, "")
	if err := st.Frame(map[string]any{"choices": []any{map[string]any{
		"index": float64(0), "delta": map[string]any{"reasoning_content": "推理"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ErrorFrame(map[string]any{"message": "上游限流", "code": "6004"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(); err != nil {
		t.Fatal(err)
	}
	events := parseSSEEvents(t, sb.String())
	names := eventNames(events)
	if names[len(names)-1] != "message_stop" {
		t.Errorf("收尾应为 message_stop: %v", names)
	}
	found := false
	for _, e := range events {
		if e.event == "content_block_start" && e.data["content_block"].(map[string]any)["type"] == "thinking" {
			found = true
		}
	}
	if !found {
		t.Errorf("应发出 thinking 块: %s", sb.String())
	}
	errFound := false
	for _, e := range events {
		if e.event == "error" {
			errFound = true
		}
	}
	if !errFound {
		t.Errorf("错误帧应转成 error 事件: %s", sb.String())
	}
}
