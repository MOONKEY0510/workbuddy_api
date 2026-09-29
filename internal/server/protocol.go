// protocol.go 多协议入口：Anthropic Messages（/v1/messages）、OpenAI Responses
// （/v1/responses，Codex CLI）与 Gemini generateContent（/v1beta/models/{model}:*）。
//
// 实现策略：先把入站协议请求体转成 OpenAI Chat Completions 请求体，复用既有
// chatCompletions（选号/轮转/降级/改写/观测全部沿用），用一个管道 ResponseWriter
// 捕获其输出（OpenAI JSON 或 SSE），再按客户端协议重新编码——既有转发管线零改动，
// 协议差异全部收敛在本文件与 internal/convert。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/convert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// protoStreamEncoder 目标协议流式编码器（internal/convert 三个流的共同接口）。
type protoStreamEncoder interface {
	Frame(chunk map[string]any) error
	ErrorFrame(errObj map[string]any) error
	Finish() error
}

// ---------------------------------------------------------------------------
// 鉴权：多协议凭证位置兼容
// ---------------------------------------------------------------------------

// protoToken 按协议约定提取调用凭证：
//   - Authorization: Bearer <key>（OpenAI / Responses / Anthropic 官方 SDK 也支持）
//   - x-api-key: <key>（Anthropic Messages，Claude Code 默认）
//   - x-goog-api-key: <key> 与 ?key=<key>（Gemini SDK / Gemini CLI）
func protoToken(r *http.Request) string {
	if tok := httpauth.BearerToken(r); tok != "" {
		return tok
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("x-goog-api-key")); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

// withAuthAny 同 withAuth，但凭证位置按多协议约定兼容（见 protoToken）。
// 校验口径与 withAuth 完全一致：主密钥或任一启用的托管密钥。
func (h *Handler) withAuthAny(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := protoToken(r)
		ok := false
		if master := h.loadLive().APIKey; master == "" || httpauth.VerifyToken(master, tok) {
			ok = true
		} else if h.cfg.Keys != nil {
			_, ok = h.cfg.Keys.Verify(tok)
		}
		if !ok {
			// 错误体按协议形状返回，客户端 SDK 才能正确识别鉴权失败。
			writeProtoAuthError(w, r)
			return
		}
		next(w, r)
	}
}

// writeProtoAuthError 按入站协议返回 401 错误体。
func writeProtoAuthError(w http.ResponseWriter, r *http.Request) {
	info := convert.ErrorInfo{Status: http.StatusUnauthorized, Message: "missing or invalid API key", Type: "authentication_error"}
	switch protoOf(r) {
	case convert.ProtoAnthropic:
		status, body := convert.AnthropicError(info)
		writeJSON(w, status, body)
	case convert.ProtoResponses:
		status, body := convert.ResponsesError(info)
		writeJSON(w, status, body)
	case convert.ProtoGemini:
		status, body := convert.GeminiError(info)
		writeJSON(w, status, body)
	default:
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
	}
}

// protoOf 按路径判断入站协议（错误编码用）。
func protoOf(r *http.Request) convert.Protocol {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v1/messages"):
		return convert.ProtoAnthropic
	case strings.HasPrefix(p, "/v1/responses"):
		return convert.ProtoResponses
	case strings.Contains(p, ":generateContent"), strings.Contains(p, ":streamGenerateContent"):
		return convert.ProtoGemini
	}
	return convert.ProtoOpenAI
}

// ---------------------------------------------------------------------------
// 管道：捕获 chatCompletions 的输出
// ---------------------------------------------------------------------------

// protoSink 是一个只写进 io.Pipe 的 ResponseWriter：chatCompletions 把它当成
// 真实客户端，实际由协议编码器消费后重新输出。Flush 为空操作（编码器自己控流）。
type protoSink struct {
	hdr    http.Header
	pw     *io.PipeWriter
	ready  chan struct{}
	once   sync.Once
	mu     sync.Mutex
	status int
}

func newProtoSink(pw *io.PipeWriter) *protoSink {
	return &protoSink{hdr: make(http.Header), pw: pw, ready: make(chan struct{})}
}

