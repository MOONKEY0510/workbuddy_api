// blackcat_test.go 补偿领取（ClaimCompensation）形态与降级测试。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestClaimCompensationWebForm 首选 Web 域形态：webBase + 空 body +
// x-client-platform: web + Bearer + plans-usage Referer；
// 官方成功响应 {"code":0,"msg":"OK"}（无 data）应判成功。
func TestClaimCompensationWebForm(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Host != "web.example" {
			return nil, errors.New("wrong host: " + r.Host)
		}
		if r.URL.Path != "/billing/meter/claim-compensation" {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("wrong method: " + r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer at" {
			return nil, errors.New("missing Bearer")
		}
		if r.Header.Get("x-client-platform") != "web" {
			return nil, errors.New("missing x-client-platform")
		}
		if r.Header.Get("Referer") != "https://web.example/profile/plans-usage" {
			return nil, errors.New("wrong referer: " + r.Header.Get("Referer"))
		}
		return jsonResp(200, `{"code":0,"msg":"OK","requestId":"x"}`), nil
	})
	c.WebBaseCN = "https://web.example"
	if err := c.ClaimCompensation(&auth.Auth{UID: "u1", AccessToken: "at"}); err != nil {
		t.Fatalf("claim: %v", err)
	}
}

// TestClaimCompensationFallback Web 域鉴权失败（401）时降级旧 billing 域形态；
// 降级只发生在鉴权/路径类错误上，顺序 web → billing。
func TestClaimCompensationFallback(t *testing.T) {
	var hosts []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.Host)
		switch r.Host {
		case "web.example":
			return jsonResp(401, `{"code":401,"msg":"unauthorized"}`), nil
		case "billing.example":
			return jsonResp(200, `{"code":0,"msg":"OK"}`), nil
		}
		return nil, errors.New("unexpected host: " + r.Host)
	})
	c.WebBaseCN = "https://web.example"
	if err := c.ClaimCompensation(&auth.Auth{UID: "u1", AccessToken: "at"}); err != nil {
		t.Fatalf("fallback claim: %v", err)
	}
	if len(hosts) != 2 || hosts[0] != "web.example" || hosts[1] != "billing.example" {
		t.Fatalf("降级顺序应为 web→billing: %v", hosts)
	}
}

// TestClaimCompensationBusinessError 业务拒绝（已领取/无可领，200 + code!=0）
// 按错误原样透出，不触发降级（再打一遍 billing 域只会得到同样的拒绝）。
func TestClaimCompensationBusinessError(t *testing.T) {
	var calls int
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"code":14001,"msg":"已领取"}`), nil
	})
	c.WebBaseCN = "https://web.example"
	err := c.ClaimCompensation(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err == nil {
		t.Fatal("业务 code!=0 应返回错误")
	}
	var ue *Error
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "14001") {
		t.Fatalf("应透出业务错误码: %v", err)
	}
	if calls != 1 {
		t.Fatalf("业务错误不应降级重试: calls=%d", calls)
	}
}
