package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
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
		if errors.As(cerr, &me) {
			// 计费 / 参数 / "稍后再查"：换号无用，重试还可能打乱已计费任务。
			// 但 raw 要带回去 —— 视频轮询的 503 里带着 status，客户端据它继续轮询。
			if me.IsBilling() || me.IsRequest() || me.IsPoll() {
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
			code = "account_unavailable"
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
func billingHint(msg string) string {
	return msg + "【提示】这是账号的 5 小时/周限额触顶，不是余额不足；" +
		"额度按窗口重置，重置后自动恢复（控制台可看剩余重置时间）"
}

// logMedia 记一条媒体请求日志（含来源 IP/UA）。
func (s *Server) logMedia(r *http.Request, model string, status int, start time.Time, err error) {
	e := reqlogEntry(r, model, status, start, err)
	e.ClientIP, e.UA = s.clientInfo(r)
	s.Logs.Add(e)
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
