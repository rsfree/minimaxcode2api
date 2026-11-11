package convert

import (
	"encoding/json"
	"testing"
)

func m3Opts(maxTokens int) RequestOptions {
	cap, _ := CapabilityOf("MiniMax-M3")
	return RequestOptions{
		Model:          "MiniMax-M3",
		MaxTokens:      maxTokens,
		ThinkingPolicy: "auto",
		Capability:     cap,
		Known:          true,
	}
}

func flashOpts(maxTokens int) RequestOptions {
	cap, _ := CapabilityOf("MiniMax-M3.1-Flash")
	return RequestOptions{
		Model:          "MiniMax-M3.1-Flash",
		MaxTokens:      maxTokens,
		ThinkingPolicy: "auto",
		Capability:     cap,
		Known:          true,
	}
}

// M3.1-Flash 是 forced_adaptive：任何请求都不能带 thinking，
// 否则上游直接 400（"requires adaptive thinking"）。
func TestForcedAdaptiveNeverSendsThinking(t *testing.T) {
	for _, effort := range []string{"", "high", "none"} {
		for _, policy := range []string{"auto", "on", "off"} {
			opt := flashOpts(8192)
			opt.ThinkingPolicy = policy
			in := &ChatRequest{
				Model:           "MiniMax-M3.1-Flash",
				ReasoningEffort: effort,
				Messages:        []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			}
			out, err := BuildAnthropicRequest(in, opt)
			if err != nil {
				t.Fatalf("effort=%q policy=%q: %v", effort, policy, err)
			}
			if out.Thinking != nil {
				t.Fatalf("effort=%q policy=%q 时仍传了 thinking=%+v，上游会 400", effort, policy, out.Thinking)
			}
		}
	}
}

// switchable 模型：auto 默认关，显式 effort 才开。
func TestSwitchableThinkingPolicy(t *testing.T) {
	in := &ChatRequest{Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	out, err := BuildAnthropicRequest(in, m3Opts(8192))
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking == nil || out.Thinking.Type != "disabled" {
		t.Fatalf("auto + 无 effort 应关闭思考，实际 %+v", out.Thinking)
	}

	in.ReasoningEffort = "high"
	out, err = BuildAnthropicRequest(in, m3Opts(8192))
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking == nil || out.Thinking.Type != "enabled" {
		t.Fatalf("effort=high 应开启思考，实际 %+v", out.Thinking)
	}
	if out.Thinking.BudgetTokens <= 0 || out.Thinking.BudgetTokens >= out.MaxTokens {
		t.Fatalf("budget_tokens=%d 必须满足 0 < budget < max_tokens=%d", out.Thinking.BudgetTokens, out.MaxTokens)
	}
}

// 客户端把 max_tokens 压得很小时，不能算出 budget >= max_tokens 的非法组合。
func TestThinkingBudgetFitsSmallMaxTokens(t *testing.T) {
	for _, maxTokens := range []int{256, 512, 1024, 1536} {
		in := &ChatRequest{
			ReasoningEffort: "high",
			Messages:        []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}
		opt := m3Opts(maxTokens)
		out, err := BuildAnthropicRequest(in, opt)
		if err != nil {
			t.Fatalf("max_tokens=%d: %v", maxTokens, err)
		}
		if out.MaxTokens != maxTokens {
			t.Fatalf("max_tokens 被改成 %d，期望 %d", out.MaxTokens, maxTokens)
		}
		if out.Thinking == nil || out.Thinking.Type != "enabled" {
			continue // 直接关掉思考也是合法解
		}
		if out.Thinking.BudgetTokens >= out.MaxTokens {
			t.Fatalf("max_tokens=%d 时 budget_tokens=%d，违反 budget < max_tokens，上游会 400",
				maxTokens, out.Thinking.BudgetTokens)
		}
	}
}

// 未知模型不猜 thinking，交给服务端默认。
func TestUnknownModelLeavesThinkingUnset(t *testing.T) {
	in := &ChatRequest{
		ReasoningEffort: "high",
		Messages:        []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	opt := RequestOptions{Model: "some-future-model", MaxTokens: 4096, Known: false}
	out, err := BuildAnthropicRequest(in, opt)
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking != nil {
		t.Fatalf("未知模型不该传 thinking，实际 %+v", out.Thinking)
	}
}
