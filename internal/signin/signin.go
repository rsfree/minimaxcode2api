// Package signin 调 MiniMax Code 的每日签到接口。
//
// 签到和媒体网关同源（agent.minimax.cn 的 /minimax-cloud/api/v1），
// 同样只要 Bearer token，不需要 x-signature —— 所以这里是一个
// 「拿 access_token 直接打」的薄客户端，不碰 refresh 逻辑：
// token 生命周期由网关的账号池独占管理，外部再造一套必然撞
// refresh_token 的单次使用限制。
//
// 面板语义（2026-11-12 实测，七天一轮）：
//
//	status=1  还没到的日子
//	status=2  今日可领（也包含「今天已签但还没领」）
//	status=3  已领取
//
// 所以签到是**两步**动作：先读面板，只有今日 status==2 才 POST claim。
// 无脑 POST 会重复领取、被上游拒掉，还会把「今天已领过」当成失败。
package signin

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

// DayStatus 面板里单日的状态。
const (
	DayFuture    = 1 // 还没到
	DayClaimable = 2 // 今日可领
	DayClaimed   = 3 // 已领取
)

// BaseResp 是上游统一的业务信封：HTTP 永远 200，错误在 status_code 里。
type BaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// Day 七连签里的一天。
type Day struct {
	DayNo       int  `json:"day_no"`
	Points      int  `json:"points"`
	BonusPoints int  `json:"bonus_points"`
	IsToday     bool `json:"is_today"`
	Status      int  `json:"status"`
}

// Panel 签到面板。
type Panel struct {
	Scene int   `json:"scene"`
	Days  []Day `json:"days"`
}

// Today 返回今日那一条（上游保证恰好一条 is_today）。
func (p *Panel) Today() (Day, bool) {
	for _, d := range p.Days {
		if d.IsToday {
			return d, true
		}
	}
	return Day{}, false
}

// Claim 领取结果。
type Claim struct {
	ClaimID     string `json:"claim_id"`
	ClaimResult int    `json:"claim_result"`
	DayNo       int    `json:"day_no"`
	Points      int    `json:"points"`
	ExpireAtMs  int64  `json:"expire_at_ms"`
	Panel       Panel  `json:"panel"`
}

// Error 上游业务错误。
type Error struct {
	HTTPStatus int
	Code       int
	Msg        string
}

func (e *Error) Error() string {
	return fmt.Sprintf("signin upstream: http=%d code=%d %s", e.HTTPStatus, e.Code, e.Msg)
}

// Client 签到客户端。
type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// New 构造客户端。签到是轻请求，超时给 20s 足够（实测 <1s）。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		HTTP:      &http.Client{Timeout: timeout},
		UserAgent: "MiniMaxAgent",
	}
}

type envelope[T any] struct {
	BaseResp BaseResp `json:"base_resp"`
	Data     T        `json:"data"`
}

func (c *Client) do(ctx context.Context, method, base, token, tz string, out any) error {
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	u := strings.TrimRight(base, "/") + "/signin/" + map[string]string{
		http.MethodGet:  "status",
		http.MethodPost: "claim",
	}[method]
	q := url.Values{}
	q.Set("timezone_id", tz)
	u += "?" + q.Encode()

	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 上游一致用 200 表达业务错误，但代理层的 5xx/401 也会出现，先分别判。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{HTTPStatus: resp.StatusCode, Code: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}

	var env envelope[json.RawMessage]
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("解析签到响应失败: %w (body=%s)", err, truncate(string(raw), 200))
	}
	if env.BaseResp.StatusCode != 0 {
		return &Error{HTTPStatus: resp.StatusCode, Code: env.BaseResp.StatusCode, Msg: env.BaseResp.StatusMsg}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("签到响应缺少 data (body=%s)", truncate(string(raw), 200))
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("解析签到 data 失败: %w", err)
	}
	return nil
}

// Status 读签到面板（只读，不消耗任何东西）。
func (c *Client) Status(ctx context.Context, base, token, tz string) (*Panel, error) {
	var p Panel
	if err := c.do(ctx, http.MethodGet, base, token, tz, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Claim 领取今日签到积分。调用前必须确认今日 status==2。
func (c *Client) Claim(ctx context.Context, base, token, tz string) (*Claim, error) {
	var cl Claim
	if err := c.do(ctx, http.MethodPost, base, token, tz, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Result 单个账号的签到结果。
//
// 放在这个包而不是 server/panel 各自定义：/signin 端点、面板签到页、
// 自动签到定时器都要序列化同一份结构，三处分叉迟早会出现字段名漂移。
type Result struct {
	AccountID   string `json:"account_id"`
	Label       string `json:"label,omitempty"`
	OK          bool   `json:"ok"`
	Claimed     bool   `json:"claimed"`
	DayNo       int    `json:"day_no,omitempty"`
	Points      int    `json:"points"`
	TodayStatus int    `json:"today_status,omitempty"`
	Error       string `json:"error,omitempty"`
}
