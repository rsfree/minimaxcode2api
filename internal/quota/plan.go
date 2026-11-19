// 套餐与积分：面板「账号额度」卡片的第二、三段数据源。
//
// 与 quota.go 的「图片免费次数」并列，但**属于完全不同的池子**，
// 三者不可相加、不可互推：
//
//	池子                 端点                                                口径
//	图片免费次数         GET  {agent}/minimax-cloud/api/v1/image/free-quota     次数（限时活动，10 次）
//	token plan           POST {agent}/matrix/api/v1/commerce/get_membership_info  tp_remains（上游原始计数）
//	积分 credits         同上，op_credit_summary                                  个
//	5 小时 / 周 窗口     GET  {www}/backend/account/token_plan/remains_percent     已用百分比
//
// 都是 Bearer 即可，不需要 x-signature —— 签名只作用于 /matrix 的业务接口
// （2026-11-17 首次实测，2026-11-19 复核仍成立）。
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Membership 是「套餐 + 积分」快照，一次请求全拿到。
type Membership struct {
	// PlanType / PlanName / Subscription 是上游的套餐标识（mplan = M Plan）。
	PlanType int    `json:"plan_type"`
	PlanName string `json:"plan_name"`
	// Tier 是套餐档位（Go / Explore / Build）；空表示账号没有 token plan。
	Tier string `json:"tier"`
	// TPRemains 是 token plan 剩余额度，**上游原始计数**。不是人民币、不是积分，
	// 与 credits 也不是同一单位 —— 面板必须原样展示，不要换算成"元"或"次"。
	TPRemains    float64 `json:"tp_remains"`
	HasTokenPlan bool    `json:"has_token_plan"`
	ExpiresAtMs  int64   `json:"expires_at_ms"`
	WillRenewal  bool    `json:"will_renewal"`
	Subscription string  `json:"subscription"`

	// 积分余额。上游给的是字符串数字（"4120"），保留字符串避免精度损失。
	CreditTotal string `json:"credit_total"`
	CreditFree  string `json:"credit_free"`
	CreditPaid  string `json:"credit_paid"`

	FetchedAt time.Time `json:"fetched_at"`
}

// Window 是 5 小时 / 周 两个滑动窗口的已用百分比。
//
// 这是**频控**而非余额：打满后上游回 402（"plan balance has run out"），
// 但 tp_remains 仍可能为正 —— 所以面板要把两者分开显示，否则会被误读成"还有余额为什么报错"。
type Window struct {
	ModelName           string    `json:"model_name"`
	IntervalUsedPercent string    `json:"interval_used_percent"`
	WeeklyUsedPercent   string    `json:"weekly_used_percent"`
	IntervalEndMs       int64     `json:"interval_end_ms"`
	WeeklyEndMs         int64     `json:"weekly_end_ms"`
	FetchedAt           time.Time `json:"fetched_at"`
}

// Plan 是面板一张卡需要的全部额度信息。
//
// 三个池各自可能失败（上游抖动、账号被踢、端点下线），失败只影响自己那一段，
// 不能把整张卡变成错误 —— 否则面板会在一个池挂掉时连积分都看不见。
type Plan struct {
	Membership      *Membership
	Window          *Window
	MembershipError string
	WindowError     string
}

// PlanOrigin 从上游根推出「账户中心」站点根（5 小时/周窗口在 www 上）。
//
//	https://agent.minimax.cn/...  → https://www.minimax.cn
//	https://agent.minimax.io/...  → https://www.minimax.io
func PlanOrigin(agentOrigin string) string {
	u, err := url.Parse(strings.TrimSpace(agentOrigin))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	host := u.Host
	if first, rest, ok := strings.Cut(host, "."); ok && first == "agent" {
		host = "www." + rest
	} else {
		host = "www." + host
	}
	return u.Scheme + "://" + host
}

