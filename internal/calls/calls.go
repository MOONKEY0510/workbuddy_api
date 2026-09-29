// Package calls 保存**逐请求**调用记录，供面板「调用记录」视图查看。
//
// 与 internal/usage 的分工：
//   - usage 是聚合台账（按 时间片×域×账号×模型 分桶累计），回答"今天用了多少"；
//   - 本包是明细流水（每次请求一行），回答"刚才那次请求到底发生了什么"——
//     哪个号、哪个模型、多少 token、命中多少缓存、扣了多少积分、耗时多久、成没成。
//
// 容量有界（环形缓冲，默认 300 条），纯内存、不落盘：这是排障视图，不是审计日志。
package calls

import (
	"sync"
	"time"
)

// Entry 一次账号尝试的明细（与 usage.Delta 同源，在 recordAttempt 汇聚点写入）。
type Entry struct {
	Seq    int64  `json:"seq"`              // 单调递增，前端据此判断新旧
	T      string `json:"t"`                // RFC3339（本地时区）
	UID    string `json:"uid"`              // 脱敏由面板侧做，这里保留全 uid 便于对照日志
	Nick   string `json:"nick,omitempty"`   // 昵称（无则前端退回 uid 前 8 位）
	Realm  string `json:"realm"`            // cn / global
	Model  string `json:"model"`            // 裸模型名
	OK     bool   `json:"ok"`               // 是否拿到 usage（与用量视图同口径）
	Status int    `json:"status,omitempty"` // HTTP 状态；0 = 未知（未走到最终分支）

	LatencyMs int64 `json:"latency_ms"`

	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`      // 命中缓存的输入 token
	CacheWriteTokens int64   `json:"cache_write_tokens"` // 写入缓存的输入 token
	ReasoningTokens  int64   `json:"reasoning_tokens"`   // 思考过程 token
	Credits          float64 `json:"credits"`            // 实扣积分

	Err string `json:"err,omitempty"` // 失败原因（传输错误 / 上游原文），成功为空
	// Kind 失败分类（与 upstream.ErrKind 同词表：soft_rate / waf_block /
	// content_blocked / transport / upstream_parse …）。面板据此给出可读归类，
	// 比裸 HTTP 状态更能说明"为什么失败"。成功为空。
	Kind string `json:"kind,omitempty"`
}

// maxErrLen 单条记录保留的错误原文上限（字节）。上游错误体可能是长 HTML
// 错误页，全文入库会让 300 条环形缓冲的体积失控；截断只影响排障观感。
const maxErrLen = 600

// truncate 按字节截断（附省略号），供写入侧统一收口。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Ring 固定容量的环形缓冲，并发安全。
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	size int   // 容量上限
	next int   // 下一个写入下标
	seq  int64 // 已写入计数
}

// NewRing 创建容量为 n 的环形缓冲（n<=0 退回 300）。
func NewRing(n int) *Ring {
	if n <= 0 {
		n = 300
	}
	return &Ring{buf: make([]Entry, 0, n), size: n}
}

// Add 写入一条记录（自动补 Seq 与时间戳；T 为空时补当前时间）。
func (r *Ring) Add(e Entry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if e.T == "" {
		e.T = time.Now().Format(time.RFC3339)
	}
	e.Err = truncate(e.Err, maxErrLen)
	if len(r.buf) < r.size {
		r.buf = append(r.buf, e)
	} else {
		r.buf[r.next] = e
	}
	r.next = (r.next + 1) % r.size
}

// List 返回快照，最新在前（调用方只读，不持有内部切片）。
func (r *Ring) List() []Entry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.buf))
	// buf 是环形写入的：下标 next 起为最旧，逆序即为最新在前
	for i := len(r.buf); i > 0; i-- {
		out = append(out, r.buf[(r.next+i-1)%len(r.buf)])
	}
	return out
}

// Cap 返回容量（面板展示"最近 N 条"用）。
func (r *Ring) Cap() int {
	if r == nil {
		return 0
	}
	return r.size
}
