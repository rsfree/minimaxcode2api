package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
	"github.com/linguo2625469/minimaxcode2api/internal/pool"
	"github.com/linguo2625469/minimaxcode2api/internal/reqlog"
)

// mediaBase 媒体网关地址：默认从文本上游地址推导
// （.../mavis/api/v1/llm/v1 -> .../mavis/api/v1/mcp），也可用 media_base_url 覆盖。
func (s *Server) mediaBase() string {
	cfg := s.Cfg.Load()
	if v := strings.TrimSpace(cfg.Upstream.MediaBaseURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	base := strings.TrimRight(cfg.Upstream.BaseURL, "/")
	if i := strings.Index(base, "/llm/"); i > 0 {
		return base[:i] + "/mcp"
	}
	return base + "/mcp"
}

func (s *Server) mediaClient() *media.Client {
	cfg := s.Cfg.Load()
	return media.New(time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second)
}

// nonIdempotentTool 判断工具是否可能"已经生成并计费"。
//
// 语音合成、语音转写、音乐生成都属于这一类：请求发出后上游可能已经产出结果，
// 只是响应在网络上丢失/超时。此时换号重试等于再扣一次费，必须直接返回错误。
func nonIdempotentTool(tool string) bool {
	switch tool {
	case media.ToolSpeech, media.ToolTranscribe, media.ToolMusic:
		return true
	}
	return false
}

// callMedia 选号 → 取新鲜 token → 打媒体网关，按上游语义决定要不要换号。
//
// 与文本链路的差别：媒体接口用 body 里的 code 表达业务错误，且 402（计费）
// 和 400（参数）换号也没用 —— 这两种必须立刻返回，否则会把一次明确的报错
// 放大成 N 次无效上游调用。
func (s *Server) callMedia(ctx context.Context, tool string, payload any) ([]byte, error) {
	const maxAttempts = 3
	exclude := map[string]bool{}
	var lastRaw []byte
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		lease, err := s.Pool.Pick(exclude)
		if err != nil {
			if lastErr != nil {
				return lastRaw, lastErr
			}
			return nil, err
		}
		acct := lease.Account

		fresh, ferr := s.ensureFresh(ctx, acct, false)
		if ferr != nil {
			lease.Release()
			s.Pool.Failure(acct.ID, http.StatusUnauthorized)
			exclude[acct.ID] = true
			lastErr = ferr
			continue
		}

		raw, cerr := s.mediaClient().Call(ctx, s.mediaBase(), fresh.AccessToken, tool, payload)
		lease.Release()
		if cerr == nil {
			s.Pool.Success(fresh.ID)
			s.Store.TouchUsage(fresh.ID, 0, 0)
			return raw, nil
		}
		lastRaw, lastErr = raw, cerr

		var me *media.Error
		isMediaErr := errors.As(cerr, &me)
		// 非幂等工具（合成/转写/音乐）的"未知结果"绝不换号重试：
		// 网络错误、5xx、超时都可能已经在上游生成并计费，重试会重复扣费
		// （audio-adapter 规范的「非幂等请求不自动重试」）。
		// 认证失败、429、400、402、451 属于"确定没生成"，换号是安全的。
		if nonIdempotentTool(tool) {
			if !isMediaErr {
				return raw, cerr
			}
			safeToRetry := me.IsAuth() || me.IsBilling() || me.IsRequest() ||
				me.IsPoll() || me.IsContent() ||
				me.HTTPStatus == http.StatusTooManyRequests || me.Code == http.StatusTooManyRequests
			if !safeToRetry {
				return raw, cerr
			}
		}
		if isMediaErr {
			// 计费 / 参数 / "稍后再查"：换号无用，重试还可能打乱已计费任务。
			// 但 raw 要带回去 —— 视频轮询的 503 里带着 status，客户端据它继续轮询。
			if me.IsContent() {
				// 内容拦截是请求层面的终态错误：换号只会把同一条 prompt
				// 再打一遍（还占一次槽位），也不该记在账号头上。
				return raw, cerr
			}
			if me.IsBilling() {
				// 额度不足是**账号级**状态：必须把该账号打进冷却，否则下一个请求
				// 立刻又会 Pick 到它，继续 402，直到人工发现。
				// 仍然立即返回而不换号重试 —— 同一 plan 的额度换号也是同一个池子，
				// 重试只会多打一次上游。
				s.Pool.Failure(acct.ID, http.StatusPaymentRequired)
				return raw, cerr
			}
			if me.IsRequest() || me.IsPoll() {
				return raw, cerr
			}
			if me.IsAuth() {
				// 先对同账号强制刷新再打一次（token 过期最常见）。
				if refreshed, rerr := s.ensureFresh(ctx, acct, true); rerr == nil {
					if raw2, e2 := s.mediaClient().Call(ctx, s.mediaBase(), refreshed.AccessToken, tool, payload); e2 == nil {
						s.Pool.Success(refreshed.ID)
						s.Store.TouchUsage(refreshed.ID, 0, 0)
						return raw2, nil
					}
				}
				s.Pool.Failure(acct.ID, http.StatusUnauthorized)
			} else {
				s.Pool.Failure(acct.ID, me.HTTPStatus)
			}
		}
		exclude[acct.ID] = true
	}
	return lastRaw, lastErr
}

