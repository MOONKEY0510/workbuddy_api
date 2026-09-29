package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiRequestToOpenAI(t *testing.T) {
	body := []byte(`{
	  "systemInstruction": {"parts":[{"text":"你是助手"}]},
	  "generationConfig": {"temperature":0.4,"topP":0.9,"topK":40,"maxOutputTokens":512,"stopSequences":["END"]},
	  "tools": [{"functionDeclarations":[{"name":"shell","description":"run","parameters":{"type":"OBJECT","properties":{"cmd":{"type":"STRING"}}}}]}],
	  "toolConfig": {"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["shell"]}},
	  "contents": [
	    {"role":"user","parts":[{"text":"看图"},{"inlineData":{"mimeType":"image/png","data":"CCC"}}]},
	    {"role":"model","parts":[{"functionCall":{"name":"shell","args":{"cmd":"ls"}}}]},
	    {"role":"user","parts":[{"functionResponse":{"name":"shell","response":{"result":"file.txt"}}},{"text":"继续"}]}
	  ]
	}`)
	out, err := GeminiToOpenAI(body, "gemini-2.5-pro", true)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req["model"] != "gemini-2.5-pro" || req["max_tokens"] != float64(512) || req["temperature"] != 0.4 {
		t.Errorf("顶层=%v", req)
	}
	if req["stream"] != true {
		t.Errorf("URL 方法决定的 stream 必须写入: %v", req["stream"])
	}
	if req["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "shell" {
		t.Errorf("tool_choice=%v", req["tool_choice"])
	}
	params := req["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if params["type"] != "object" { // 大写 OBJECT 必须归一为小写
		t.Errorf("parameters.type=%v", params["type"])
	}
	props := params["properties"].(map[string]any)["cmd"].(map[string]any)
	if props["type"] != "string" {
		t.Errorf("嵌套 type=%v", props["type"])
	}
	msgs := req["messages"].([]any)
	// system + user(富) + assistant(tool_call) + tool + user
	if len(msgs) != 5 {
		t.Fatalf("messages=%d: %s", len(msgs), string(out))
	}
	if msgs[0].(map[string]any)["content"] != "你是助手" {
		t.Errorf("system=%v", msgs[0])
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,CCC" {
		t.Errorf("inlineData 转图失败: %v", parts)
	}
	asst := msgs[2].(map[string]any)
	callID := asst["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
	if callID == "" || !strings.HasPrefix(callID, "call_") {
		t.Errorf("tool_calls id=%v", asst["tool_calls"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != callID || toolMsg["content"] != "file.txt" {
		t.Errorf("functionResponse 配对失败: %v (callID=%s)", toolMsg, callID)
	}
	if msgs[4].(map[string]any)["content"] != "继续" {
		t.Errorf("后继 user=%v", msgs[4])
	}
	if req["stop"].([]any)[0] != "END" {
		t.Errorf("stop=%v", req["stop"])
	}
}

func TestGeminiResponse(t *testing.T) {
	resp := map[string]any{
		"model": "glm-5.2",
		"choices": []any{map[string]any{
			"index": float64(0),
			"message": map[string]any{
				"role": "assistant", "content": "答案", "reasoning_content": "想想",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": float64(8), "completion_tokens": float64(2), "total_tokens": float64(10),
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(1)}},
	}
	out := GeminiResponse(resp, "")
	cands := out["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates=%v", cands)
	}
	cand := cands[0].(map[string]any)
	if cand["finishReason"] != "STOP" {
		t.Errorf("finishReason=%v", cand["finishReason"])
	}
	content := cand["content"].(map[string]any)
	if content["role"] != "model" {
		t.Errorf("role=%v", content["role"])
	}
	parts := content["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts=%v", parts)
	}
	if thought, _ := parts[0].(map[string]any)["thought"].(bool); !thought {
		t.Errorf("首 part 应为 thought: %v", parts[0])
	}
	if parts[1].(map[string]any)["text"] != "答案" {
		t.Errorf("文本 part=%v", parts[1])
	}
	fc := parts[2].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "shell" || fc["args"].(map[string]any)["cmd"] != "ls" {
		t.Errorf("functionCall=%v", fc)
	}
	usage := out["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != 8 || usage["candidatesTokenCount"] != 2 || usage["totalTokenCount"] != 10 {
		t.Errorf("usage=%v", usage)
	}
	if usage["cachedContentTokenCount"] != 1 {
		t.Errorf("cached 口径缺失: %v", usage)
	}
}

func TestGeminiStreamSSEAndArray(t *testing.T) {
	frames := []map[string]any{
		{"model": "glm-5.2", "choices": []any{map[string]any{
			"index": float64(0), "delta": map[string]any{"role": "assistant", "content": "你好"}}}},
		{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "世界",
			"tool_calls": []any{map[string]any{"index": float64(0), "id": "call_2", "type": "function",
				"function": map[string]any{"name": "shell", "arguments": `{"cmd"`}}}}}}},
		{
			"choices": []any{map[string]any{
				"index": float64(0),
				"delta": map[string]any{"tool_calls": []any{
					map[string]any{"index": float64(0), "function": map[string]any{"arguments": `:"ls"}`}},
				}},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": float64(1), "completion_tokens": float64(2), "total_tokens": float64(3)},
		},
	}

	// SSE 模式
	var sb strings.Builder
	st := NewGeminiStream(&sb, nil, "m", true)
	for _, f := range frames {
		if err := st.Frame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Finish(); err != nil {
		t.Fatal(err)
	}
	events := parseSSEEvents(t, sb.String())
	if len(events) == 0 {
		t.Fatal("无 SSE 事件")
	}
	var textOut strings.Builder
	var toolArgs map[string]any
	var finish string
	var usage map[string]any
	for _, e := range events {
		for _, cv := range asSlice(e.data["candidates"]) {
			c := cv.(map[string]any)
			if fr := asString(c["finishReason"]); fr != "" {
				finish = fr
			}
			for _, pv := range asSlice(c["content"].(map[string]any)["parts"]) {
				p := pv.(map[string]any)
				if t, ok := p["text"].(string); ok {
					textOut.WriteString(t)
				}
				if fc := asMap(p["functionCall"]); fc != nil {
					toolArgs = asMap(fc["args"])
				}
			}
		}
		if u := asMap(e.data["usageMetadata"]); u != nil {
			usage = u
		}
	}
	if textOut.String() != "你好世界" {
		t.Errorf("文本拼接=%q", textOut.String())
	}
	if toolArgs == nil || toolArgs["cmd"] != "ls" {
		t.Errorf("functionCall args=%v", toolArgs)
	}
	if finish != "STOP" {
		t.Errorf("finishReason=%v", finish)
	}
	// 事件经 SSE 文本解析 → JSON number 为 float64
	if usage == nil || usage["totalTokenCount"] != float64(3) {
		t.Errorf("usage=%v", usage)
	}

	// JSON 数组模式（Gemini REST 默认 alt=json）
	var ab strings.Builder
	st2 := NewGeminiStream(&ab, nil, "m", false)
	for _, f := range frames {
		_ = st2.Frame(f)
	}
	_ = st2.Finish()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(ab.String()), &arr); err != nil {
		t.Fatalf("JSON 数组输出非法: %v (%s)", err, ab.String())
	}
	if len(arr) < 3 {
		t.Errorf("chunk 数=%d", len(arr))
	}
	last := arr[len(arr)-1]
	if asSlice(last["candidates"])[0].(map[string]any)["finishReason"] != "STOP" {
		t.Errorf("末 chunk=%v", last)
	}
}

func TestGeminiSSEContentTypeContract(t *testing.T) {
	// alt=sse 下每条事件以 data: 开头且不含 event: 行（Gemini 规范）。
	var sb strings.Builder
	st := NewGeminiStream(&sb, nil, "m", true)
	_ = st.Frame(map[string]any{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "x"}}}})
	_ = st.Finish()
	if strings.Contains(sb.String(), "event: ") {
		t.Errorf("Gemini SSE 不应有 event: 行: %s", sb.String())
	}
	if !strings.Contains(sb.String(), "data: {") {
		t.Errorf("Gemini SSE 缺少 data 帧: %s", sb.String())
	}
}
