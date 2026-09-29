// Package convert 把 Anthropic Messages、OpenAI Responses（Codex）与 Gemini
// generateContent 三种入站协议与网关内部统一使用的 OpenAI Chat Completions
// 协议互转——客户端用任意一种协议发来，网关都转成 OpenAI 格式交给既有
// 转发管线（选号/轮转/改写/观测），再把响应按原协议形状回给客户端。
//
// 设计约束：
//   - 零外部依赖（只用标准库），与项目其余部分的依赖面一致；
//   - 请求方向「尽力转」：不认识的块一律跳过而不是报 400（客户端形态差异大，
//     因为一个冷门字段把整个请求打回不值得）；只有连「模型名/messages 结构」
//     都不成立时才报错；
//   - 响应方向「严格按官方事件序列」：流式事件名/顺序/字段名对齐官方规范，
//     官方 SDK（anthropic-sdk / openai-sdk / google-genai）与 CLI
//     （Claude Code / Codex CLI / Gemini CLI）才能零改造解析。
package convert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Protocol 入站协议枚举。
type Protocol string

const (
	// ProtoOpenAI 原生 OpenAI Chat Completions（/v1/chat/completions）。
	ProtoOpenAI Protocol = "openai"
	// ProtoAnthropic Anthropic Messages（/v1/messages，Claude Code / anthropic-sdk）。
	ProtoAnthropic Protocol = "anthropic"
	// ProtoResponses OpenAI Responses（/v1/responses，Codex CLI）。
	ProtoResponses Protocol = "responses"
	// ProtoGemini Gemini generateContent（/v1beta/models/{model}:generateContent）。
	ProtoGemini Protocol = "gemini"
)

// genID 生成短随机 ID 后缀（hex）。加前缀后形如 msg_9f2c…，与官方 ID 形状一致，
// 避免客户端对 ID 前缀做正则匹配时拒绝。
func genID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// nowUnix 当前秒级时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// asMap 把任意 JSON 值断言为 map（非对象 → nil）。
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// asSlice 把任意 JSON 值断言为数组（非数组 → nil）。
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// asString 把任意 JSON 值断言为字符串（非字符串 → ""）。
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asFloat 把 JSON number 归一为 float64（int/int64/float64 均可），供跨协议数值搬运。
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// asInt 取整数值（缺失/非数值 → 0,false）。
func asInt(v any) (int, bool) {
	f, ok := asFloat(v)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// jsonString 把任意值序列化成紧凑 JSON 字符串；失败返回 ""。
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// truncate 截断字符串到 max 字节（超长参数/文本进日志或占位时用）。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// numToIntJSON 把 float64 写成 int64（JSON 序列化时避免 1.28e5 科学计数法）；
// 非整数保持原值。
func numToIntJSON(f float64) any {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return int64(f)
	}
	return f
}

// ---------------------------------------------------------------------------
// 响应方向公共：从 OpenAI 响应/流中提取信息的工具
// ---------------------------------------------------------------------------

// openaiChoice 取 OpenAI 响应/流帧的首个 choice（无 choices → nil）。
func openaiChoice(obj map[string]any) map[string]any {
	chs := asSlice(obj["choices"])
	if len(chs) == 0 {
		return nil
	}
	return asMap(chs[0])
}

// usageFromOpenAI 从 OpenAI usage 对象提取各口径 token 数（缺失字段 ok=false）。
type usageTokens struct {
	Prompt        int
	HasPrompt     bool
	Completion    int
	HasCompletion bool
	Cached        int
	HasCached     bool
	CacheCreation int
	HasCacheWrite bool
	Reasoning     int
	HasReasoning  bool
	Total         int
	HasTotal      bool
}

// parseUsage 解析 OpenAI usage（兼容 anthropic 口径别名 cache_read_input_tokens 等）。
func parseUsage(v any) usageTokens {
	var u usageTokens
	m := asMap(v)
	if m == nil {
		return u
	}
	if n, ok := asInt(m["prompt_tokens"]); ok {
		u.Prompt, u.HasPrompt = n, true
	}
	if n, ok := asInt(m["completion_tokens"]); ok {
		u.Completion, u.HasCompletion = n, true
	}
	if n, ok := asInt(m["total_tokens"]); ok {
		u.Total, u.HasTotal = n, true
	}
	if d := asMap(m["prompt_tokens_details"]); d != nil {
		if n, ok := asInt(d["cached_tokens"]); ok {
			u.Cached, u.HasCached = n, true
		}
	}
	if d := asMap(m["completion_tokens_details"]); d != nil {
		if n, ok := asInt(d["reasoning_tokens"]); ok {
			u.Reasoning, u.HasReasoning = n, true
		}
	}
	// Anthropic 口径兜底（网关 usage 归一化会保留这些别名）
	if !u.HasCached {
		if n, ok := asInt(m["cache_read_input_tokens"]); ok {
			u.Cached, u.HasCached = n, true
		}
	}
	if n, ok := asInt(m["cache_creation_input_tokens"]); ok {
		u.CacheCreation, u.HasCacheWrite = n, true
	}
	return u
}