// writeMediaError 把上游错误映射成对外的 OpenAI 风格错误。
func writeMediaError(w http.ResponseWriter, err error) {
	// 池的两种容量错误都要对齐文本链路，否则客户端会把冷却当成 5xx 重试风暴：
	//   池里没账号（全删/全禁用）→ 503 no_account
	//   账号全在冷却或在途满   → 429 all_accounts_busy
	if errors.Is(err, pool.ErrNoAccount) {
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}
	if errors.Is(err, pool.ErrAllBusy) {
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusTooManyRequests, "all_accounts_busy", err.Error())
		return
	}
	status := media.StatusOf(err)
	code := "upstream_error"
	var me *media.Error
	if errors.As(err, &me) {
		switch {
		case me.IsBilling():
			code = "insufficient_quota"
		case me.IsRequest():
			code = "invalid_request"
		case me.IsAuth():
			code = "upstream_auth"
		}
	}
	msg := err.Error()
	if me != nil && me.Message != "" {
		msg = me.Message
		if me.IsBilling() {
			msg = billingHint(msg)
		}
	}
	writeOpenAIError(w, status, code, msg)
}

// fetchBytes 下载产物。上限 64MB（4K 图与短视频都在这个量级内）。
func (s *Server) fetchBytes(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.mediaClient().HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New("下载产物失败: HTTP " + resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	return body, sniffMime(body, resp.Header.Get("Content-Type")), nil
}

// sniffMime 按 magic bytes 判类型。
//
// 这不是洁癖：实测上游把 JPEG 标成 image/png 下发（文件名也是 .png），
// 直接透传 header 会让客户端把 JPEG 当 PNG 解析而失败。
func sniffMime(b []byte, fallback string) string {
	head := b
	if len(head) > 16 {
		head = head[:16]
	}
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav"
	case len(b) >= 4 && string(b[:3]) == "ID3":
		return "audio/mpeg"
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "audio/mpeg"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		return "video/mp4"
	}
	if fallback != "" {
		return fallback
	}
	return "application/octet-stream"
}

// billingHint 给 402 补一句"这是限额不是余额"。
//
// 上游原文是 "plan balance has run out … need to top up"，但控制台实际显示的是
// 「5 小时限额 100% / 周限额 10%，按窗口重置」。照抄原文会把运维带向"去充值"，
// 而真实处置是等窗口重置（或降频）。
// creditCostRe 从 402 原文里抠出 "this task costs 229 credits" 的数额。
var creditCostRe = regexp.MustCompile(`costs?\s+(\d+)\s+credits`)

// billingHint 给 402 补一句**与上游口径一致**的提示。
//
// 这里踩过一次：早先不区分场景，一律加"这是限额触顶，不是余额不足"。
// 但视频 402 的原文是 "insufficient allowance or credits … costs 280 credits"，
// 也就是**真的缺积分**；再加"不是余额不足"等于把用户往反方向带。
// 所以按上游措辞分流，绝不覆盖它本来的判断。
func billingHint(msg string) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "plan balance"):
		// 图片/语音这类：控制台显示的是 5 小时/周窗口额度，按窗口重置。
		return msg + "【提示】这是账号的 5 小时/周限额触顶，额度按窗口重置后自动恢复" +
			"（控制台可看剩余重置时间），无需充值"
	case strings.Contains(low, "credit"):
		// 视频这类：按 credits 计费。上游 402 原文通常带 "this task costs N credits"，
		// 直接把 N 摘出来，比塞一个写死的样例价（Hailuo 6s/720p = 280）准 —— 实测
		// H3-Max 5s/480P 是 229，同一个提示会把人带偏。
		hint := "【提示】这是积分不足："
		if m := creditCostRe.FindStringSubmatch(msg); m != nil {
			hint += "本次任务需 " + m[1] + " credits（上游原文）。"
		} else {
			hint += "视频按 credits 计费。"
		}
		return msg + hint + "请补充积分，或确认套餐档位是否覆盖该型号"
	default:
		return msg
	}
}

// logMedia 记一条媒体请求日志（含来源 IP/UA）。
func (s *Server) logMedia(r *http.Request, model string, status int, start time.Time, err error) {
	e := reqlogEntry(r, model, status, start, err)
	e.ClientIP, e.UA = s.clientInfo(r)
	s.Logs.Add(e)
	// 媒体调用同时进用量统计（次数口径）。以前只写 reqlog，面板用量页
	// 看不到图片/视频，用户会以为网关没统计。
	if s.Usage != nil {
		s.Usage.RecordMedia(firstNonEmptyStr(model, mediaKind(r.URL.Path)),
			time.Since(start), status >= http.StatusBadRequest)
		s.markUsageDirty()
	}
}

// mediaKind 在 model 缺失时按路径归类，保证失败调用也有可读的桶名。
func mediaKind(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/images"):
		return "image"
	case strings.HasPrefix(path, "/v1/videos"), strings.HasPrefix(path, "/v1/contents"):
		return "video"
	case strings.HasPrefix(path, "/v1/audio"):
		return "audio"
	default:
		return "media"
	}
}

// reqlogEntry 组一条媒体请求日志。
//
// 媒体调用没有 token 用量（上游不返回），所以 Prompt/Output 都为 0；
// 面板里靠 Path 与 Duration 区分调用类型。
func reqlogEntry(r *http.Request, model string, status int, start time.Time, err error) reqlog.Entry {
	e := reqlog.Entry{
		Time:     start,
		Method:   r.Method,
		Path:     r.URL.Path,
		Model:    model,
		Status:   status,
		Duration: time.Since(start).Milliseconds(),
	}
	if err != nil {
		e.Error = err.Error()
	}
	return e
}
