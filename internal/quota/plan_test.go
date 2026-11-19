package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游真实响应（2026-11-19 生产实测截取），字段名与类型必须一模一样：
// 数字是数字、字符串是字符串 —— 混一处解析就会静默归零。
const membershipBody = `{
  "plan_type": 3,
  "plan_name": "",
  "will_renewal": false,
  "expires_at": 1796947200000,
  "total_remains_credit": 0,
  "has_token_plan": true,
  "token_plan_tier": "Go",
  "token_plan_expires_at": 1796947200000,
  "opcredit_balance": 0,
  "tp_remains": 14291685264.12,
  "subscription_type": "mplan",
  "op_credit_summary": {
    "total_remaining_amount": "4120",
    "purchased_remaining_amount": "0",
    "free_remaining_amount": "4120"
  },
  "base_resp": {"status_code": 0, "status_msg": "success"}
}`

func TestMembershipParse(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 16)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(membershipBody))
	}))
	defer ts.Close()

	m, err := New(nil).Membership(context.Background(), ts.URL, "tok")
	if err != nil {
		t.Fatalf("Membership: %v", err)
	}
	// 端点契约：必须 POST 到 /matrix 前缀，且 body 是空 JSON 对象。
	if gotMethod != http.MethodPost {
		t.Errorf("方法 = %s，应为 POST", gotMethod)
	}
	if gotPath != "/matrix/api/v1/commerce/get_membership_info" {
		t.Errorf("路径 = %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if strings.TrimSpace(gotBody) != "{}" {
		t.Errorf("body = %q，应为 {}", gotBody)
	}

	if m.Tier != "Go" || !m.HasTokenPlan {
		t.Errorf("套餐 = %q / has=%v", m.Tier, m.HasTokenPlan)
	}
	if m.TPRemains != 14291685264.12 {
		t.Errorf("TPRemains = %v", m.TPRemains)
	}
	if m.ExpiresAtMs != 1796947200000 {
		t.Errorf("ExpiresAtMs = %d", m.ExpiresAtMs)
	}
	// 积分是字符串数字，必须原样保留，不能被 JSON 数字解析截断。
	if m.CreditTotal != "4120" || m.CreditFree != "4120" || m.CreditPaid != "0" {
		t.Errorf("积分 = %q/%q/%q", m.CreditTotal, m.CreditFree, m.CreditPaid)
	}
	if m.FetchedAt.IsZero() {
		t.Error("FetchedAt 未设置")
	}
}

func TestMembershipBusinessError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"invalid token"}}`))
	}))
	defer ts.Close()
	_, err := New(nil).Membership(context.Background(), ts.URL, "tok")
	if err == nil || !strings.Contains(err.Error(), "1004") {
		t.Fatalf("应把业务错误码透出，实际 err = %v", err)
	}
}

// 窗口接口会按模型分行；面板要「账号整体」，所以 general 优先。
func TestWindowUsagePicksGeneral(t *testing.T) {
	body := `{
      "model_remains": [
        {"model_name":"video","current_interval_used_percent":"80%","current_weekly_used_percent":"70%",
         "end_time":1000,"weekly_end_time":2000},
        {"model_name":"general","current_interval_used_percent":"0%","current_weekly_used_percent":"12%",
         "end_time":1795090324261,"weekly_end_time":1795609538106}
      ],
      "base_resp": {"status_code": 0, "status_msg": "success"}
    }`
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	w, err := New(nil).WindowUsage(context.Background(), ts.URL, "tok")
	if err != nil {
		t.Fatalf("WindowUsage: %v", err)
	}
	if gotPath != "/backend/account/token_plan/remains_percent" {
		t.Errorf("路径 = %s", gotPath)
	}
	if w.ModelName != "general" || w.IntervalUsedPercent != "0%" || w.WeeklyUsedPercent != "12%" {
		t.Errorf("取到 %q %q/%q", w.ModelName, w.IntervalUsedPercent, w.WeeklyUsedPercent)
	}
	if w.IntervalEndMs != 1795090324261 || w.WeeklyEndMs != 1795609538106 {
		t.Errorf("重置时间 = %d/%d", w.IntervalEndMs, w.WeeklyEndMs)
	}
}

// 免费账号（无 token plan）上游返回空 model_remains —— 正常状态，不是错误。
// 返回 nil 且 err 为 nil，面板据此整段不显示；绝不能显示成 0% 用量。
func TestWindowUsageEmptyMeansNoPlan(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model_remains":[],"base_resp":{"status_code":0}}`))
	}))
	defer ts.Close()
	w, err := New(nil).WindowUsage(context.Background(), ts.URL, "tok")
	if err != nil {
		t.Fatalf("空 model_remains 不该报错: %v", err)
	}
	if w != nil {
		t.Fatalf("应返回 nil，实际 %+v", w)
	}
}

func TestPlanOrigin(t *testing.T) {
	cases := map[string]string{
		"https://agent.minimax.cn":              "https://www.minimax.cn",
		"https://agent.minimax.cn/mavis/api/v1": "https://www.minimax.cn",
		"https://agent.minimax.io/mavis/api/v1": "https://www.minimax.io",
		"https://agent.minimax.cn:8443/x":       "https://www.minimax.cn:8443",
		"":                                      "",
		// 没有 host 的输入必须返回空，不能拼出一个假域名。
		"not-a-url": "",
	}
	for in, want := range cases {
		if got := PlanOrigin(in); got != want {
			t.Errorf("PlanOrigin(%q) = %q，期望 %q", in, got, want)
		}
	}
}