func (p *protoSink) Header() http.Header { return p.hdr }

func (p *protoSink) WriteHeader(code int) {
	p.mu.Lock()
	if p.status == 0 {
		p.status = code
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.ready) })
}

func (p *protoSink) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.ready) }) // 隐式 200：首个数据字节到达
	return p.pw.Write(b)
}

func (p *protoSink) Flush() {}

// Status 已提交的状态码（未显式 WriteHeader 并已开始写数据 → 200）。
func (p *protoSink) Status() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == 0 {
		return http.StatusOK
	}
	return p.status
}

// ContentType 捕获到的 Content-Type（流式判定）。
func (p *protoSink) ContentType() string { return p.hdr.Get("Content-Type") }

// pipelineParams 协议适配参数（各 handler 填自己的转换逻辑）。
type pipelineParams struct {
	// OpenAIBody 转换后的 OpenAI chat.completions 请求体。
	OpenAIBody []byte
	// Stream 客户端期望流式响应（决定等待/编码路径）。
	Stream bool
	// SSE 流式响应的 Content-Type 是否为 text/event-stream；false 走缓冲式
	// （Gemini 非 alt=sse 的 JSON 数组形态）。
	Encoder func(w io.Writer, flush func()) protoStreamEncoder
	// NonStream OpenAI 非流式响应 → 目标协议响应体。
	NonStream func(resp map[string]any) map[string]any
	// WrapError OpenAI 错误响应体 → （状态码，目标协议错误体）。
	WrapError func(status int, openaiBody []byte) (int, any)
}

