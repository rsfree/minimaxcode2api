package server

import (
	"strings"
	"testing"
)

// 402 的提示必须用上游原文里的实际 credits 数，而不是写死的样例价。
func TestBillingHintUsesUpstreamCost(t *testing.T) {
	msg := "the remaining M Plan weekly limit and account credits cannot cover this " +
		"MiniMax-H3 video generation; this task costs 229 credits"
	got := billingHint(msg)
	if !strings.Contains(got, "229 credits") {
		t.Fatalf("没摘出实际价格: %q", got)
	}
	if strings.Contains(got, "280 credits") {
		t.Fatalf("仍在用写死的 Hailuo 样例价: %q", got)
	}
	if !strings.Contains(got, "套餐档位") {
		t.Fatalf("缺少档位提示: %q", got)
	}
}

// 图片/TTS 的 "plan balance" 口径不变：窗口限额，不是要充值。
func TestBillingHintPlanBalance(t *testing.T) {
	got := billingHint("plan balance has run out")
	if !strings.Contains(got, "无需充值") {
		t.Fatalf("plan balance 口径被改动: %q", got)
	}
}
