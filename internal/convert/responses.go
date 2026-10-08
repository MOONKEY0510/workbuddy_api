// responses.go 实现 OpenAI Responses API（/v1/responses，Codex CLI 使用的协议）
// 与 OpenAI Chat Completions 的互转：请求（instructions/input/tools → messages）
// 与响应（completion/delta → response 对象与官方事件流）。
package convert

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// 请求方向：Responses → OpenAI Chat Completions
// ---------------------------------------------------------------------------

// ResponsesToOpenAI 把 Responses 请求体转换为 OpenAI chat.completions 请求体。
func ResponsesToOpenAI(body []byte) ([]byte, bool, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("invalid responses request: %w", err)
	}
	stream, _ := req["stream"].(bool)
	out := map[string]any{"model": asString(req["model"]), "stream": stream}

	msgs := make([]any, 0, 8)
	if ins := asString(req["instructions"]); ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}
	msgs = append(msgs, responsesInputToMessages(req["input"])...)

	// Codex 有时把最后一条 user 输入放在 input 之外的顶层（旧版客户端）：
	// 兜底把空 messages 视为错误，避免把无上下文请求送到上游。
	if len(msgs) == 0 {
		return nil, false, fmt.Errorf("invalid responses request: input is empty")
	}
	out["messages"] = msgs

	if n, ok := asInt(req["max_output_tokens"]); ok && n > 0 {
		out["max_tokens"] = n
	}
	if v, ok := req["temperature"].(float64); ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok {
		out["top_p"] = v
	}
	// reasoning.effort → reasoning_effort（网关会按模型支持档位降级）。
	if r := asMap(req["reasoning"]); r != nil {
		if e := asString(r["effort"]); e != "" {
			out["reasoning_effort"] = e
		}
	}
	if tools := responsesToolsToOpenAI(asSlice(req["tools"])); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := responsesToolChoiceToOpenAI(req["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	// text.format → response_format（json_schema / json_object 的语义等价映射）。
	if rf := responsesTextFormatToResponseFormat(asMap(req["text"])); rf != nil {
		out["response_format"] = rf
	}
	if u := asString(req["user"]); u != "" {
		out["user"] = u
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return buf, stream, nil
}

// responsesInputToMessages 把 Responses 的 input（字符串或 item 数组）转成 messages。
// 关键点：function_call / reasoning 是独立 item，要并入相邻的 assistant 消息
// （OpenAI Chat 要求 tool_calls 挂在 assistant 消息上）。
func responsesInputToMessages(input any) []any {
	if s, ok := input.(string); ok {
		if s == "" {
			return nil
		}
		return []any{map[string]any{"role": "user", "content": s}}
	}
	items := asSlice(input)
	if items == nil {
		return nil
	}
	var out []any
	// asst 累积中的 assistant 消息（content/tool_calls/reasoning_content）。
	var asst map[string]any
	var asstText strings.Builder
	var asstReason strings.Builder
	var asstTools []any

	flushAsst := func() {
		if asst == nil {
			return
		}
		if asstText.Len() > 0 {
			asst["content"] = asstText.String()
		} else {
			asst["content"] = ""
		}
		if len(asstTools) > 0 {
			asst["tool_calls"] = asstTools
		}
		if asstReason.Len() > 0 {
			asst["reasoning_content"] = asstReason.String()
		}
		out = append(out, asst)
		asst = nil
		asstText.Reset()
		asstReason.Reset()
		asstTools = nil
	}
	ensureAsst := func() {
		if asst == nil {
			asst = map[string]any{"role": "assistant"}
		}
	}

	for _, it := range items {
		m := asMap(it)
		if m == nil {
			continue
		}
		typ := asString(m["type"])
		// type 缺省但带 role：按 message 处理（宽容解析）。
		if typ == "" {
			if asString(m["role"]) != "" {
				typ = "message"
			} else {
				continue
			}
		}
		switch typ {
		case "message":
			role := pickStr(asString(m["role"]), "user")
			text, rich := responsesContentToOpenAI(m["content"])
			if role == "assistant" {
				ensureAsst()
				asstText.WriteString(text)
				if rich != nil {
					asst["content"] = rich
				}
				continue
			}
			flushAsst()
			if rich != nil {
				out = append(out, map[string]any{"role": role, "content": rich})
			} else if text != "" {
				out = append(out, map[string]any{"role": role, "content": text})
			}
		case "function_call":
			ensureAsst()
			asstTools = append(asstTools, map[string]any{
				"id":   pickStr(asString(m["call_id"]), asString(m["id"])),
				"type": "function",
				"function": map[string]any{
					"name":      asString(m["name"]),
					"arguments": pickStr(asString(m["arguments"]), "{}"),
				},
			})
		case "function_call_output":
			flushAsst()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": asString(m["call_id"]),
				"content":      responsesToolOutputText(m["output"]),
			})
		case "reasoning":
			ensureAsst()
			for _, s := range asSlice(m["summary"]) {
				sm := asMap(s)
				if sm == nil {
					continue
				}
				if txt := asString(sm["text"]); txt != "" {
					asstReason.WriteString(txt)
				}
			}
			if c := asString(m["content"]); c != "" {
				asstReason.WriteString(c)
			}
		default:
			// item_reference / computer_call / 其它扩展 item：跳过（无 Chat 等价物）。
			continue
		}
	}
	flushAsst()
	return out
}

