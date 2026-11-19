package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
)

func newTestPool(t *testing.T, ids ...string) (*Pool, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	holder := config.NewHolder(cfg)
	st, err := store.Open(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for _, id := range ids {
		if err := st.Add(&store.Account{ID: id, AccessToken: "tok-" + id}); err != nil {
			t.Fatalf("store.Add: %v", err)
		}
	}
	return New(holder, st), cfg
}

// cooldownLeft 读该账号剩余冷却时长（同包测试可直接看 state）。
func cooldownLeft(p *Pool, id string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.state[id]
	if !ok {
		return 0
	}
	return time.Until(s.cooldownUntil)
}

// 402（额度不足）必须让账号进冷却，且时长按「额度」口径走，不是 5xx 的 30 秒。
//
// 这是曾经的缺口：402 走 default 分支，只累积 failures，账号照常被选中，
// 于是每个请求都撞同一面墙，直到人工发现。
func TestBillingFailureCoolsAccount(t *testing.T) {
	p, cfg := newTestPool(t, "a1")
	want := cfg.Pool.CooldownBilling.Std()

	p.Failure("a1", 402)
	got := cooldownLeft(p, "a1")
	if got < want-time.Second || got > want+time.Second {
		t.Fatalf("402 冷却 = %v，期望约 %v", got, want)
	}

	// 冷却中的账号不能再被选中。
	if _, err := p.Pick(nil); err != ErrAllBusy {
		t.Fatalf("冷却中应返回 ErrAllBusy，实际 %v", err)
	}
}

// 连续 402 指数退避并封顶；一次成功立刻清零。
func TestBillingBackoffAndReset(t *testing.T) {
	p, cfg := newTestPool(t, "a1")
	base := cfg.Pool.CooldownBilling.Std()
	// 上限是「较长时间跨度」的保护，不能反过来比 base 还短。
	maxD := cfg.Pool.CooldownBillingMax.Std()
	if maxD < base {
		t.Fatalf("CooldownBillingMax(%v) 小于 CooldownBilling(%v)", maxD, base)
	}

	steps := []time.Duration{base, 2 * base, 4 * base, 8 * base}
	for i, want := range steps {
		p.Failure("a1", 402)
		capped := want
		if capped > maxD {
			capped = maxD
		}
		got := cooldownLeft(p, "a1")
		if got < capped-time.Second || got > capped+time.Second {
			t.Fatalf("第 %d 次 402 冷却 = %v，期望约 %v", i+1, got, capped)
		}
	}

	// 一直失败也不会超过上限。
	for i := 0; i < 10; i++ {
		p.Failure("a1", 402)
	}
	if got := cooldownLeft(p, "a1"); got > maxD+time.Second {
		t.Fatalf("冷却超过上限: %v > %v", got, maxD)
	}

	p.Success("a1")
	if got := cooldownLeft(p, "a1"); got > time.Second {
		t.Fatalf("成功后应清冷却，实际还剩 %v", got)
	}
}

// 429 与 402 必须是两条独立的退避阶梯。
// 共用一个 stage 的后果：一次频控 + 一次额度不足就跳到 2 倍，
// 把「刚打满的账号」估成「已经打满两轮」，恢复时间越拖越久。
func TestBillingAndRateLimitStagesIndependent(t *testing.T) {
	p, cfg := newTestPool(t, "a1")

	p.Failure("a1", 429)
	p.Failure("a1", 402)

	p.mu.Lock()
	s := p.state["a1"]
	stage429, stage402 := s.cooldownStage, s.billingStage
	p.mu.Unlock()

	if stage429 != 1 || stage402 != 1 {
		t.Fatalf("阶梯计数 = 429:%d 402:%d，都应为 1", stage429, stage402)
	}

	// 冷却取两者较大值：402 的 5 分钟 > 429 的 1 分钟。
	want := cfg.Pool.CooldownBilling.Std()
	if got := cooldownLeft(p, "a1"); got < want-time.Second || got > want+time.Second {
		t.Fatalf("冷却 = %v，应取 402 的 %v", got, want)
	}
}

// backoff 的边界：stage=0/1 取 base，超限返回 max，非正 base 返回 0。
func TestBackoffEdges(t *testing.T) {
	base, max := time.Minute, 10*time.Minute
	for _, c := range []struct {
		stage int
		want  time.Duration
	}{
		{0, base}, {1, base}, {2, 2 * base}, {3, 4 * base}, {4, 8 * base}, {5, max}, {99, max},
	} {
		if got := backoff(base, max, c.stage); got != c.want {
			t.Errorf("backoff(stage=%d) = %v，期望 %v", c.stage, got, c.want)
		}
	}
	if got := backoff(0, max, 3); got != 0 {
		t.Errorf("base=0 应返回 0，实际 %v", got)
	}
}
