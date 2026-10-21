// queue_test.go 排队等待（code 6020）的分类与解析测试。
package upstream

import (
	"testing"
	"time"
)

// queueWaitSample 上游排队响应的实测样本（2026-10）。
const queueWaitSample = `{"code":6020,"msg":"queue.waiting.title","requestId":"96477397623b8fba630e64c4aa49270e","data":{"queue_position":2710,"queue_size":"999+","retry_after":40,"estimated_wait":13550,"client_user_type":"free"}}`

// TestClassifyQueueWait 排队判定必须先于状态码分支：200/429/503 三种外壳都归
// ErrQueueWait（5xx 外壳若落 ErrServer，handler 会 NoteError 把号池熔断）。
func TestClassifyQueueWait(t *testing.T) {
	for _, status := range []int{200, 429, 503} {
		if got := Classify(status, queueWaitSample); got != ErrQueueWait {
			t.Errorf("Classify(%d)=%v want ErrQueueWait", status, got)
		}
	}
	// 回归：普通 5xx（无排队特征）仍归 ErrServer，熔断语义不变。
	if got := Classify(503, `{"code":500,"msg":"internal error"}`); got != ErrServer {
		t.Errorf("plain 503=%v want ErrServer", got)
	}
}

// TestClassifyQueueFull 6021 queue.full（队列已满）与 6020 同属排队族：
// 同样必须先于状态码分支判定——否则 429 外壳会落 ErrSoftRate，给账号挂软冷却。
func TestClassifyQueueFull(t *testing.T) {
	const body = `{"code":6021,"msg":"queue.full.title","requestId":"701745d665c901903b7f7766a3069e4f"}`
	for _, status := range []int{200, 429, 503} {
		if got := Classify(status, body); got != ErrQueueWait {
			t.Errorf("Classify(%d)=%v want ErrQueueWait", status, got)
		}
	}
}

// TestParseQueueWait 解析建议等待 / 队列位置 / 队列长度；畸形输入返回零值。
func TestParseQueueWait(t *testing.T) {
	q := ParseQueueWait(queueWaitSample)
	if q.RetryAfter != 40*time.Second {
		t.Errorf("retry_after=%v want 40s", q.RetryAfter)
	}
	if q.Position != 2710 {
		t.Errorf("position=%d want 2710", q.Position)
	}
	if q.Size != "999+" {
		t.Errorf("size=%q want 999+", q.Size)
	}

	// 无 retry_after 时回落 estimated_wait 的毫秒口径。
	if q2 := ParseQueueWait(`{"code":6020,"data":{"estimated_wait":13550}}`); q2.RetryAfter != 13550*time.Millisecond {
		t.Errorf("estimated_wait fallback=%v want 13.55s", q2.RetryAfter)
	}
	// 非 JSON / 空 body：零值，不 panic、不编造。
	if q3 := ParseQueueWait("not-json"); q3.RetryAfter != 0 || q3.Position != 0 || q3.Size != "" {
		t.Errorf("malformed body should yield zero value: %+v", q3)
	}
}

// TestQueueWaitDetection queue_position / queue.waiting 文案同样命中（code 变体兜底）。
func TestQueueWaitDetection(t *testing.T) {
	cases := []string{
		queueWaitSample,
		`{"code": "6020", "msg": "queue.waiting.title"}`,
		`{"msg":"queue waiting","data":{"queue_position":3}}`,
		`{"code":6021,"msg":"queue.full.title"}`,
		`{"code": "6021", "msg": "queue.full.title"}`,
	}
	for _, body := range cases {
		if !IsQueueWait(body) {
			t.Errorf("IsQueueWait(%s)=false want true", body)
		}
	}
	if IsQueueWait(`{"code":0,"msg":"OK"}`) {
		t.Error("normal OK body must not match queue wait")
	}
}
