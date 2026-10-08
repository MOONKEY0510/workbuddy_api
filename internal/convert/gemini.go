// gemini.go 实现 Google Gemini generateContent（/v1beta/models/{model}:generateContent）
// 与 OpenAI Chat Completions 的互转：请求（contents/systemInstruction/tools）与
// 响应（completion/delta → GenerateContentResponse 与 SSE / JSON 数组流）。
package convert

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// 请求方向：Gemini → OpenAI Chat Completions
// ---------------------------------------------------------------------------

// GeminiToOpenAI 把 Gemini generateContent 请求体转换为 OpenAI chat.completions
// 请求体。model 与 stream 均来自 URL（Gemini 请求体里既不含模型名，流式与否也
// 由 :generateContent / :streamGenerateContent 方法决定）。
func GeminiToOpenAI(body []byte, model string, stream bool) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid gemini request: %w", err)
	}
	out := map[string]any{"model": model, "stream": stream}

	msgs := make([]any, 0, 8)
	if sys := geminiSystemText(asMap(req["systemInstruction"])); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	msgs = append(msgs, geminiContentsToMessages(asSlice(req["contents"]))...)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("invalid gemini request: contents is empty")
	}
	out["messages"] = msgs

	if tools := geminiToolsToOpenAI(asSlice(req["tools"])); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := geminiToolConfigToOpenAI(asMap(req["toolConfig"])); tc != nil {
		out["tool_choice"] = tc
	}
	gen := asMap(req["generationConfig"])
	if gen != nil {
		if n, ok := asInt(gen["maxOutputTokens"]); ok && n > 0 {
			out["max_tokens"] = n
		}
		if v, ok := asFloat(gen["temperature"]); ok {
			out["temperature"] = v
		}
		if v, ok := asFloat(gen["topP"]); ok {
			out["top_p"] = v
		}
		if ss := asSlice(gen["stopSequences"]); len(ss) > 0 {
			stop := make([]string, 0, len(ss))
			for _, s := range ss {
				stop = append(stop, asString(s))
			}
			out["stop"] = stop
		}
		if rf := geminiResponseFormat(gen); rf != nil {
			out["response_format"] = rf
		}
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// geminiSystemText systemInstruction.parts[].text 拼接。
func geminiSystemText(sys map[string]any) string {
	if sys == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range asSlice(sys["parts"]) {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		if txt := asString(pm["text"]); txt != "" {
			sb.WriteString(txt)
		}
	}
	return sb.String()
}

// geminiContentsToMessages Gemini contents → OpenAI messages。
// functionCall → assistant.tool_calls；functionResponse → tool 消息；二者按
// name→id 栈配对（Gemini 不给调用 ID，网关侧生成并回填配对）。
func geminiContentsToMessages(contents []any) []any {
	var out []any
	calls := map[string][]string{} // function name → 待配对的调用 ID 栈

	for _, cv := range contents {
		c := asMap(cv)
		if c == nil {
			continue
		}
		role := "user"
		if asString(c["role"]) == "model" {
			role = "assistant"
		}
		var text strings.Builder
		var rich []any
		hasImage := false
		var toolCalls []any
		var toolResults []any
		var reasoning strings.Builder

		for _, pv := range asSlice(c["parts"]) {
			p := asMap(pv)
			if p == nil {
				continue
			}
			switch {
			case p["text"] != nil:
				txt := asString(p["text"])
				if txt == "" {
					continue
				}
				if thought, _ := p["thought"].(bool); thought {
					reasoning.WriteString(txt)
					continue
				}
				if hasImage {
					rich = append(rich, map[string]any{"type": "text", "text": txt})
				} else {
					text.WriteString(txt)
				}
			case p["inlineData"] != nil:
				d := asMap(p["inlineData"])
				data, mt := asString(d["data"]), pickStr(asString(d["mimeType"]), "image/png")
				if data == "" {
					continue
				}
				if !hasImage {
					hasImage = true
					if text.Len() > 0 {
						rich = append(rich, map[string]any{"type": "text", "text": text.String()})
						text.Reset()
					}
				}
				rich = append(rich, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + mt + ";base64," + data},
				})
			case p["fileData"] != nil:
				d := asMap(p["fileData"])
				uri := asString(d["fileUri"])
				if uri == "" {
					continue
				}
				if !hasImage {
					hasImage = true
					if text.Len() > 0 {
						rich = append(rich, map[string]any{"type": "text", "text": text.String()})
						text.Reset()
					}
				}
				rich = append(rich, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": uri},
				})
			case p["functionCall"] != nil:
				fc := asMap(p["functionCall"])
				name := asString(fc["name"])
				if name == "" {
					continue
				}
				id := "call_" + genID()
				calls[name] = append(calls[name], id)
				args := "{}"
				if a, ok := fc["args"]; ok && a != nil {
					if s := jsonString(a); s != "" {
						args = s
					}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case p["functionResponse"] != nil:
				fr := asMap(p["functionResponse"])
				name := asString(fr["name"])
				id := name // 无配对时退化为 name（至少保证非空）
				if stack := calls[name]; len(stack) > 0 {
					id = stack[len(stack)-1]
					calls[name] = stack[:len(stack)-1]
				}
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": id,
					"content":      geminiFunctionResponseText(fr["response"]),
				})
			default:
				// executableCode / codeExecutionResult 等：无 Chat 等价物，跳过。
				continue
			}
		}

		if role == "assistant" {
			msg := map[string]any{"role": "assistant", "content": text.String()}
			if hasImage {
				msg["content"] = rich
			}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			if reasoning.Len() > 0 {
				msg["reasoning_content"] = reasoning.String()
			}
			if len(toolCalls) > 0 || text.Len() > 0 || hasImage || reasoning.Len() > 0 {
				out = append(out, msg)
			}
			continue
		}
		// user 侧：tool 结果独立成条（在 user 文本之前，保证与 assistant.tool_calls 配对）。
		out = append(out, toolResults...)
		if hasImage {
			out = append(out, map[string]any{"role": "user", "content": rich})
		} else if text.Len() > 0 {
			out = append(out, map[string]any{"role": "user", "content": text.String()})
		}
	}
	return out
}

