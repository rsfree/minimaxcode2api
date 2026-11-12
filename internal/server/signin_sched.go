package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// startSigninScheduler 每天在 signin.at 自动签一次到。
//
// 为什么放进程内而不是只靠 crontab：
//   - 签到幂等（status=2 才领），重启、多副本、手动补签都不会领两次；
//   - 容器换机器/改时区/重建时，crontab 很容易和进程生命周期脱节；
//   - 面板上的「自动签到」开关必须能真正控制行为，而不是写着好看。
//
// 启动时先补一次，之后按天跑：宕机错过的当天，起来就能补上。
func (s *Server) startSigninScheduler() {
	go func() {
		// 等服务与账号池就绪，避免和启动瞬间的刷新抢 token 锁。
		time.Sleep(20 * time.Second)
		if s.Cfg.Load().Signin.Auto {
			s.runAutoSignin("启动补签")
		}

		for {
			cfg := s.Cfg.Load()
			if !cfg.Signin.Auto {
				time.Sleep(30 * time.Second)
				continue
			}
			loc := loadZone(cfg.Signin.Timezone)
			now := time.Now().In(loc)
			h, m, ok := parseClock(cfg.Signin.At)
			if !ok {
				log.Printf("签到: signin.at=%q 不是 HH:MM，自动签到暂停", cfg.Signin.At)
				time.Sleep(time.Hour)
				continue
			}
			next := nextSigninAt(now, h, m)

			// 分片睡：面板改配置（关掉/改时间）最多 1 分钟内生效，
			// 而不是"改完还要等一天"。
			if d := time.Until(next); d > time.Minute {
				chunk := d - time.Minute
				if chunk > time.Minute {
					chunk = time.Minute
				}
				time.Sleep(chunk)
				continue
			}

			// 到点前后各有一个 tick 会落进来，用分钟键去重。
			key := next.Format("2006-01-02T15:04")
			if s.signinLastRun() == key {
				time.Sleep(70 * time.Second)
				continue
			}
			s.markSigninRun(key)
			s.runAutoSignin("定时签到")
			// 跑完跨过当前分钟，避免同一次触发重入。
			time.Sleep(70 * time.Second)
		}
	}()
}

func (s *Server) runAutoSignin(trigger string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	results, err := s.SigninAccounts(ctx, "")
	if err != nil {
		log.Printf("签到(%s): %v", trigger, err)
		return
	}
	total, failed := 0, 0
	var parts []string
	for _, r := range results {
		if !r.OK {
			failed++
			continue
		}
		total += r.Points
		if r.Claimed {
			parts = append(parts, fmt.Sprintf("%s +%d", r.Label, r.Points))
		}
	}
	if len(results) == 0 {
		log.Printf("签到(%s): 池里还没有账号", trigger)
		return
	}
	detail := "今日已签"
	if len(parts) > 0 {
		detail = strings.Join(parts, "，")
	}
	log.Printf("签到(%s): %d 个账号，本次领取 %d 积分（%s），失败 %d", trigger, len(results), total, detail, failed)
}

// ---------- 小工具（纯函数，便于测试） ----------

func loadZone(name string) *time.Location {
	if strings.TrimSpace(name) == "" {
		name = "Asia/Shanghai"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	// 容器缺 tzdata 时兜底：中国无夏令时，固定 +8 与 IANA 等价。
	return time.FixedZone("CST", 8*3600)
}

// parseClock 解析 "HH:MM"（也容忍 "H:M"）。
func parseClock(v string) (hour, minute int, ok bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// nextSigninAt 返回「严格晚于 now」的下一个 HH:MM。
//
// 严格大于是关键：到点那一分钟里反复调用必须得到同一个明天的结果，
// 否则循环会在 00:10:00.999 反复重算成「现在」，把定时器打成死循环。
func nextSigninAt(now time.Time, hour, minute int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *Server) signinLastRun() string {
	s.signinMu.Lock()
	defer s.signinMu.Unlock()
	return s.signinRunKey
}

func (s *Server) markSigninRun(key string) {
	s.signinMu.Lock()
	s.signinRunKey = key
	s.signinMu.Unlock()
}
