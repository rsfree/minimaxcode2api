package server

import (
	"context"
	"errors"
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
