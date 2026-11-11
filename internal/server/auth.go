package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/store"
)

var errNoRefreshToken = errors.New("账号没有 refresh_token，无法刷新")

// refreshLock 取账号级刷新互斥锁。
func (s *Server) refreshLock(id string) *sync.Mutex {
	v, _ := s.refreshLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ensureFresh 保证返回的账号 access_token 足够新。
//
// force=false：仅在快过期时刷新（正常请求路径）。
// force=true ：401 之后的强制刷新（同账号重试一次）。
//
// 关键设计：刷新前会重新读一次账号，因为同一个账号的并发请求可能已经被别的
// goroutine 刷新过了；拿到锁之后再读一次，避免用旧的 refresh_token 二次轮换。
func (s *Server) ensureFresh(ctx context.Context, acct store.Account, force bool) (store.Account, error) {
	cfg := s.Cfg.Load()
	if !force {
		if !acct.ExpiresWithin(time.Now(), cfg.Pool.RefreshBefore.Std()) {
			return acct, nil
		}
	}

	lock := s.refreshLock(acct.ID)
	lock.Lock()
	defer lock.Unlock()

	// 拿锁后重读，可能已经被别人刷过。
	if cur, ok := s.Store.Get(acct.ID); ok {
		if !force && !cur.ExpiresWithin(time.Now(), cfg.Pool.RefreshBefore.Std()) {
			return cur, nil
		}
		acct = cur
	}

	if acct.RefreshToken == "" {
		return acct, errNoRefreshToken
	}

	tok, err := s.OAuth.Refresh(acct.RefreshToken)
	if err != nil {
		s.Store.Update(acct.ID, func(a *store.Account) {
			a.LastError = "refresh: " + err.Error()
		})
		return acct, fmt.Errorf("刷新 token 失败: %w", err)
	}

	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()
	updated, _, uerr := s.Store.Update(acct.ID, func(a *store.Account) {
		a.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			a.RefreshToken = tok.RefreshToken
		}
		a.ExpiresAtMs = expiresAt
		if len(tok.Scopes) > 0 {
			a.Scopes = tok.Scopes
		}
		a.Generation++
		a.LastError = ""
	})
	if uerr != nil {
		return acct, uerr
	}
	return updated, nil
}
