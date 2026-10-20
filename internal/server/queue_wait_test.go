// queue_wait_test.go 上游排队（code 6020）不得罚账号的端到端回归测试。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// queueWaitBody 上游排队响应实测样本（5xx / 200 两种外壳共用）。
const queueWaitBody = `{"code":6020,"msg":"queue.waiting.title","requestId":"96477397623b8fba630e64c4aa49270e","data":{"queue_position":2710,"queue_size":"999+","retry_after":40,"estimated_wait":13550,"client_user_type":"free"}}`

// TestQueueWait5xxDoesNotPenalizeAccount 排队（5xx 外壳）此前被判 ErrServer →
// NoteError → 号池熔断。熔断阈值设为 1（一罚即熔断），若仍误罚本用例必挂。
func TestQueueWait5xxDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 503, queueWaitBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 一罚即熔断：精确验证"排队不罚号"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "40" {
		t.Errorf("Retry-After=%q want 40", got)
	}
	// 原文透传（客户端必须看到上游的排队信息）。
	if !strings.Contains(rec.Body.String(), "queue.waiting") {
		t.Errorf("message should carry upstream original body: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "queue_wait") {
		t.Errorf("error.code should be queue_wait: %s", rec.Body)
	}

	st, _ := p.Status("u1")
	if st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Fatalf("排队不得喂熔断: breaker_fails=%d until=%v", st.BreakerFails, st.BreakerUntil)
	}
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.ConsecutiveFails != 0 {
		t.Fatalf("排队不得冷却/降权/累错: %+v", st)
	}
}

// TestQueueWait200JSON 200 + JSON 信封形态：不能当"空流"记 upstream_parse，
// 同样按排队处理（429 + Retry-After），且不罚账号。
func TestQueueWait200JSON(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, queueWaitBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("200+queue JSON code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "40" {
		t.Errorf("Retry-After=%q want 40", got)
	}
	st, _ := p.Status("u1")
	if st.BreakerFails != 0 || st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("200+排队不得罚号: %+v", st)
	}
}

// TestServer5xxStillPenalizes 回归：普通 5xx（非排队）仍喂熔断——修复不能把
// 真正的上游故障一起放生。
func TestServer5xxStillPenalizes(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 503, `{"code":500,"msg":"internal error"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("普通 5xx 不应变成 429 排队: body=%s", rec.Body)
	}
	st, _ := p.Status("u1")
	if st.BreakerFails == 0 && st.BreakerUntil.IsZero() {
		t.Fatalf("普通 5xx 仍应喂熔断: %+v", st)
	}
}
