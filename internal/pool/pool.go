// Package pool 账号池调度：选号、并发租约、冷却与熔断。
//
// 目标只有一个：单个账号出问题时，请求不要跟着一起死。
// 因此所有失败都要落到"账号维度"的状态上，而不是"请求维度"重试到天荒地老。
package pool

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
)

var (
	// ErrNoAccount 池里没有可用账号。
	ErrNoAccount = errors.New("没有可用账号")
	// ErrAllBusy 账号都处于冷却或在途上限。
	ErrAllBusy = errors.New("账号全部冷却或在途已满")
)

type state struct {
	inflight      int
	cooldownUntil time.Time
	failures      int
	// cooldownStage 429 指数退避的阶梯计数，成功一次清零。
	cooldownStage int
	lastPick      time.Time
}

// Pool 账号池。
type Pool struct {
	cfg   *config.Holder
	store *store.Store

	mu    sync.Mutex
	state map[string]*state
	rr    int
	rnd   *rand.Rand
}

func New(cfg *config.Holder, st *store.Store) *Pool {
	return &Pool{
		cfg:   cfg,
		store: st,
		state: map[string]*state{},
		rnd:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (p *Pool) st(id string) *state {
	s, ok := p.state[id]
	if !ok {
		s = &state{}
		p.state[id] = s
	}
	return s
}

// Lease 一次选号结果，使用完必须 Release。
type Lease struct {
	Account store.Account
	pool    *Pool
	once    sync.Once
}

// Release 归还租约。
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		if s, ok := l.pool.state[l.Account.ID]; ok && s.inflight > 0 {
			s.inflight--
		}
	})
}

// Success 记账成功：清失败计数与冷却。
func (p *Pool) Success(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.state[id]; ok {
		s.failures = 0
		s.cooldownStage = 0
		s.cooldownUntil = time.Time{}
	}
}

// Cooldown 直接把账号打入冷却（例如 429）。
func (p *Pool) Cooldown(id string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.st(id)
	if d <= 0 {
		return
	}
	until := time.Now().Add(d)
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
}

// Failure 记一次失败并按配置施加冷却；达到阈值触发熔断。
func (p *Pool) Failure(id string, status int) {
	cfg := p.cfg.Load().Pool
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.st(id)
	s.failures++

	switch {
	case status == 429:
		s.cooldownStage++
		base := cfg.Cooldown429.Std()
		d := base
		for i := 1; i < s.cooldownStage; i++ {
			d *= 2
			if d >= cfg.Cooldown429Max.Std() {
				d = cfg.Cooldown429Max.Std()
				break
			}
		}
		p.extendLocked(s, d)

	case status == 401 || status == 403:
		// token 失效：短冷却，等刷新或人工处理。
		p.extendLocked(s, cfg.Cooldown5xx.Std()*2)

	case status >= 500:
		p.extendLocked(s, cfg.Cooldown5xx.Std())

	default:
		// 请求本身有问题（400 等），不算账号故障，但连续多次仍然保护一下。
		if s.failures >= cfg.BreakerFailures {
			p.extendLocked(s, cfg.BreakerCooldown.Std())
			s.failures = 0
		}
	}
}

func (p *Pool) extendLocked(s *state, d time.Duration) {
	if d <= 0 {
		return
	}
	until := time.Now().Add(d)
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
}

// Pick 选一个账号；exclude 用于同一次请求内的重试换号。
func (p *Pool) Pick(exclude map[string]bool) (*Lease, error) {
	cfg := p.cfg.Load()
	now := time.Now()

	accounts := p.store.List()
	if len(accounts) == 0 {
		return nil, ErrNoAccount
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	type cand struct {
		acct     store.Account
		inflight int
		last     time.Time
	}
	var cands []cand
	cooling := 0
	busy := 0
	for _, a := range accounts {
		if a.Disabled || exclude[a.ID] {
			continue
		}
		s := p.st(a.ID)
		if now.Before(s.cooldownUntil) {
			cooling++
			continue
		}
		if s.inflight >= cfg.Pool.MaxInFlightAcct {
			busy++
			continue
		}
		cands = append(cands, cand{acct: a, inflight: s.inflight, last: s.lastPick})
	}
	if len(cands) == 0 {
		switch {
		case cooling > 0 || busy > 0:
			return nil, ErrAllBusy
		default:
			return nil, ErrNoAccount
		}
	}

	// 总并发闸门：所有账号在途数之和。
	total := 0
	for _, s := range p.state {
		total += s.inflight
	}
	if total >= cfg.Pool.MaxInFlight {
		return nil, ErrAllBusy
	}

	var chosen cand
	switch cfg.Pool.Strategy {
	case "round_robin":
		p.rr = (p.rr + 1) % len(cands)
		chosen = cands[p.rr]
	default:
		// weighted：在途少 + 最久未被选中的优先，取 top-3 后随机，
		// 避免所有请求在同一瞬间挤向同一个"最闲"账号。
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].inflight != cands[j].inflight {
				return cands[i].inflight < cands[j].inflight
			}
			return cands[i].last.Before(cands[j].last)
		})
		top := cands
		if len(top) > 3 {
			top = top[:3]
		}
		chosen = top[p.rnd.Intn(len(top))]
	}

	s := p.st(chosen.acct.ID)
	s.inflight++
	s.lastPick = now
	return &Lease{Account: chosen.acct, pool: p}, nil
}

// Stats 面板展示用的账号状态。
type Stats struct {
	ID            string    `json:"id"`
	Inflight      int       `json:"inflight"`
	CooldownUntil time.Time `json:"cooldown_until"`
	Failures      int       `json:"failures"`
	Available     bool      `json:"available"`
}

// Snapshot 返回每个账号的运行时状态。
func (p *Pool) Snapshot() map[string]Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make(map[string]Stats, len(p.state))
	for id, s := range p.state {
		out[id] = Stats{
			ID:            id,
			Inflight:      s.inflight,
			CooldownUntil: s.cooldownUntil,
			Failures:      s.failures,
			Available:     !now.Before(s.cooldownUntil),
		}
	}
	return out
}

// Reset 清除某账号的冷却（面板"恢复"按钮）。
func (p *Pool) Reset(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.state[id]; ok {
		s.cooldownUntil = time.Time{}
		s.failures = 0
		s.cooldownStage = 0
	}
}