// membershipEnvelope 是上游响应信封。
type membershipEnvelope struct {
	PlanType     int     `json:"plan_type"`
	PlanName     string  `json:"plan_name"`
	Tier         string  `json:"token_plan_tier"`
	TPRemains    float64 `json:"tp_remains"`
	HasTokenPlan bool    `json:"has_token_plan"`
	ExpiresAtMs  int64   `json:"token_plan_expires_at"`
	WillRenewal  bool    `json:"will_renewal"`
	Subscription string  `json:"subscription_type"`
	Credit       struct {
		Total     string `json:"total_remaining_amount"`
		Purchased string `json:"purchased_remaining_amount"`
		Free      string `json:"free_remaining_amount"`
	} `json:"op_credit_summary"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// Membership 取一次套餐 + 积分快照。
func (c *Client) Membership(ctx context.Context, agentOrigin, accessToken string) (*Membership, error) {
	agentOrigin = strings.TrimRight(strings.TrimSpace(agentOrigin), "/")
	if agentOrigin == "" {
		return nil, fmt.Errorf("quota: 无法从上游地址推导站点根")
	}
	const path = "/matrix/api/v1/commerce/get_membership_info"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentOrigin+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	// 必须带 Content-Type: 空 body 的 POST 不带它，部分网关会按表单解析。
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MiniMaxAgent Mozilla/5.0")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota: 会员信息请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota: 会员信息 HTTP %d %s", resp.StatusCode, snippet(raw))
	}
	var env membershipEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("quota: 会员信息不是合法 JSON: %w", err)
	}
	if env.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("quota: 会员信息业务错误 %d %s", env.BaseResp.StatusCode, env.BaseResp.StatusMsg)
	}
	return &Membership{
		PlanType:     env.PlanType,
		PlanName:     env.PlanName,
		Tier:         strings.TrimSpace(env.Tier),
		TPRemains:    env.TPRemains,
		HasTokenPlan: env.HasTokenPlan,
		ExpiresAtMs:  env.ExpiresAtMs,
		WillRenewal:  env.WillRenewal,
		Subscription: env.Subscription,
		CreditTotal:  env.Credit.Total,
		CreditFree:   env.Credit.Free,
		CreditPaid:   env.Credit.Purchased,
		FetchedAt:    time.Now(),
	}, nil
}

// windowEnvelope 是 remains_percent 的响应。
type windowEnvelope struct {
	ModelRemains []struct {
		ModelName           string `json:"model_name"`
		IntervalUsedPercent string `json:"current_interval_used_percent"`
		WeeklyUsedPercent   string `json:"current_weekly_used_percent"`
		IntervalEndMs       int64  `json:"end_time"`
		WeeklyEndMs         int64  `json:"weekly_end_time"`
	} `json:"model_remains"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// WindowUsage 取一次 5 小时 / 周窗口用量。
//
// model_remains 会按模型分行（general 等）。取 general 优先、否则第一行 ——
// 面板要的是"账号整体有没有被打满"，逐模型列出反而噪音。
func (c *Client) WindowUsage(ctx context.Context, planOrigin, accessToken string) (*Window, error) {
	planOrigin = strings.TrimRight(strings.TrimSpace(planOrigin), "/")
	if planOrigin == "" {
		return nil, fmt.Errorf("quota: 无法推导账户中心域名")
	}
	const path = "/backend/account/token_plan/remains_percent"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, planOrigin+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MiniMaxAgent Mozilla/5.0")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota: 窗口用量请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota: 窗口用量 HTTP %d %s", resp.StatusCode, snippet(raw))
	}
	var env windowEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("quota: 窗口用量不是合法 JSON: %w", err)
	}
	if env.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("quota: 窗口用量业务错误 %d %s", env.BaseResp.StatusCode, env.BaseResp.StatusMsg)
	}
	if len(env.ModelRemains) == 0 {
		// 免费账号（没有 token plan）上游会返回空数组 —— 这是正常状态，
		// 不是错误。返回 nil 让面板整段不显示，而不是给用户看一条假告警。
		return nil, nil
	}
	pick := env.ModelRemains[0]
	for _, m := range env.ModelRemains {
		if m.ModelName == "general" {
			pick = m
			break
		}
	}
	return &Window{
		ModelName:           pick.ModelName,
		IntervalUsedPercent: pick.IntervalUsedPercent,
		WeeklyUsedPercent:   pick.WeeklyUsedPercent,
		IntervalEndMs:       pick.IntervalEndMs,
		WeeklyEndMs:         pick.WeeklyEndMs,
		FetchedAt:           time.Now(),
	}, nil
}