// runPipeline 执行「转换后的 OpenAI 请求 → 复用 chatCompletions → 回编码」全流程。
func (h *Handler) runPipeline(w http.ResponseWriter, r *http.Request, p pipelineParams) {
	pr, pw := io.Pipe()
	sink := newProtoSink(pw)

	// clone 请求：替换 body 为 OpenAI 形态，其余头/上下文原样（X-Conversation-Request-ID
	// 等会话头族仍生效）。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(p.OpenAIBody))
	r2.ContentLength = int64(len(p.OpenAIBody))
	r2.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(p.OpenAIBody)), nil
	}

	go func() {
		defer pw.Close()
		h.chatCompletions(sink, r2)
	}()

	// 客户端断连看门狗：关掉读端，唤醒可能阻塞在读取（等待上游）或写入（管道满）
	// 的两侧，避免请求已死而 goroutine 悬挂到上游超时。
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-r.Context().Done():
			_ = pr.CloseWithError(r.Context().Err())
		case <-stopWatch:
		}
	}()

	select {
	case <-sink.ready:
	case <-r.Context().Done():
		_ = pr.CloseWithError(r.Context().Err())
		return
	}

	status := sink.Status()

	// 错误路径：读完整错误体 → 转成目标协议错误。
	if status != http.StatusOK {
		raw, _ := io.ReadAll(pr)
		_ = pr.Close()
		if p.WrapError != nil {
			ws, body := p.WrapError(status, raw)
			writeJSON(w, ws, body)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(raw)
		return
	}

	// 非流式（或上游未按 SSE 返回）：聚合后一次转换。
	if !p.Stream || !strings.Contains(sink.ContentType(), "text/event-stream") {
		raw, err := io.ReadAll(pr)
		_ = pr.Close()
		if err != nil {
			writeProtoError(w, r, http.StatusBadGateway, "upstream_parse", "read upstream response failed: "+err.Error())
			return
		}
		var resp map[string]any
		if err := json.Unmarshal(raw, &resp); err != nil {
			writeProtoError(w, r, http.StatusBadGateway, "upstream_parse", "invalid upstream response: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p.NonStream(resp))
		return
	}

	// 流式：逐帧读 OpenAI SSE，转成目标协议事件流实时写出。
	// Content-Type 若调用方未预设（Gemini 的 JSON 数组模式会预设 application/json），
	// 默认按 SSE 输出。
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	if fl, ok := w.(http.Flusher); ok {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
	}
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	enc := p.Encoder(w, flush)
	if err := relayOpenAIStream(pr, enc); err != nil {
		// 客户端断连 / 写失败：让上游 goroutine 立刻收到写错误终止，不泄漏。
		_ = pr.CloseWithError(err)
		return
	}
	_ = pr.Close()
	_ = enc.Finish()
	flush()
}

// relayOpenAIStream 读 OpenAI SSE（chatCompletions 输出）并逐帧交给编码器。
// [DONE] 只结束读取，不转发（各协议有自己的收尾事件）。
func relayOpenAIStream(r io.Reader, enc protoStreamEncoder) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				return nil
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) == nil {
				if e, ok := obj["error"].(map[string]any); ok {
					if werr := enc.ErrorFrame(e); werr != nil {
						return werr
					}
				} else if werr := enc.Frame(obj); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// wrapErrorFunc 生成「OpenAI 错误体 → 目标协议错误体」的转换闭包。
func wrapErrorFunc(proto convert.Protocol) func(int, []byte) (int, any) {
	return func(status int, raw []byte) (int, any) {
		info := convert.OpenAIErrorInfo(status, raw)
		switch proto {
		case convert.ProtoAnthropic:
			return convert.AnthropicError(info)
		case convert.ProtoResponses:
			return convert.ResponsesError(info)
		case convert.ProtoGemini:
			return convert.GeminiError(info)
		}
		return status, map[string]any{"error": map[string]any{"message": info.Message, "type": info.Type, "code": info.Code}}
	}
}

// readProtoBody 读入请求体并施加 server.max_body_mb 上限（与 chatCompletions 同口径）。
// 返回 false 表示已写错误响应，调用方直接返回。
func (h *Handler) readProtoBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limit := h.maxBodyBytes()
	if limit > 0 && r.ContentLength > limit {
		writeProtoError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("request body exceeds gateway limit of %d MB (server.max_body_mb; 0 = unlimited)", limit>>20))
		return nil, false
	}
	reader := io.Reader(r.Body)
	if limit > 0 {
		reader = io.LimitReader(r.Body, limit+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		writeProtoError(w, r, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return nil, false
	}
	if limit > 0 && int64(len(body)) > limit {
		writeProtoError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("request body exceeds gateway limit of %d MB (server.max_body_mb; 0 = unlimited)", limit>>20))
		return nil, false
	}
	return body, true
}

// writeProtoError 按协议形状写一个本地错误（转换失败等网关侧错误）。
func writeProtoError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	info := convert.ErrorInfo{Status: status, Code: code, Message: msg}
	switch protoOf(r) {
	case convert.ProtoAnthropic:
		s, body := convert.AnthropicError(info)
		writeJSON(w, s, body)
	case convert.ProtoResponses:
		s, body := convert.ResponsesError(info)
		writeJSON(w, s, body)
	case convert.ProtoGemini:
		s, body := convert.GeminiError(info)
		writeJSON(w, s, body)
	default:
		writeOpenAIErrorHint(w, status, code, msg, "")
	}
}

// ---------------------------------------------------------------------------
// Anthropic Messages
// ---------------------------------------------------------------------------