// ---------------------------------------------------------------------------
// 错误方向公共：OpenAI 错误 → 各协议错误对象
// ---------------------------------------------------------------------------

// ErrorInfo 协议无关的错误描述。
type ErrorInfo struct {
	Status  int    // 建议的 HTTP 状态码
	Message string // 人类可读信息（透传上游原文）
	Code    string // 机器可读码（网关/上游 code）
	Type    string // OpenAI error.type 或协议内分类
}

// OpenAIErrorInfo 从 OpenAI 错误响应体提取 ErrorInfo（HTTP 状态由调用方给出）。
func OpenAIErrorInfo(status int, body []byte) ErrorInfo {
	info := ErrorInfo{Status: status, Message: strings.TrimSpace(string(body))}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return info
	}
	e := asMap(root["error"])
	if e == nil {
		// 非标准信封：字段提升一层（部分上游直接给 {code,message}）
		e = root
	}
	if s := asString(e["message"]); s != "" {
		info.Message = s
	}
	if s := asString(e["code"]); s != "" {
		info.Code = s
	}
	if s := asString(e["type"]); s != "" {
		info.Type = s
	}
	return info
}

// AnthropicError 把 ErrorInfo 转成 Anthropic 错误响应体与状态码。
// Anthropic 用 529 表示过载（对应网关的「无可用账号/上游不可用」）。
func AnthropicError(info ErrorInfo) (int, map[string]any) {
	status := info.Status
	typ := info.Type
	switch {
	case status == 0:
		status = 500
	case status == 404:
		typ = pickStr(typ, "not_found_error")
	case status == 413:
		typ = pickStr(typ, "request_too_large")
	case status == 429:
		typ = pickStr(typ, "rate_limit_error")
	case status == 400 || status == 422:
		typ = pickStr(typ, "invalid_request_error")
	case status == 401 || status == 403:
		typ = pickStr(typ, "authentication_error")
	case status >= 500:
		// 502/503/504 统一按 Anthropic 的「过载」语义回 529：客户端（Claude Code）
		// 对 529 会退避重试，对 500 会当硬错误直接暴露给用户。
		typ = pickStr(typ, "overloaded_error")
		status = 529
	default:
		typ = pickStr(typ, "api_error")
	}
	msg := info.Message
	if msg == "" {
		msg = "gateway error"
	}
	return status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	}
}

// ResponsesError 把 ErrorInfo 转成 Responses API 错误响应体与状态码。
func ResponsesError(info ErrorInfo) (int, map[string]any) {
	status := info.Status
	if status == 0 {
		status = 500
	}
	typ := info.Type
	if typ == "" {
		switch {
		case status == 400 || status == 422:
			typ = "invalid_request_error"
		case status == 401:
			typ = "authentication_error"
		case status == 404:
			typ = "not_found_error"
		case status == 429:
			typ = "rate_limit_error"
		case status >= 500:
			typ = "server_error"
		default:
			typ = "invalid_request_error"
		}
	}
	errObj := map[string]any{
		"message": pickStr(info.Message, "gateway error"),
		"type":    typ,
	}
	if info.Code != "" {
		errObj["code"] = info.Code
	}
	return status, map[string]any{"error": errObj}
}

// ErrorBody 按目标协议构造错误响应体（网关侧本地错误用，如转换失败/读体失败）。
// 返回 HTTP 状态码与响应体。
func ErrorBody(proto Protocol, status int, code, msg string) (int, map[string]any) {
	info := ErrorInfo{Status: status, Code: code, Message: msg}
	switch proto {
	case ProtoAnthropic:
		return AnthropicError(info)
	case ProtoResponses:
		return ResponsesError(info)
	case ProtoGemini:
		return GeminiError(info)
	}
	return status, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code}}
}

// GeminiError 把 ErrorInfo 转成 Google API 错误响应体与状态码。
func GeminiError(info ErrorInfo) (int, map[string]any) {
	status := info.Status
	if status == 0 {
		status = 500
	}
	msg := pickStr(info.Message, "gateway error")
	return status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": msg,
			"status":  geminiStatusName(status),
		},
	}
}

// geminiStatusName HTTP 状态 → google.rpc.Code 名称。
func geminiStatusName(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 408:
		return "DEADLINE_EXCEEDED"
	case 409:
		return "ABORTED"
	case 413:
		return "INVALID_ARGUMENT"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 499:
		return "CANCELLED"
	case 500:
		return "INTERNAL"
	case 501:
		return "UNIMPLEMENTED"
	case 503:
		return "UNAVAILABLE"
	case 504:
		return "DEADLINE_EXCEEDED"
	default:
		if status >= 500 {
			return "INTERNAL"
		}
		return "UNKNOWN"
	}
}

// pickStr 返回第一个非空值（都空时返回空）。
func pickStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
