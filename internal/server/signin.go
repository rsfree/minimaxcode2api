package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/signin"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
)

// cloudBase 云服务基址（签到等业务接口所在域）。
//
// 与 LLM/媒体网关不同，这些接口挂在 /minimax-cloud/api/v1 而不是 /mavis，
// 但同属一个 origin。所以默认用 upstream.base_url 的 scheme+host 推导，
// 切 region 时自动跟着走；真有大变动再显式配 signin.cloud_base。
func (s *Server) cloudBase() string {
	cfg := s.Cfg.Load()
	if v := strings.TrimSpace(cfg.Signin.CloudBase); v != "" {
		return strings.TrimRight(v, "/")
	}
	if u, err := url.Parse(cfg.Upstream.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/minimax-cloud/api/v1"
	}
	return "https://agent.minimax.cn/minimax-cloud/api/v1"
}

// signinResult 单个账号的签到结果。
type signinResult struct {
	AccountID   string `json:"account_id"`
	Label       string `json:"label,omitempty"`
	OK          bool   `json:"ok"`
	Claimed     bool   `json:"claimed"`
	DayNo       int    `json:"day_no,omitempty"`
	Points      int    `json:"points"`
	TodayStatus int    `json:"today_status,omitempty"`
	Error       string `json:"error,omitempty"`
}

// handleSignin 是「所有账号各签一次」的管理端点。
//
// 设计要点：
//   - 两步走（先读面板再 claim），因为上游 status=2 才是可领，直接 POST
//     在已领取的日子会被拒；
//   - 每账号独立成败，单号 401 不影响其他号；
//   - 并发 3，避免串行时一个卡住的账号拖垮整批（自动化有超时）。
func (s *Server) handleSignin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
			return
		}
	}

	accounts := s.Store.List()
	if req.AccountID != "" {
		a, ok := s.Store.Get(req.AccountID)
		if !ok {
			writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+req.AccountID)
			return
		}
		accounts = []store.Account{a}
	}

	// 禁用账号也签：积分照拿，只是不参与调度，所以这里不过滤 Disabled。
	targets := accounts
	if len(targets) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account", "还没有账号，先在面板里添加")
		return
	}

	cfg := s.Cfg.Load()
	tz := cfg.Signin.Timezone
	client := signin.New(20 * time.Second)

	results := make([]signinResult, len(targets))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, acct := range targets {
		wg.Add(1)
		go func(i int, acct store.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.signinOne(r.Context(), client, acct, tz)
		}(i, acct)
	}
	wg.Wait()

	okAll := true
	total := 0
	for _, res := range results {
		if !res.OK {
			okAll = false
		}
		total += res.Points
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           okAll,
		"timezone":     tz,
		"total_points": total,
		"results":      results,
	})
}

// signinOne 单账号：取新鲜 token → 读面板 → 必要时领取。
func (s *Server) signinOne(ctx context.Context, client *signin.Client, acct store.Account, tz string) signinResult {
	res := signinResult{AccountID: acct.ID, Label: acct.Label}

	fresh, err := s.ensureFresh(ctx, acct, false)
	if err != nil {
		res.Error = "取 token 失败: " + err.Error()
		return res
	}

	panel, err := client.Status(ctx, s.cloudBase(), fresh.AccessToken, tz)
	if err != nil {
		res.Error = "读签到面板失败: " + err.Error()
		return res
	}
	today, ok := panel.Today()
	if !ok {
		res.Error = "面板里没有今天这一格（上游改了结构？）"
		return res
	}
	res.DayNo = today.DayNo
	res.TodayStatus = today.Status

	// 已领取 / 还没到日子：都不是错误，如实汇报即可。
	if today.Status != signin.DayClaimable {
		res.OK = true
		return res
	}

	claim, err := client.Claim(ctx, s.cloudBase(), fresh.AccessToken, tz)
	if err != nil {
		res.Error = "签到失败: " + err.Error()
		return res
	}
	res.OK = true
	res.Claimed = true
	res.Points = claim.Points
	if claim.DayNo > 0 {
		res.DayNo = claim.DayNo
	}
	res.TodayStatus = signin.DayClaimed
	s.Store.TouchUsage(fresh.ID, 0, 0)
	return res
}