// responsesContentToOpenAI 解析 Responses message.content（字符串或 part 数组）。
// 返回纯文本与富数组（含图片时为非 nil）。
func responsesContentToOpenAI(content any) (string, []any) {
	if s, ok := content.(string); ok {
		return s, nil
	}
	parts := asSlice(content)
	if parts == nil {
		return "", nil
	}
	var sb strings.Builder
	var rich []any
	hasImage := false
	for _, p := range parts {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		switch asString(pm["type"]) {
		case "input_text", "output_text", "text", "summary_text":
			if txt := asString(pm["text"]); txt != "" {
				if hasImage {
					rich = append(rich, map[string]any{"type": "text", "text": txt})
				} else {
					sb.WriteString(txt)
				}
			}
		case "refusal":
			if txt := asString(pm["refusal"]); txt != "" {
				sb.WriteString(txt)
			}
		case "input_image":
			url := asString(pm["image_url"])
			if url == "" {
				continue // file_id 形态：网关无法取回，跳过
			}
			if !hasImage {
				hasImage = true
				if sb.Len() > 0 {
					rich = append(rich, map[string]any{"type": "text", "text": sb.String()})
					sb.Reset()
				}
			}
			img := map[string]any{"url": url}
			if d := asString(pm["detail"]); d != "" {
				img["detail"] = d
			}
			rich = append(rich, map[string]any{"type": "image_url", "image_url": img})
		}
	}
	if hasImage {
		return "", rich
	}
	return sb.String(), nil
}

// responsesToolOutputText function_call_output.output 归一为文本。
func responsesToolOutputText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, p := range t {
			pm := asMap(p)
			if pm == nil {
				continue
			}
			if txt := asString(pm["text"]); txt != "" {
				sb.WriteString(txt)
			}
		}
		return sb.String()
	default:
		return jsonString(v)
	}
}

// responsesToolsToOpenAI Responses tools → OpenAI tools（function 字段是扁平结构）。
func responsesToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, tv := range tools {
		t := asMap(tv)
		if t == nil || asString(t["type"]) != "function" {
			continue // web_search / file_search 等宿主工具无 Chat 等价物：跳过
		}
		name := asString(t["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d := asString(t["description"]); d != "" {
			fn["description"] = d
		}
		if p, ok := t["parameters"]; ok && p != nil {
			fn["parameters"] = p
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// responsesToolChoiceToOpenAI Responses tool_choice → OpenAI tool_choice。
func responsesToolChoiceToOpenAI(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "required", "none":
			return t
		}
		return nil
	case map[string]any:
		if asString(t["type"]) == "function" {
			if name := asString(t["name"]); name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	return nil
}

// responsesTextFormatToResponseFormat text.format → response_format。
func responsesTextFormatToResponseFormat(text map[string]any) any {
	if text == nil {
		return nil
	}
	f := asMap(text["format"])
	if f == nil {
		return nil
	}
	switch asString(f["type"]) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		inner := map[string]any{}
		for _, k := range []string{"name", "schema", "strict", "description"} {
			if v, ok := f[k]; ok {
				inner[k] = v
			}
		}
		if len(inner) == 0 {
			return nil
		}
		return map[string]any{"type": "json_schema", "json_schema": inner}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 响应方向（非流式）：OpenAI completion → Responses response
// ---------------------------------------------------------------------------

// ResponsesStopStatus OpenAI finish_reason → Responses 的 incomplete 判定。
func responsesIncompleteReason(fr string) string {
	switch fr {
	case "length":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	}
	return ""
}

// ResponsesResponse 把 OpenAI 非流式 completion 转换 Responses response 对象。
func ResponsesResponse(resp map[string]any, model string) map[string]any {
	choice := openaiChoice(resp)
	msg := asMap(choice["message"])
	output := make([]any, 0, 3)

	if rc := asString(msg["reasoning_content"]); rc != "" {
		output = append(output, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + genID(),
			"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
		})
	}
	text := openaiMessageText(msg["content"])
	if text != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + genID(),
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}
	for _, tcv := range asSlice(msg["tool_calls"]) {
		tc := asMap(tcv)
		fn := asMap(tc["function"])
		if fn == nil {
			continue
		}
		callID := pickStr(asString(tc["id"]), "call_"+genID())
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + genID(),
			"call_id":   callID,
			"name":      asString(fn["name"]),
			"arguments": pickStr(asString(fn["arguments"]), "{}"),
			"status":    "completed",
		})
	}
	if model == "" {
		model = pickStr(asString(resp["model"]), "unknown")
	}
	fr := asString(choice["finish_reason"])
	out := responsesBase(model, responsesResponseID(asString(resp["id"])), asInt64(resp["created"]))
	out["status"] = "completed"
	if r := responsesIncompleteReason(fr); r != "" && fr == "length" {
		out["status"] = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": r}
	}
	out["output"] = output
	out["usage"] = responsesUsage(resp["usage"])
	return out
}

