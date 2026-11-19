package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/quota"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
)

const (
	// 面板每次进概览都会问一次额度，不缓存会把上游打成筛子。
	quotaTTLSuccess = 60 * time.Second
	// 失败也缓存：上游抖动 + 面板自动刷新 = 请求风暴，这个坑很容易踩。
	quotaTTLFailure = 30 * time.Second
	// 额度是旁路信息，绝不能拖慢概览：单次最多等 8 秒。
	quotaTimeout = 8 * time.Second
)

type quotaEntry struct {
	at   time.Time
	data *quota.ImageFree
	err  error
}

// ImageQuotaForAccount 取某账号的图片生成免费额度。
//
// 走 ensureFresh 取 token —— 与主链路共用同一把**账号级**锁，所以不会出现
// 「面板刷新额度」和「正在跑的请求」同时轮换 refresh_token 把彼此刷成
// invalid_grant。这一点比额度本身更重要。
func (s *Server) ImageQuotaForAccount(ctx context.Context, acct store.Account) (*quota.ImageFree, error) {
	if s.Quota == nil {
		return nil, errors.New("额度客户端未初始化")
	}
	if data, err, ok := s.quotaCached(acct.ID); ok {
		return data, err
	}
	data, err := s.fetchQuota(ctx, acct)
	s.quotaStore(acct.ID, data, err)
	return data, err
}

func (s *Server) quotaCached(id string) (*quota.ImageFree, error, bool) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	e, ok := s.quotaCache[id]
	if !ok {
		return nil, nil, false
	}
	ttl := quotaTTLSuccess
	if e.err != nil {
		ttl = quotaTTLFailure
	}
	if time.Since(e.at) >= ttl {
		return nil, nil, false
	}
	return e.data, e.err, true
}

func (s *Server) quotaStore(id string, data *quota.ImageFree, err error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.quotaCache == nil {
		s.quotaCache = map[string]quotaEntry{}
	}
	s.quotaCache[id] = quotaEntry{at: time.Now(), data: data, err: err}
}

func (s *Server) fetchQuota(ctx context.Context, acct store.Account) (*quota.ImageFree, error) {
	fresh, err := s.ensureFresh(ctx, acct, false)
	if err != nil {
		return nil, err
	}
	origin := quota.Origin(s.Cfg.Load().Upstream.BaseURL)
	qctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	return s.Quota.ImageFree(qctx, origin, fresh.AccessToken)
}

// ---------- 套餐 / 积分 / 窗口 ----------

const (
	// 套餐与积分变化很慢（只随扣费或签到变动），缓存可以比图片额度更长；
	// 但也不能太长 —— 用户点面板就是为了确认"刚才那次调用扣了多少"。
	planTTLSuccess = 60 * time.Second
	planTTLFailure = 30 * time.Second
	// 两个端点各自最多等 6 秒，且**并发**取：串行会让首屏等 12 秒。
	planTimeout = 6 * time.Second
)

type planEntry struct {
	at   time.Time
	data *quota.Plan
}

func (s *Server) planCached(id string) (*quota.Plan, bool) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	e, ok := s.planCache[id]
	if !ok {
		return nil, false
	}
	ttl := planTTLSuccess
	if e.data == nil || (e.data.Membership == nil && e.data.Window == nil) {
		ttl = planTTLFailure
	}
	if time.Since(e.at) >= ttl {
		return nil, false
	}
	return e.data, true
}

func (s *Server) planStore(id string, data *quota.Plan) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if s.planCache == nil {
		s.planCache = map[string]planEntry{}
	}
	s.planCache[id] = planEntry{at: time.Now(), data: data}
}

// PlanForAccount 取某账号的「套餐 / 积分 / 窗口」快照。
//
// 两个上游端点并发取，任何一个失败只标记它自己那一段 —— 面板绝不能因为
// 窗口接口抖动就连积分都不显示。只有「拿不到 token」才整体返回 error。
//
// 与 ImageQuotaForAccount 一样走 ensureFresh：共用账号级锁，避免面板刷新
// 与主链路竞态把 refresh_token 刷成 invalid_grant。
func (s *Server) PlanForAccount(ctx context.Context, acct store.Account) (*quota.Plan, error) {
	if s.Quota == nil {
		return nil, errors.New("额度客户端未初始化")
	}
	if data, ok := s.planCached(acct.ID); ok {
		return data, nil
	}
	fresh, err := s.ensureFresh(ctx, acct, false)
	if err != nil {
		return nil, err
	}
	origin := quota.Origin(s.Cfg.Load().Upstream.BaseURL)
	planOrigin := quota.PlanOrigin(origin)
	token := fresh.AccessToken

	out := &quota.Plan{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mctx, cancel := context.WithTimeout(ctx, planTimeout)
		defer cancel()
		m, err := s.Quota.Membership(mctx, origin, token)
		if err != nil {
			out.MembershipError = err.Error()
			return
		}
		out.Membership = m
	}()
	go func() {
		defer wg.Done()
		wctx, cancel := context.WithTimeout(ctx, planTimeout)
		defer cancel()
		w, err := s.Quota.WindowUsage(wctx, planOrigin, token)
		if err != nil {
			out.WindowError = err.Error()
			return
		}
		out.Window = w
	}()
	wg.Wait()

	s.planStore(acct.ID, out)
	return out, nil
}
