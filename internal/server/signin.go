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

// SigninStatus 只读签到面板：不领取，只回报每个账号今天的状态。
//
// 面板签到页初始加载走这里 —— 打开页面就自动领一次太粗暴，订阅看的
// 和按的应该是两件事。
func (s *Server) SigninStatus(ctx context.Context, accountID string) ([]signin.Result, error) {
	return s.signinRun(ctx, accountID, false)
}

// SigninAccounts 逐账号签到：今日 status=2 才领取。
//
// 幂等；单号失败不影响其他号；accountID 为空表示全池。
func (s *Server) SigninAccounts(ctx context.Context, accountID string) ([]signin.Result, error) {
	return s.signinRun(ctx, accountID, true)
}

func (s *Server) signinRun(ctx context.Context, accountID string, claim bool) ([]signin.Result, error) {
	targets, err := s.signinTargets(accountID)
	if err != nil {
		return nil, err
	}

	cfg := s.Cfg.Load()
	tz := cfg.Signin.Timezone
	client := signin.New(20 * time.Second)

	results := make([]signin.Result, len(targets))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, acct := range targets {
		wg.Add(1)
		go func(i int, acct store.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.signinOne(ctx, client, acct, tz, claim)
		}(i, acct)
	}
	wg.Wait()
	return results, nil
}

func (s *Server) signinTargets(accountID string) ([]store.Account, error) {
	if accountID != "" {
		a, ok := s.Store.Get(accountID)
		if !ok {
			return nil, errAccountNotFound
		}
		return []store.Account{a}, nil
	}
	// 禁用账号也签：积分照拿，只是不参与调度，所以这里不过滤 Disabled。
	return s.Store.List(), nil
}

var errAccountNotFound = errors.New("账号不存在")

// signinOne 单账号：取新鲜 token → 读面板 → 必要时领取。
func (s *Server) signinOne(ctx context.Context, client *signin.Client, acct store.Account, tz string, claim bool) signin.Result {
	res := signin.Result{AccountID: acct.ID, Label: acct.Label}

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

	// 已领取 / 还没到日子 / 只读模式：都不是错误，如实汇报即可。
	if today.Status != signin.DayClaimable || !claim {
		res.OK = true
		return res
	}

	cl, err := client.Claim(ctx, s.cloudBase(), fresh.AccessToken, tz)
	if err != nil {
		res.Error = "签到失败: " + err.Error()
		return res
	}
	res.OK = true
	res.Claimed = true
	res.Points = cl.Points
	if cl.DayNo > 0 {
		res.DayNo = cl.DayNo
	}
	res.TodayStatus = signin.DayClaimed
	s.Store.TouchUsage(fresh.ID, 0, 0)
	return res
}

// handleSignin 是「所有账号各签一次」的管理端点。
//
// 两步走（先读面板再 claim），因为上游 status=2 才是可领，直接 POST
// 在已领取的日子会被拒；并发 3，避免串行时一个卡住的账号拖垮整批。
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

	results, err := s.SigninAccounts(r.Context(), strings.TrimSpace(req.AccountID))
	if err != nil {
		if errors.Is(err, errAccountNotFound) {
			writeOpenAIError(w, http.StatusNotFound, "account_not_found", "账号不存在: "+req.AccountID)
			return
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}

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
		"timezone":     s.Cfg.Load().Signin.Timezone,
		"total_points": total,
		"results":      results,
	})
}