// responsesResponseID 把 chatcmpl-xxx 归一到 resp_ 前缀。
func responsesResponseID(id string) string {
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	if id == "" {
		return "resp_" + genID()
	}
	return "resp_" + strings.TrimPrefix(id, "chatcmpl-")
}

// asInt64 JSON number → int64（缺失 → 当前时间）。
func asInt64(v any) int64 {
	if n, ok := asFloat(v); ok && n > 0 {
		return int64(n)
	}
	return nowUnix()
}

// responsesBase 构造 response 对象骨架（created/in_progress/completed 各态共用）。
func responsesBase(model, id string, created int64) map[string]any {
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           created,
		"status":               "in_progress",
		"background":           false,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                false,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"user":                 nil,
		"metadata":             map[string]any{},
	}
}

// responsesUsage OpenAI usage → Responses usage。
func responsesUsage(v any) map[string]any {
	u := parseUsage(v)
	if !u.HasPrompt && !u.HasCompletion && !u.HasTotal {
		return nil
	}
	out := map[string]any{
		"input_tokens":  u.Prompt,
		"output_tokens": u.Completion,
	}
	if u.HasTotal {
		out["total_tokens"] = u.Total
	} else {
		out["total_tokens"] = u.Prompt + u.Completion
	}
	if u.HasCached {
		out["input_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
	}
	if u.HasReasoning {
		out["output_tokens_details"] = map[string]any{"reasoning_tokens": u.Reasoning}
	}
	return out
}

// ---------------------------------------------------------------------------
// 响应方向（流式）：OpenAI SSE 帧 → Responses 事件流
// ---------------------------------------------------------------------------

// ResponsesStream 把 OpenAI 流式 chunk 编码为 Responses API 事件流。
type ResponsesStream struct {
	w       io.Writer
	flush   func()
	model   string
	respID  string
	created int64

	seq         int
	started     bool
	finished    bool
	failed      bool // 已发 response.failed（终态）：收尾不得再补 response.completed
	outputIndex int
	output      []any
	usage       map[string]any
	stopRead    string

	// 文本 message item 状态
	msgID    string
	msgOpen  bool
	msgIndex int
	textBuf  strings.Builder

	// reasoning item 状态
	rsID    string
	rsOpen  bool
	rsIndex int
	rsBuf   strings.Builder

	// function_call item 状态
	fcOpen   bool
	fcID     string
	fcCallID string
	fcName   string
	fcIndex  int
	fcArgs   strings.Builder
	fcSeq    int // 当前 item 对应的 OpenAI tool_calls index
}

// NewResponsesStream 构建 Responses 事件编码器。
func NewResponsesStream(w io.Writer, flush func(), model string) *ResponsesStream {
	if model == "" {
		model = "unknown"
	}
	return &ResponsesStream{
		w: w, flush: flush, model: model,
		respID:  "resp_" + genID(),
		created: nowUnix(),
	}
}

// event 写一条 Responses SSE 事件（event: + data: 两行 + 空行）。
func (s *ResponsesStream) event(name string, payload map[string]any) error {
	payload["sequence_number"] = s.seq
	s.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}

// responseSnapshot 当前 response 对象（含已完成的 output 与 usage）。
func (s *ResponsesStream) responseSnapshot(status string) map[string]any {
	out := responsesBase(s.model, s.respID, s.created)
	out["status"] = status
	out["output"] = append([]any{}, s.output...)
	if s.usage != nil {
		out["usage"] = s.usage
	}
	return out
}

// ensureStart 惰性发 response.created / response.in_progress。
func (s *ResponsesStream) ensureStart() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := s.event("response.created", map[string]any{
		"type":     "response.created",
		"response": s.responseSnapshot("in_progress"),
	}); err != nil {
		return err
	}
	return s.event("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": s.responseSnapshot("in_progress"),
	})
}