// geminiFunctionResponseText functionResponse.response 归一为文本。
func geminiFunctionResponseText(v any) string {
	if m := asMap(v); m != nil {
		// 常见形态 {"result": ...} 或 {"output": ...}：优先取常见键的文本值。
		for _, k := range []string{"result", "output", "content", "text"} {
			if s := asString(m[k]); s != "" {
				return s
			}
		}
		return jsonString(m)
	}
	return jsonString(v)
}

// geminiToolsToOpenAI tools[].functionDeclarations[] → OpenAI tools。
func geminiToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, tv := range tools {
		t := asMap(tv)
		if t == nil {
			continue
		}
		for _, dv := range asSlice(t["functionDeclarations"]) {
			d := asMap(dv)
			if d == nil {
				continue
			}
			name := asString(d["name"])
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if desc := asString(d["description"]); desc != "" {
				fn["description"] = desc
			}
			if p, ok := d["parameters"]; ok && p != nil {
				fn["parameters"] = geminiSchemaToJSONSchema(p)
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

// geminiSchemaToJSONSchema 把 Gemini 的 Schema（type 用大写枚举）转成 JSON Schema
// （type 用小写）。只做类型名归一，其余字段原样保留。
func geminiSchemaToJSONSchema(v any) any {
	m := asMap(v)
	if m == nil {
		return v
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		switch k {
		case "type":
			if s := asString(val); s != "" {
				out[k] = strings.ToLower(s)
				continue
			}
			out[k] = val
		case "properties":
			props := asMap(val)
			if props == nil {
				out[k] = val
				continue
			}
			np := make(map[string]any, len(props))
			for pk, pv := range props {
				np[pk] = geminiSchemaToJSONSchema(pv)
			}
			out[k] = np
		case "items":
			out[k] = geminiSchemaToJSONSchema(val)
		default:
			out[k] = val
		}
	}
	return out
}

// geminiToolConfigToOpenAI toolConfig.functionCallingConfig.mode → OpenAI tool_choice。
func geminiToolConfigToOpenAI(cfg map[string]any) any {
	if cfg == nil {
		return nil
	}
	fcc := asMap(cfg["functionCallingConfig"])
	if fcc == nil {
		return nil
	}
	switch strings.ToUpper(asString(fcc["mode"])) {
	case "AUTO":
		return "auto"
	case "ANY":
		// 指定 allowedFunctionNames 时按指定函数强制调用，否则 required。
		if names := asSlice(fcc["allowedFunctionNames"]); len(names) == 1 {
			if name := asString(names[0]); name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
		return "required"
	case "NONE":
		return "none"
	}
	return nil
}

// geminiResponseFormat generationConfig.responseMimeType/responseSchema → response_format。
func geminiResponseFormat(gen map[string]any) any {
	if strings.EqualFold(asString(gen["responseMimeType"]), "application/json") {
		if schema := gen["responseSchema"]; schema != nil {
			return map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "response",
					"schema": geminiSchemaToJSONSchema(schema),
				},
			}
		}
		return map[string]any{"type": "json_object"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 响应方向（非流式）：OpenAI completion → GenerateContentResponse
// ---------------------------------------------------------------------------

// GeminiFinishReason OpenAI finish_reason → Gemini finishReason。
func GeminiFinishReason(fr string) string {
	switch fr {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "tool_calls", "function_call", "stop", "":
		return "STOP"
	default:
		return "STOP"
	}
}

// GeminiResponse 把 OpenAI 非流式 completion 转换为 GenerateContentResponse。
func GeminiResponse(resp map[string]any, model string) map[string]any {
	msg := asMap(openaiChoice(resp)["message"])
	parts := make([]any, 0, 3)
	if rc := asString(msg["reasoning_content"]); rc != "" {
		parts = append(parts, map[string]any{"text": rc, "thought": true})
	}
	if txt := openaiMessageText(msg["content"]); txt != "" {
		parts = append(parts, map[string]any{"text": txt})
	}
	for _, tcv := range asSlice(msg["tool_calls"]) {
		tc := asMap(tcv)
		fn := asMap(tc["function"])
		if fn == nil {
			continue
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{
				"name": asString(fn["name"]),
				"args": parseToolArguments(asString(fn["arguments"])),
			},
		})
	}
	if v := asString(resp["model"]); v != "" {
		model = v
	}
	if model == "" {
		model = "unknown"
	}
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": GeminiFinishReason(asString(openaiChoice(resp)["finish_reason"])),
			"index":        0,
		}},
		"modelVersion": model,
	}
	if u := geminiUsage(resp["usage"]); u != nil {
		out["usageMetadata"] = u
	}
	return out
}

// geminiUsage OpenAI usage → usageMetadata。
func geminiUsage(v any) map[string]any {
	u := parseUsage(v)
	if !u.HasPrompt && !u.HasCompletion && !u.HasTotal {
		return nil
	}
	total := u.Total
	if !u.HasTotal {
		total = u.Prompt + u.Completion
	}
	out := map[string]any{
		"promptTokenCount":     u.Prompt,
		"candidatesTokenCount": u.Completion,
		"totalTokenCount":      total,
	}
	if u.HasCached {
		out["cachedContentTokenCount"] = u.Cached
	}
	if u.HasReasoning {
		out["thoughtsTokenCount"] = u.Reasoning
	}
	return out
}

// ---------------------------------------------------------------------------
// 响应方向（流式）：OpenAI SSE 帧 → Gemini 流
// ---------------------------------------------------------------------------

// geminiChunkWriter 流式输出后端：SSE（alt=sse）或 JSON 数组（默认 alt=json）。
type geminiChunkWriter interface {
	WriteChunk(obj map[string]any) error
	Finish() error
}

// geminiSSEWriter 每 chunk 一条 data: 帧（不带 [DONE]，Gemini 规范如此）。
type geminiSSEWriter struct {
	w     io.Writer
	flush func()
}

func (g *geminiSSEWriter) WriteChunk(obj map[string]any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(g.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if g.flush != nil {
		g.flush()
	}
	return nil
}

func (g *geminiSSEWriter) Finish() error { return nil }

// geminiArrayWriter 缓冲全部 chunk，结束时写 JSON 数组（Gemini 默认 alt=json 形态）。
type geminiArrayWriter struct {
	w     io.Writer
	flush func()
	buf   []json.RawMessage
}

func (g *geminiArrayWriter) WriteChunk(obj map[string]any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	g.buf = append(g.buf, b)
	return nil
}

func (g *geminiArrayWriter) Finish() error {
	out := make([]byte, 0, 64)
	out = append(out, '[')
	for i, b := range g.buf {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, b...)
	}
	out = append(out, ']')
	if _, err := g.w.Write(out); err != nil {
		return err
	}
	if g.flush != nil {
		g.flush()
	}
	return nil
}

// GeminiStream 把 OpenAI 流式 chunk 编码为 Gemini GenerateContentResponse 流。
// 工具调用需完整参数（Gemini 的 functionCall.args 是对象，不支持分片），
// 故在流内累积、收尾时统一发出。
type GeminiStream struct {
	cw    geminiChunkWriter
	model string

	finished bool
	failed   bool // 已写 error chunk（终态）：收尾不得再补工具/终止 chunk
	// 工具调用累积：index → {name, args}
	toolOrder []int
	toolName  map[int]string
	toolArgs  map[int]*strings.Builder

	finishReason string
	usage        map[string]any
	sawText      bool
}

// NewGeminiStream 构建 Gemini 流编码器；sse=false 走 JSON 数组输出。
func NewGeminiStream(w io.Writer, flush func(), model string, sse bool) *GeminiStream {
	if model == "" {
		model = "unknown"
	}
	var cw geminiChunkWriter
	if sse {
		cw = &geminiSSEWriter{w: w, flush: flush}
	} else {
		cw = &geminiArrayWriter{w: w, flush: flush}
	}
	return &GeminiStream{
		cw: cw, model: model,
		toolName: map[int]string{},
		toolArgs: map[int]*strings.Builder{},
	}
}

// chunk 输出一个 candidate 片段（parts 可为空）。
func (s *GeminiStream) chunk(parts []any, finish string) error {
	cand := map[string]any{
		"content": map[string]any{"role": "model", "parts": parts},
		"index":   0,
	}
	if finish != "" {
		cand["finishReason"] = finish
	}
	obj := map[string]any{
		"candidates":   []any{cand},
		"modelVersion": s.model,
	}
	if finish != "" && s.usage != nil {
		obj["usageMetadata"] = s.usage
	}
	return s.cw.WriteChunk(obj)
}

// Frame 处理一帧 OpenAI 流式 chunk。
func (s *GeminiStream) Frame(chunk map[string]any) error {
	if m := asString(chunk["model"]); m != "" && s.model == "unknown" {
		s.model = m
	}
	if u := geminiUsage(chunk["usage"]); u != nil {
		s.usage = u
	}
	choice := openaiChoice(chunk)
	if choice == nil {
		return nil
	}
	if fr := asString(choice["finish_reason"]); fr != "" {
		s.finishReason = fr
	}
	delta := asMap(choice["delta"])
	if delta == nil {
		return nil
	}
	var parts []any
	if rc := asString(delta["reasoning_content"]); rc != "" {
		parts = append(parts, map[string]any{"text": rc, "thought": true})
	}
	if txt := asString(delta["content"]); txt != "" {
		parts = append(parts, map[string]any{"text": txt})
		s.sawText = true
	}
	for _, tcv := range asSlice(delta["tool_calls"]) {
		tc := asMap(tcv)
		if tc == nil {
			continue
		}
		idx, _ := asInt(tc["index"])
		fn := asMap(tc["function"])
		if _, ok := s.toolArgs[idx]; !ok {
			s.toolArgs[idx] = &strings.Builder{}
			s.toolOrder = append(s.toolOrder, idx)
		}
		if name := asString(fn["name"]); name != "" {
			s.toolName[idx] = name
		}
		if args := asString(fn["arguments"]); args != "" {
			s.toolArgs[idx].WriteString(args)
		}
	}
	if len(parts) > 0 {
		return s.chunk(parts, "")
	}
	return nil
}

// ErrorFrame 处理 OpenAI error 帧：Gemini 无流内错误事件，转为带 error 的 chunk。
// 该 chunk 即终态（StreamHint/收尾都不再补成功语义的终止 chunk），客户端不会把失败
// 读成"正常结束但内容为空"。
func (s *GeminiStream) ErrorFrame(errObj map[string]any) error {
	s.failed = true
	msg := asString(errObj["message"])
	if msg == "" {
		msg = jsonString(errObj)
	}
	return s.cw.WriteChunk(map[string]any{
		"error": map[string]any{
			"code":    500,
			"message": msg,
			"status":  "INTERNAL",
		},
	})
}

// Finish 收尾：发出累积的工具调用与终止 chunk（幂等）。
// 已写过 error chunk 时直接结束：不能再补 finishReason 的终止 chunk（那等于宣告
// "正常结束"，与 error 互相矛盾）。
func (s *GeminiStream) Finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	if s.failed {
		return s.cw.Finish()
	}
	if len(s.toolOrder) > 0 {
		parts := make([]any, 0, len(s.toolOrder))
		for _, idx := range s.toolOrder {
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": s.toolName[idx],
					"args": parseToolArguments(s.toolArgs[idx].String()),
				},
			})
		}
		if err := s.chunk(parts, ""); err != nil {
			return err
		}
	}
	if err := s.chunk([]any{}, GeminiFinishReason(s.finishReason)); err != nil {
		return err
	}
	return s.cw.Finish()
}
