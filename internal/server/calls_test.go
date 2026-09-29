package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/calls"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestCallsFailureDetailUpstreamError 上游 4xx：调用记录带状态码 / 分类 / 上游原文，
// 面板「失败详情」据此回答"为什么失败"（这些字段此前恒为空）。
func TestCallsFailureDetailUpstreamError(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 429,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":6004,"message":"model usage limit exceeded"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	ring := calls.NewRing(8)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Calls: ring})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	list := ring.List()
	if len(list) != 1 {
		t.Fatalf("单账号单次尝试应恰记一条: %d", len(list))
	}
	e := list[0]
	if e.OK {
		t.Error("失败尝试必须 ok=false")
	}
	if e.Status != 429 {
		t.Errorf("status=%d want 429", e.Status)
	}
	if e.Kind != "soft_rate" {
		t.Errorf("kind=%q want soft_rate", e.Kind)
	}
	if !strings.Contains(e.Err, "6004") {
		t.Errorf("上游原文应入库: %q", e.Err)
	}
	if e.Model != "glm-5.2" || e.UID != "u1" {
		t.Errorf("基本字段缺失: %+v", e)
	}
}

// TestCallsFailureDetailTransport 传输层错误：503 + transport 分类 + 错误文案。
func TestCallsFailureDetailTransport(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp 127.0.0.1:443: connect: connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	ring := calls.NewRing(8)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Calls: ring})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	e := ring.List()[0]
	if e.Kind != "transport" || e.Status != http.StatusServiceUnavailable {
		t.Errorf("kind=%q status=%d", e.Kind, e.Status)
	}
	if !strings.Contains(e.Err, "connection refused") {
		t.Errorf("错误文案应入库: %q", e.Err)
	}
}

// TestCallsSuccessHasNoFailureDetail 成功路径不写失败明细（面板只对失败行开详情）。
func TestCallsSuccessHasNoFailureDetail(t *testing.T) {
	ring := calls.NewRing(8)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: captureUpstream(t, nil, sseOK), Calls: ring})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	e := ring.List()[0]
	if !e.OK || e.Kind != "" || e.Err != "" || e.Status != 0 {
		t.Errorf("成功记录不应带失败明细: %+v", e)
	}
}