// closeReasoning 收尾 reasoning item。
func (s *ResponsesStream) closeReasoning() error {
	if !s.rsOpen {
		return nil
	}
	s.rsOpen = false
	text := s.rsBuf.String()
	item := map[string]any{
		"type":    "reasoning",
		"id":      s.rsID,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	part := map[string]any{"type": "summary_text", "text": text}
	if err := s.event("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": s.rsID,
		"output_index": s.rsIndex, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := s.event("response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "item_id": s.rsID,
		"output_index": s.rsIndex, "summary_index": 0, "part": part,
	}); err != nil {
		return err
	}
	if err := s.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.rsIndex, "item": item,
	}); err != nil {
		return err
	}
	s.output = append(s.output, item)
	return nil
}

// closeMessage 收尾文本 message item。
func (s *ResponsesStream) closeMessage() error {
	if !s.msgOpen {
		return nil
	}
	s.msgOpen = false
	text := s.textBuf.String()
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	item := map[string]any{
		"type": "message", "id": s.msgID, "status": "completed",
		"role": "assistant", "content": []any{part},
	}
	if err := s.event("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": s.msgID,
		"output_index": s.msgIndex, "content_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := s.event("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": s.msgID,
		"output_index": s.msgIndex, "content_index": 0, "part": part,
	}); err != nil {
		return err
	}
	if err := s.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.msgIndex, "item": item,
	}); err != nil {
		return err
	}
	s.output = append(s.output, item)
	return nil
}

// closeFunc 收尾 function_call item。
//
// 参数完整性护栏（截断保护）：arguments 必须是**合法 JSON**。上游流被掐断时残留的是
// 残缺分片（如 `{"cmd":"ls`），一旦下发，Codex 会把它持久化进会话并在之后的每一次
// 请求里重放——上游校验工具记录时判「内容已损坏」（HTTP 400 code=11148，「直接重试
// 无效，请新建任务」），整条会话报废且无法自愈（网关只能靠剥离工具历史抢救）。
// 因此残缺调用**不下发**：不补 done 事件，该 item 在客户端侧始终未完成、不会进历史
// （客户端同时会收到断流 error 帧 / response.failed，整轮被丢弃）。
// 与聚合路径 dropTruncatedToolCalls 同哲学：宁可不给，也不给出会卡死会话的脏参数。
func (s *ResponsesStream) closeFunc() error {
	if !s.fcOpen {
		return nil
	}
	s.fcOpen = false
	args := pickStr(s.fcArgs.String(), "{}")
	if !json.Valid([]byte(args)) {
		return nil
	}
	item := map[string]any{
		"type": "function_call", "id": s.fcID, "call_id": s.fcCallID,
		"name": s.fcName, "arguments": args, "status": "completed",
	}
	if err := s.event("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": s.fcID,
		"output_index": s.fcIndex, "arguments": args,
	}); err != nil {
		return err
	}
	if err := s.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.fcIndex, "item": item,
	}); err != nil {
		return err
	}
	s.output = append(s.output, item)
	return nil
}

