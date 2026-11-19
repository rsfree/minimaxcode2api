package upstream

import (
	"net/http"
	"strings"
	"testing"
)

// 额度不足有三种上游写法，都必须识别成「计费」，
// 否则账号不会进额度冷却，每个请求都要白打一次上游。
func TestBillingError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"HTTP 402", 402, `{"error":{"type":"billing_error","message":"x"}}`, true},
		{"400 + insufficient_quota 类型", 400, `{"error":{"type":"insufficient_quota","message":"x"}}`, true},
		{"400 + plan balance 文案", 400, `{"error":{"type":"invalid_request_error","message":"the user's plan balance has run out"}}`, true},
		{"400 + insufficient quota 文案", 400, `{"error":{"message":"Insufficient quota for this request"}}`, true},
		// 参数错不能被误判成额度问题，否则会把好账号冷却掉。
		{"400 参数错", 400, `{"error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`, false},
		{"500", 500, `{"error":{"type":"server_error","message":"boom"}}`, false},
		{"空 body", 500, ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := ParseError(c.status, strings.NewReader(c.body))
			if got := e.BillingError(); got != c.want {
				t.Fatalf("BillingError() = %v，期望 %v（status=%d body=%s）", got, c.want, c.status, c.body)
			}
		})
	}
}

// 计费错误不该并入 Retryable：换号重试通常是同一个 plan 的同一个池。
func TestBillingNotRetryable(t *testing.T) {
	e := ParseError(http.StatusPaymentRequired, strings.NewReader(`{"error":{"message":"plan balance has run out"}}`))
	if e.Retryable() {
		t.Fatal("402 不应被判为可重试")
	}
}
