// anthropic.go 实现 Anthropic Messages API（/v1/messages）与 OpenAI Chat
// Completions 的互转：请求（system/messages/tools/tool_choice → OpenAI）与
// 响应（OpenAI completion/delta → Anthropic Message 与官方事件流）。
package convert

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// 请求方向：Anthropic → OpenAI
// ---------------------------------------------------------------------------

// anthropicReq Anthropic /v1/messages 请求体（字段用 json.RawMessage 承接多变体：
// system 与 content 既可能是字符串也可能是块数组）。
type anthropicReq struct {
	Model         string            `json:"model"`
	MaxTokens     json.RawMessage   `json:"max_tokens"`
	System        json.RawMessage   `json:"system"`
	Messages      []json.RawMessage `json:"messages"`
	Tools         []json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
	Temperature   json.RawMessage   `json:"temperature"`
	TopP          json.RawMessage   `json:"top_p"`
	StopSequences []string          `json:"stop_sequences"`
	Stream        bool              `json:"stream"`
	Metadata      map[string]any    `json:"metadata"`
}

// AnthropicToOpenAI 把 Anthropic Messages 请求体转换为 OpenAI chat.completions
// 请求体。返回 stream 供上层判断走流式还是非流式分支。
func AnthropicToOpenAI(body []byte) ([]byte, bool, error) {
	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("invalid anthropic request: %w", err)
	}
	if len(req.Messages) == 0 {
		return nil, false, fmt.Errorf("invalid anthropic request: messages is empty")
	}
	out := map[string]any{"model": req.Model, "stream": req.Stream}
	msgs := make([]any, 0, len(req.Messages)+1)

	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, raw := range req.Messages {
		msgs = append(msgs, anthropicMessageToOpenAI(raw)...)
	}
	out["messages"] = msgs

	if n, ok := asInt(rawValue(req.MaxTokens)); ok && n > 0 {
		out["max_tokens"] = n
	}
	if v := rawValue(req.Temperature); v != nil {
		out["temperature"] = v
	}
	if v := rawValue(req.TopP); v != nil {
		out["top_p"] = v
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if tools := anthropicToolsToOpenAI(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := anthropicToolChoiceToOpenAI(rawValue(req.ToolChoice)); tc != nil {
		out["tool_choice"] = tc
	}
	// metadata.user_id → user（弱等价的会话归因字段）。
	if req.Metadata != nil {
		if uid := asString(req.Metadata["user_id"]); uid != "" {
			out["user"] = uid
		}
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return buf, req.Stream, nil
}

// rawValue 解析 json.RawMessage（空/非法 → nil）。
func rawValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// anthropicSystemText system 字段归一为纯文本（string 或 [{type:text,text:…}]）。
func anthropicSystemText(raw json.RawMessage) string {
	switch t := rawValue(raw).(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			m := asMap(item)
			if m == nil || asString(m["type"]) != "text" {
				continue
			}
			if txt := asString(m["text"]); txt != "" {
				sb.WriteString(txt)
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicMessageToOpenAI 把一条 Anthropic 消息转成 0..N 条 OpenAI 消息。
// tool_result 块会被拆成独立的 tool 角色消息（OpenAI 的 tool 结果必须独立成条，
// 且要排在对应 assistant tool_calls 之后——Anthropic 里 tool_result 天然在
// 下一条 user 消息头部，拆分顺序保持即符合 OpenAI 配对要求）。
func anthropicMessageToOpenAI(raw json.RawMessage) []any {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	role := asString(m["role"])
	if role == "" {
		role = "user"
	}
	// content 为纯字符串：一对一映射。
	if s, ok := m["content"].(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	parts := asSlice(m["content"])
	if parts == nil {
		return nil
	}

	var out []any
	var rich []any // 富内容（含图片）
	var plain strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	hasImage := false

	flushUser := func() {
		if hasImage {
			if plain.Len() > 0 {
				rich = append([]any{map[string]any{"type": "text", "text": plain.String()}}, rich...)
				plain.Reset()
			}
			out = append(out, map[string]any{"role": role, "content": rich})
			rich, hasImage = nil, false
			return
		}
		if plain.Len() > 0 {
			out = append(out, map[string]any{"role": role, "content": plain.String()})
			plain.Reset()
		}
	}

	for _, p := range parts {
		blk := asMap(p)
		if blk == nil {
			continue
		}
		switch asString(blk["type"]) {
		case "text":
			if txt := asString(blk["text"]); txt != "" {
				plain.WriteString(txt)
			}
		case "image":
			if url := anthropicImageURL(asMap(blk["source"])); url != "" {
				hasImage = true
				rich = append(rich, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "tool_result":
			if role != "assistant" {
				flushUser() // tool_result 之前的 user 内容先收口，保证 tool 消息紧随 assistant
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": asString(blk["tool_use_id"]),
				"content":      anthropicToolResultText(blk["content"]),
			})
		case "tool_use":
			if role != "assistant" {
				continue
			}
			args := "{}"
			if input, ok := blk["input"]; ok && input != nil {
				if s := jsonString(input); s != "" {
					args = s
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   asString(blk["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      asString(blk["name"]),
					"arguments": args,
				},
			})
		case "thinking", "redacted_thinking":
			// 思维链回填为 reasoning_content（与 payload.go 的 backfillReasoningContent 同口径）。
			if txt := asString(blk["thinking"]); txt != "" {
				reasoning.WriteString(txt)
			}
		}
	}
	// assistant 的文本与 tool_calls 必须合并进同一条消息（OpenAI 的 tool_calls
	// 挂在 assistant 消息上）；user 侧才需要把累积内容单独成条。
	if role == "assistant" {
		msg := map[string]any{"role": "assistant", "content": plain.String()}
		if hasImage {
			msg["content"] = rich
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		if reasoning.Len() > 0 {
			msg["reasoning_content"] = reasoning.String()
		}
		if len(toolCalls) > 0 || plain.Len() > 0 || hasImage || reasoning.Len() > 0 {
			out = append(out, msg)
		}
		return out
	}
	flushUser()
	// user 侧：有 tool_result 拆出的消息但没有其他内容时，out 里已有 tool 消息。
	return out
}

// anthropicImageURL 把 Anthropic image source 转成可放进 image_url.url 的字符串。
func anthropicImageURL(src map[string]any) string {
	if src == nil {
		return ""
	}
	switch asString(src["type"]) {
	case "base64":
		data := asString(src["data"])
		if data == "" {
			return ""
		}
		return "data:" + pickStr(asString(src["media_type"]), "image/png") + ";base64," + data
	case "url":
		return asString(src["url"])
	}
	return ""
}

// anthropicToolResultText tool_result.content 归一为文本（string 或块数组）。
func anthropicToolResultText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			m := asMap(item)
			if m == nil || asString(m["type"]) != "text" {
				continue
			}
			if txt := asString(m["text"]); txt != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(txt)
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicToolsToOpenAI Anthropic tools → OpenAI tools。
func anthropicToolsToOpenAI(tools []json.RawMessage) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		var t map[string]any
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		name := asString(t["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d := asString(t["description"]); d != "" {
			fn["description"] = d
		}
		if params, ok := t["input_schema"]; ok && params != nil {
			fn["parameters"] = params
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// anthropicToolChoiceToOpenAI Anthropic tool_choice → OpenAI tool_choice。
func anthropicToolChoiceToOpenAI(v any) any {
	m := asMap(v)
	if m == nil {
		return nil
	}
	switch asString(m["type"]) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name := asString(m["name"]); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return "auto"
	}
	return nil
}

// ---------------------------------------------------------------------------
// 响应方向（非流式）：OpenAI completion → Anthropic Message
// ---------------------------------------------------------------------------

// AnthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func AnthropicStopReason(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// AnthropicResponse 把 OpenAI 非流式 completion 响应转换为 Anthropic Message。
func AnthropicResponse(resp map[string]any, model string) map[string]any {
	choice := openaiChoice(resp)
	msg := asMap(choice["message"])
	content := make([]any, 0, 3)

	if rc := asString(msg["reasoning_content"]); rc != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
	}
	if txt := openaiMessageText(msg["content"]); txt != "" {
		content = append(content, map[string]any{"type": "text", "text": txt})
	}
	for _, tc := range asSlice(msg["tool_calls"]) {
		call := asMap(tc)
		fn := asMap(call["function"])
		if fn == nil {
			continue
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    pickStr(asString(call["id"]), "toolu_"+genID()),
			"name":  asString(fn["name"]),
			"input": parseToolArguments(asString(fn["arguments"])),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	u := parseUsage(resp["usage"])
	usage := map[string]any{"input_tokens": u.Prompt, "output_tokens": u.Completion}
	if u.HasCached {
		usage["cache_read_input_tokens"] = u.Cached
	}
	if u.HasCacheWrite {
		usage["cache_creation_input_tokens"] = u.CacheCreation
	}
	if m := asString(resp["model"]); m != "" {
		model = m
	}
	if model == "" {
		model = "unknown"
	}
	return map[string]any{
		"id":            anthropicMessageID(asString(resp["id"])),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   AnthropicStopReason(asString(choice["finish_reason"])),
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// anthropicMessageID 把 OpenAI 的 chatcmpl-xxx 归一到 msg_ 前缀（保留可追溯后缀）。
func anthropicMessageID(id string) string {
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	if id == "" {
		return "msg_" + genID()
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}

// openaiMessageText 取 OpenAI message.content 文本（字符串或富数组里的 text 块）。
func openaiMessageText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			m := asMap(item)
			if m != nil && asString(m["type"]) == "text" {
				sb.WriteString(asString(m["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// parseToolArguments 把 tool_call.function.arguments（JSON 字符串）解析为对象；
// 解析失败返回空对象（Anthropic 的 input 必须是对象，不能透传裸字符串）。
func parseToolArguments(args string) map[string]any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var v any
	if json.Unmarshal([]byte(args), &v) != nil {
		return map[string]any{}
	}
	if m := asMap(v); m != nil {
		return m
	}
	// 非对象（数组/标量）：包一层，保留原始值不丢信息。
	return map[string]any{"value": v}
}

// ---------------------------------------------------------------------------
// 响应方向（流式）：OpenAI SSE 帧 → Anthropic 事件流
// ---------------------------------------------------------------------------

// AnthropicStream 把 OpenAI 流式 chunk 编码为 Anthropic Messages 事件流。
// 事件序列（官方规范）：
//
//	message_start → (content_block_start → content_block_delta* → content_block_stop)*
//	→ message_delta → message_stop
type AnthropicStream struct {
	w     io.Writer
	flush func()
	model string
	msgID string

	inTok      int
	outTok     int
	cacheRead  int
	cacheWrite int
	hasCache   bool

	started  bool
	finished bool
	failed   bool   // 已发 error 事件（终态）：收尾不得再补 message_delta/message_stop
	curKind  string // "" / "text" / "thinking" / "tool"
	curIndex int    // Anthropic content block index（从 0 起递增分配）
	curTool  int    // 当前 tool 块对应的 OpenAI tool_calls index
	toolID   string
	toolName string
	stopRead string // 最近一次 finish_reason
}

// NewAnthropicStream 构建编码器；flush 可为 nil。
func NewAnthropicStream(w io.Writer, flush func(), model string) *AnthropicStream {
	if model == "" {
		model = "unknown"
	}
	// curIndex 从 -1 起：首个块自增后为 0（Anthropic 官方 block index 从 0 开始）。
	return &AnthropicStream{w: w, flush: flush, model: model, msgID: "msg_" + genID(), curIndex: -1}
}

// event 写一条 Anthropic SSE 事件（event: + data: 两行 + 空行）。
func (s *AnthropicStream) event(name string, payload map[string]any) error {
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

// ensureStart 惰性发 message_start（首帧到来才发，空流也能收尾出合法消息）。
func (s *AnthropicStream) ensureStart() error {
	if s.started {
		return nil
	}
	s.started = true
	return s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// closeBlock 发 content_block_stop（无打开的块时为空操作）。
func (s *AnthropicStream) closeBlock() error {
	if s.curKind == "" {
		return nil
	}
	idx := s.curIndex
	s.curKind = ""
	return s.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

// openTextBlock 打开文本/思维块（同类型重复调用为空操作）。
func (s *AnthropicStream) openTextBlock(kind string) error {
	if s.curKind == kind {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.curKind = kind
	s.curIndex++
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": ""}
	}
	return s.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.curIndex,
		"content_block": block,
	})
}

// openToolBlock 打开一个 tool_use 块（登记 id/name 后发出 content_block_start）。
func (s *AnthropicStream) openToolBlock(idx int) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.curKind = "tool"
	s.curTool = idx
	s.curIndex++
	return s.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.curIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    pickStr(s.toolID, "toolu_"+genID()),
			"name":  pickStr(s.toolName, "unknown_tool"),
			"input": map[string]any{},
		},
	})
}

// delta 发一条 content_block_delta（作用于当前打开的块）。
func (s *AnthropicStream) delta(delta map[string]any) error {
	return s.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.curIndex,
		"delta": delta,
	})
}

// Frame 处理一帧 OpenAI 流式 chunk。
func (s *AnthropicStream) Frame(chunk map[string]any) error {
	choice := openaiChoice(chunk)
	if choice == nil && chunk["usage"] == nil {
		return nil // 非数据帧（如上游的纯元数据帧）：忽略
	}
	if err := s.ensureStart(); err != nil {
		return err
	}
	if m := asString(chunk["model"]); m != "" && s.model == "unknown" {
		s.model = m
	}
	if u := parseUsage(chunk["usage"]); u.HasPrompt || u.HasCompletion || u.HasCached {
		if u.HasPrompt {
			s.inTok = u.Prompt
		}
		if u.HasCompletion {
			s.outTok = u.Completion
		}
		if u.HasCached || u.HasCacheWrite {
			s.cacheRead, s.cacheWrite, s.hasCache = u.Cached, u.CacheCreation, true
		}
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
	// 思维链在文本之前（与官方 Claude 流同序）。
	if rc := asString(delta["reasoning_content"]); rc != "" {
		if err := s.openTextBlock("thinking"); err != nil {
			return err
		}
		if err := s.delta(map[string]any{"type": "thinking_delta", "thinking": rc}); err != nil {
			return err
		}
	}
	if txt := asString(delta["content"]); txt != "" {
		if err := s.openTextBlock("text"); err != nil {
			return err
		}
		if err := s.delta(map[string]any{"type": "text_delta", "text": txt}); err != nil {
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
		if s.curKind != "tool" || idx != s.curTool {
			// 新 index：登记身份后开新块。
			s.toolID = pickStr(asString(tc["id"]), s.toolID)
			if name := asString(fn["name"]); name != "" {
				s.toolName = name
			}
			if err := s.openToolBlock(idx); err != nil {
				return err
			}
		} else {
			// 同一 index 的后续分片：id/name 直覆盖（通常缺省，缺省时保持首片值）。
			if v := asString(tc["id"]); v != "" {
				s.toolID = v
			}
			if name := asString(fn["name"]); name != "" {
				s.toolName = name
			}
		}
		if args := asString(fn["arguments"]); args != "" {
			if err := s.delta(map[string]any{"type": "input_json_delta", "partial_json": args}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ErrorFrame 处理 OpenAI error 帧（error-passthrough）：转成 Anthropic error 事件。
//
// Anthropic 协议里 error 事件是流的**终态**：其后不得再发 message_delta/message_stop，
// 否则客户端（Claude Code / anthropic-sdk）可能把失败读成"回答完成但内容为空"。
func (s *AnthropicStream) ErrorFrame(errObj map[string]any) error {
	s.failed = true
	msg := asString(errObj["message"])
	if msg == "" {
		msg = jsonString(errObj)
	}
	typ := "api_error"
	if c := strings.ToLower(asString(errObj["code"]) + asString(errObj["type"])); strings.Contains(c, "rate") {
		typ = "rate_limit_error"
	}
	return s.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// Finish 收尾：关闭打开的块 + message_delta + message_stop（幂等）。
// 已发过 error 事件（终态）时直接结束——补收尾事件会让客户端把失败读成
// "回答完成但内容为空"（详见 ErrorFrame 注释）。
func (s *AnthropicStream) Finish() error {
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
	if err := s.closeBlock(); err != nil {
		return err
	}
	usage := map[string]any{"output_tokens": s.outTok}
	if s.inTok > 0 {
		usage["input_tokens"] = s.inTok
	}
	if s.hasCache {
		usage["cache_read_input_tokens"] = s.cacheRead
		usage["cache_creation_input_tokens"] = s.cacheWrite
	}
	if err := s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": AnthropicStopReason(s.stopRead), "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{"type": "message_stop"})
}