// openReasoning 开启 reasoning item。
func (s *ResponsesStream) openReasoning() error {
	if s.rsOpen {
		return nil
	}
	if err := s.closeMessage(); err != nil {
		return err
	}
	if err := s.closeFunc(); err != nil {
		return err
	}
	s.rsOpen = true
	s.rsIndex = s.outputIndex
	s.outputIndex++
	s.rsID = "rs_" + genID()
	s.rsBuf.Reset()
	if err := s.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.rsIndex,
		"item": map[string]any{"type": "reasoning", "id": s.rsID, "summary": []any{}},
	}); err != nil {
		return err
	}
	return s.event("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "item_id": s.rsID,
		"output_index": s.rsIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

// openMessage 开启文本 message item。
func (s *ResponsesStream) openMessage() error {
	if s.msgOpen {
		return nil
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.closeFunc(); err != nil {
		return err
	}
	s.msgOpen = true
	s.msgIndex = s.outputIndex
	s.outputIndex++
	s.msgID = "msg_" + genID()
	s.textBuf.Reset()
	if err := s.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.msgIndex,
		"item": map[string]any{
			"type": "message", "id": s.msgID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return s.event("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": s.msgID,
		"output_index": s.msgIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// openFunc 开启 function_call item。
func (s *ResponsesStream) openFunc(idx int, callID, name string) error {
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.closeMessage(); err != nil {
		return err
	}
	if err := s.closeFunc(); err != nil {
		return err
	}
	s.fcOpen = true
	s.fcSeq = idx
	s.fcIndex = s.outputIndex
	s.outputIndex++
	s.fcID = "fc_" + genID()
	s.fcCallID = pickStr(callID, "call_"+genID())
	s.fcName = pickStr(name, "unknown_tool")
	s.fcArgs.Reset()
	return s.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.fcIndex,
		"item": map[string]any{
			"type": "function_call", "id": s.fcID, "call_id": s.fcCallID,
			"name": s.fcName, "arguments": "", "status": "in_progress",
		},
	})
}

// Frame 处理一帧 OpenAI 流式 chunk。
func (s *ResponsesStream) Frame(chunk map[string]any) error {
	choice := openaiChoice(chunk)
	if choice == nil && chunk["usage"] == nil {
		return nil
	}
	if err := s.ensureStart(); err != nil {
		return err
	}
	if m := asString(chunk["model"]); m != "" && s.model == "unknown" {
		s.model = m
	}
	if u := responsesUsage(chunk["usage"]); u != nil {
		s.usage = u
	}
	if choice == nil {
		return nil
	}
	if fr := asString(choice["finish_reason"]); fr != "" {
		s.stopRead = fr
	}
	delta := asMap(choice["delta"])
	if delta == nil {
		return nil
	}
	if rc := asString(delta["reasoning_content"]); rc != "" {
		if err := s.openReasoning(); err != nil {
			return err
		}
		s.rsBuf.WriteString(rc)
		if err := s.event("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": s.rsID,
			"output_index": s.rsIndex, "summary_index": 0, "delta": rc,
		}); err != nil {
			return err
		}
	}
	if txt := asString(delta["content"]); txt != "" {
		if err := s.openMessage(); err != nil {
			return err
		}
		s.textBuf.WriteString(txt)
		if err := s.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": s.msgID,
			"output_index": s.msgIndex, "content_index": 0, "delta": txt,
		}); err != nil {
			return err
		}
	}
	for _, tcv := range asSlice(delta["tool_calls"]) {
		tc := asMap(tcv)
		if tc == nil {
			continue
		}
		idx, _ := asInt(tc["index"])
		fn := asMap(tc["function"])
		if !s.fcOpen || idx != s.fcSeq {
			if err := s.openFunc(idx, pickStr(asString(tc["id"]), s.fcCallID), asString(fn["name"])); err != nil {
				return err
			}
		} else {
			if v := asString(tc["id"]); v != "" {
				s.fcCallID = v
			}
			if name := asString(fn["name"]); name != "" {
				s.fcName = name
			}
		}
		if args := asString(fn["arguments"]); args != "" {
			s.fcArgs.WriteString(args)
			if err := s.event("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": s.fcID,
				"output_index": s.fcIndex, "delta": args,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ErrorFrame 处理 OpenAI error 帧 → response.failed 事件。
//
// response.failed 是 Responses 协议的**终态**事件：此后不得再发 response.completed。
// 否则客户端（Codex）会以"最后那个终态"为准，把失败读成"回答完成但输出为空"——
// 这正是 Codex 侧「有输入、没有返回」的来源：真正的原因（上游失败）被矛盾的终态淹没。
func (s *ResponsesStream) ErrorFrame(errObj map[string]any) error {
	if err := s.ensureStart(); err != nil {
		return err
	}
	s.failed = true
	msg := asString(errObj["message"])
	if msg == "" {
		msg = jsonString(errObj)
	}
	resp := s.responseSnapshot("failed")
	resp["error"] = map[string]any{
		"code":    pickStr(asString(errObj["code"]), "upstream_error"),
		"message": msg,
	}
	return s.event("response.failed", map[string]any{
		"type":     "response.failed",
		"response": resp,
	})
}

// Finish 收尾：关闭打开的 item + response.completed（幂等）。
// 已发过 response.failed 时直接结束：失败是终态，补 response.completed 会让客户端
// 把本次失败读成"完成但空输出"（详见 ErrorFrame 注释）。
func (s *ResponsesStream) Finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	if s.failed {
		return nil
	}
	if err := s.ensureStart(); err != nil {
		return err
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.closeMessage(); err != nil {
		return err
	}
	if err := s.closeFunc(); err != nil {
		return err
	}
	resp := s.responseSnapshot("completed")
	if fr := responsesIncompleteReason(s.stopRead); fr != "" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": fr}
	}
	return s.event("response.completed", map[string]any{
		"type":     "response.completed",
		"response": resp,
	})
}