// messagesAnthropic 处理 POST /v1/messages（Claude Code / anthropic-sdk）。
func (h *Handler) messagesAnthropic(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readProtoBody(w, r)
	if !ok {
		return
	}
	oaiBody, stream, err := convert.AnthropicToOpenAI(body)
	if err != nil {
		writeProtoError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	model := peek.Model

	h.runPipeline(w, r, pipelineParams{
		OpenAIBody: oaiBody,
		Stream:     stream,
		Encoder: func(w io.Writer, flush func()) protoStreamEncoder {
			return convert.NewAnthropicStream(w, flush, model)
		},
		NonStream: func(resp map[string]any) map[string]any {
			return convert.AnthropicResponse(resp, model)
		},
		WrapError: wrapErrorFunc(convert.ProtoAnthropic),
	})
}

// countTokensAnthropic 处理 POST /v1/messages/count_tokens：Claude Code 用它做
// 上下文预算。网关不做真实分词（上游无对应接口），按字符数 /4 粗略估算——
// 只影响客户端「还剩多少上下文」的提示精度，不影响请求正确性。
func (h *Handler) countTokensAnthropic(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readProtoBody(w, r)
	if !ok {
		return
	}
	oaiBody, _, err := convert.AnthropicToOpenAI(body)
	if err != nil {
		writeProtoError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var oai map[string]any
	_ = json.Unmarshal(oaiBody, &oai)
	chars := 0
	for _, m := range asAnySlice(oai["messages"]) {
		mm, _ := m.(map[string]any)
		chars += len(fmt.Sprint(mm["content"]))
		if tc, ok := mm["tool_calls"]; ok {
			chars += len(fmt.Sprint(tc))
		}
	}
	if t, ok := oai["tools"]; ok {
		chars += len(fmt.Sprint(t))
	}
	tokens := chars / 4
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// asAnySlice 断言 []any（nil 安全）。
func asAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// ---------------------------------------------------------------------------
// OpenAI Responses（Codex CLI）
// ---------------------------------------------------------------------------

// responsesCodex 处理 POST /v1/responses。
func (h *Handler) responsesCodex(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readProtoBody(w, r)
	if !ok {
		return
	}
	oaiBody, stream, err := convert.ResponsesToOpenAI(body)
	if err != nil {
		writeProtoError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	model := peek.Model

	h.runPipeline(w, r, pipelineParams{
		OpenAIBody: oaiBody,
		Stream:     stream,
		Encoder: func(w io.Writer, flush func()) protoStreamEncoder {
			return convert.NewResponsesStream(w, flush, model)
		},
		NonStream: func(resp map[string]any) map[string]any {
			return convert.ResponsesResponse(resp, model)
		},
		WrapError: wrapErrorFunc(convert.ProtoResponses),
	})
}

// ---------------------------------------------------------------------------
// Gemini generateContent
// ---------------------------------------------------------------------------

// geminiGenerate 处理 POST /v1beta/models/{model}:generateContent 与
// POST /v1beta/models/{model}:streamGenerateContent（Google SDK / Gemini CLI）。
func (h *Handler) geminiGenerate(w http.ResponseWriter, r *http.Request) {
	pathModel := r.PathValue("model")
	name, method := splitGeminiPath(pathModel)
	stream := false
	switch method {
	case "generateContent":
	case "streamGenerateContent":
		stream = true
	default:
		writeProtoError(w, r, http.StatusNotFound, "not_found",
			fmt.Sprintf("unsupported method %q (want generateContent or streamGenerateContent)", method))
		return
	}
	if name == "" {
		writeProtoError(w, r, http.StatusBadRequest, "invalid_request", "model name is required")
		return
	}
	body, ok := h.readProtoBody(w, r)
	if !ok {
		return
	}
	oaiBody, err := convert.GeminiToOpenAI(body, name, stream)
	if err != nil {
		writeProtoError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 流式输出形态跟随 alt：alt=sse（或 Accept: text/event-stream）走 SSE，
	// 默认 alt=json 走缓冲式 JSON 数组（Gemini REST 的默认形态）。
	sse := geminiWantSSE(r)
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	h.runPipeline(w, r, pipelineParams{
		OpenAIBody: oaiBody,
		Stream:     stream,
		Encoder: func(w io.Writer, flush func()) protoStreamEncoder {
			return convert.NewGeminiStream(w, flush, name, sse)
		},
		NonStream: func(resp map[string]any) map[string]any {
			return convert.GeminiResponse(resp, name)
		},
		WrapError: wrapErrorFunc(convert.ProtoGemini),
	})
}

// splitGeminiPath 拆分 "gemini-2.5-pro:streamGenerateContent" 为模型名与方法名。
// 无冒号时 method 为空（由调用方判 404）。
func splitGeminiPath(s string) (name, method string) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// geminiWantSSE 判定 Gemini 流式输出是否用 SSE（alt=sse 或 Accept 头）。
func geminiWantSSE(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("alt"), "sse") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}
