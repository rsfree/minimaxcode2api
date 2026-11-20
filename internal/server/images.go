package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/imageproto"
	"github.com/linguo2625469/minimaxcode2api/internal/media"
	"github.com/linguo2625469/minimaxcode2api/internal/modelcat"
	"github.com/linguo2625469/minimaxcode2api/internal/negcache"
	"github.com/linguo2625469/minimaxcode2api/internal/pool"
)

// 图片接口按 OpenAI 图像协议的 canonical 契约实现：
//
//	POST /v1/images/generations
//
// 只保留这一个入口：canonical 就是 JSON，图片引用用字符串（URL / data URI /
// 裸 base64），不引 multipart —— 少一个载体前门就少一套「哪些字段能传」的歧义，
// 也不用替客户端去公网取图。

const maxImageBody = 64 << 20

// imageSynthResponse 是上游 image_synthesize 的响应（槽位制）。
type imageSynthResponse struct {
	Summary        string `json:"summary"`
	TotalRequested int    `json:"total_requested"`
	TotalSuccess   int    `json:"total_success"`
	TotalFailed    int    `json:"total_failed"`
	SuccessItems   []struct {
		OutputFile     string `json:"output_file"`
		OutputURL      string `json:"output_url"`
		TextResponse   string `json:"text_response"`
		HasImageOutput bool   `json:"has_image_output"`
		CleanOSSKey    string `json:"clean_oss_key"`
		VisibleOSSKey  string `json:"visible_oss_key"`
	} `json:"success_items"`
	FailedItems []struct {
		OutputFile string `json:"output_file"`
		ErrCode    any    `json:"err_code"`
		ErrorMsg   string `json:"error_msg"`
	} `json:"failed_items"`
}

// handleImages 文生图 / 图生图（JSON 入口）。
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxImageBody))
	if err != nil {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "读取请求体失败: " + err.Error(),
		})
		return
	}
	req, perr := imageproto.Decode(raw)
	// model 只做类别校验：上游 image_synthesize **没有 model 参数**（见 README），
	// 带不带它的差别只是"客户端以为自己在选模型"。但对话/视频模型发到这里是实打实的
	// 配置错误，直接 400 指路；清单外的未知写法保持放行 —— 生态里不少客户端会写死
	// dall-e-3 / gpt-image-1，为一个装饰字段拒绝请求不值得。
	if perr == nil {
		if info, ok := modelcat.Lookup(req.Model); ok && info.Kind != modelcat.Image {
			perr = &imageproto.ProtocolError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Param: "model", Code: "invalid_model",
				Message: misrouteMessage(req.Model, info.Kind),
			}
		}
	}
	// 参考图归一：上游 image_synthesize 的 input_urls 只认 cdn_url 或内联
	// base64，任意公网 URL 会被上游拒掉（HTTP 200 + generation_failed，
	// 原文见 fetchmedia.go）。这里代劳下载 → data URI，与图像工具同一套 SSRF 防护。
	if perr == nil {
		req.Image, perr = s.inlineImageRefs(r.Context(), req.Image)
	}
	if perr == nil {
		var payload map[string]any
		var warnings []string
		// 默认档位来自配置：客户端没指定 size/quality/resolution 时用它。
		payload, warnings, perr = req.Build(s.Cfg.Load().Defaults.ImageResolution)
		if perr == nil {
			s.runImageGeneration(w, r, req, payload, warnings, start)
			return
		}
	}
	s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
	writeImageError(w, perr)
}

// maxImageRefRaw 是参考图内联的上限（原始字节）。
//
// 上游对 input_urls 里 base64 的硬上限没有文档；实测 1.05MB base64 可过。
// 取 8MiB 原图（≈10.7MB base64）而不是 maxInlineFetch 的 20MiB：参考图通常远小于此，
// 超过就明确报错，不去赌一个必然很慢的大请求。
const maxImageRefRaw = 8 << 20

// inlineImageRefs 把 image 字段的引用归一成上游能吃的形式。
//
//   - cdn.hailuoai.com 的地址 → 原样（上游自己的 CDN，省一次往返）
//   - 其它 http(s) → 下载（SSRF 防护）→ data URI
//   - data URI / 裸 base64 → 原样
//
// 这是「客户端只给 URL 也能用」的关键：以前这里直接透传，公网 URL 一律 502。
func (s *Server) inlineImageRefs(ctx context.Context, refs []string) ([]string, *imageproto.ProtocolError) {
	if len(refs) == 0 {
		return refs, nil
	}
	out := make([]string, 0, len(refs))
	for i, ref := range refs {
		src := strings.TrimSpace(ref)
		switch {
		case isUpstreamCDN(src):
			out = append(out, src)
		case strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://"):
			body, mime, err := s.fetchExternal(ctx, src, maxImageRefRaw)
			if err != nil {
				return nil, &imageproto.ProtocolError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Param: fmt.Sprintf("image[%d]", i), Code: "fetch_failed",
					Message: fmt.Sprintf("第 %d 张参考图下载失败: %v", i+1, err),
				}
			}
			out = append(out, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(body))
		default:
			out = append(out, src)
		}
	}
	return out, nil
}

// runImageGeneration 是两条前门共用的执行体。
func (s *Server) runImageGeneration(
	w http.ResponseWriter, r *http.Request,
	req *imageproto.Request, payload map[string]any, warnings []string, start time.Time,
) {
	// 负缓存：同一内容刚被审核拒过就直接回同样的 451，不再打上游 ——
	// 批量重放同一批 prompt 时，这一步省下的是真金白银的槽位与积分。
	cacheKey := imageNegKey(payload)
	if e, ok := s.ImgNeg.Get(cacheKey); ok {
		s.logMedia(r, req.Model, e.Status, start, errors.New(e.Message))
		w.Header().Set("X-Content-Cache", "hit")
		writeImageError(w, &imageproto.ProtocolError{
			Status: e.Status, Type: errorType(e.Status), Code: e.Code, Message: e.Message,
		})
		return
	}

	resp, perr := s.synthesizeImage(r.Context(), req, payload, start, r)
	if perr != nil {
		// 只缓存内容层面的终态拒绝；可重试错误进缓存等于自己制造故障。
		if perr.Status == http.StatusUnavailableForLegalReasons {
			s.ImgNeg.Put(cacheKey, negcache.Entry{
				Status: perr.Status, Code: perr.Code, Message: perr.Message,
			})
		}
		if perr.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "5")
		}
		writeImageError(w, perr)
		return
	}

	wantB64 := req.ResponseFormat == "b64_json"
	type datum struct {
		URL string `json:"url,omitempty"`
		B64 string `json:"b64_json,omitempty"`
	}
	data := make([]datum, 0, len(resp.SuccessItems))
	for _, it := range resp.SuccessItems {
		if !it.HasImageOutput || it.OutputURL == "" {
			continue
		}
		if wantB64 {
			body, _, ferr := s.fetchBytes(r.Context(), it.OutputURL)
			if ferr != nil {
				writeImageError(w, &imageproto.ProtocolError{
					Status: http.StatusBadGateway, Type: "upstream_error",
					Code: "download_failed", Message: ferr.Error(),
				})
				return
			}
			data = append(data, datum{B64: base64.StdEncoding.EncodeToString(body)})
		} else {
			data = append(data, datum{URL: it.OutputURL})
		}
	}
	if len(data) == 0 {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "empty_result", Message: "上游称成功但没有产物（可能被内容策略过滤）",
		})
		return
	}

	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	out := map[string]any{"created": time.Now().Unix(), "data": data}
	// 方舟协议的 usage 是**张数**口径（generated_images / input_images）。
	// output_tokens 不编造：上游响应里没有尺寸信息，算面积需要额外下载图片。
	out["usage"] = map[string]any{
		"generated_images": len(data),
		"input_images":     len(req.Image),
	}
	// warnings 是附加字段（协议允许透传附加键），只在非空时出现。
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, out)
}

// maxImageSoftRetries 是「上游槽位软失败」的补打次数。
//
// 上游出图是槽位制：HTTP 200 + success_items 为空 + failed_items 里写
// "please retry the same request shortly" 属于**常态抖动**，不是终态错误。
// 实测 6 路并发约 1/6、10 路并发约 1/8 会抽到，串行请求几乎不出现；同一条
// payload 原样重打即可成功（实测同一 prompt 首打 502、二打 200）。
// 所以宁可多打两次，也不要把可恢复的抖动当 502 透给客户端。
const maxImageSoftRetries = 3

// imageBusyPoll 是池满时重新取号的间隔。
const imageBusyPoll = 300 * time.Millisecond

// imageSoftRetryDelay 是软失败补打前的基准等待。变量而非常量：单测里调小。
//
// 必须带抖动：并发请求在同一时刻吃到软失败后，如果都等固定的 1.2s，补打会
// 再次同时打到上游，等于复现一次突发 —— 实测 10 路并发时两条补打日志相隔
// 10ms 落在同一秒，就是这么来的。
var imageSoftRetryDelay = 1500 * time.Millisecond

// imageSoftRetryWait 返回带抖动的补打等待（0.7~1.6 倍）。
func imageSoftRetryWait() time.Duration {
	return time.Duration(float64(imageSoftRetryDelay) * (0.7 + 0.9*rand.Float64()))
}

// statusClientClosed 是「客户端已断开」的记账码（nginx 惯例 499），只进日志。
const statusClientClosed = 499

// synthesizeImage 调上游出图，带两类重试：
//
//  1. 池满（ErrAllBusy）：等到 pool.max_wait 为止。图片是长任务（1K 约 20s），
//     突发并发下立刻回 429 会把本可成功的请求直接丢掉；排队等前一批释放槽位，
//     10 路突发的成功率从 70% 拉到接近 100%。全部账号都在冷却时不等 —— 那是
//     账号级故障，排队只会把客户端拖到超时。
//  2. 上游软失败：换号补打，最多 maxImageSoftRetries 次（见上）。
//
// 返回 (resp, nil) 表示拿到了 success_items；否则 perr 是终态错误。
func (s *Server) synthesizeImage(
	ctx context.Context, req *imageproto.Request, payload map[string]any,
	start time.Time, r *http.Request,
) (*imageSynthResponse, *imageproto.ProtocolError) {
	// 排队截止时间按「请求开始」算，不按重试次数算：否则池满时每次重试都能
	// 续命，客户端会被一路拖到 nginx 的 660s 读超时。
	waitUntil := start.Add(s.Cfg.Load().Pool.MaxWait.Std())
	softFails := 0

	for {
		raw, err := s.callMedia(ctx, media.ToolImageSynthesize, payload)
		if err != nil {
			if errors.Is(err, pool.ErrAllBusy) && time.Now().Before(waitUntil) && !s.Pool.AllCooling() {
				if !sleepCtx(ctx, imageBusyPoll) {
					s.logMedia(r, req.Model, statusClientClosed, start, ctx.Err())
					return nil, &imageproto.ProtocolError{
						Status: statusClientClosed, Type: "api_error",
						Code: "client_closed", Message: "客户端已断开",
					}
				}
				continue
			}
			s.logMedia(r, req.Model, http.StatusBadGateway, start, err)
			return nil, imageErrorFromMedia(err)
		}

		var resp imageSynthResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			s.logMedia(r, req.Model, http.StatusBadGateway, start, err)
			return nil, &imageproto.ProtocolError{
				Status: http.StatusBadGateway, Type: "upstream_error",
				Code: "bad_upstream", Message: "上游响应无法解析: " + err.Error(),
			}
		}

		// 上游把「模型拒答」表现为 success=false + text_response，而不是 HTTP 错误。
		if len(resp.SuccessItems) > 0 {
			return &resp, nil
		}

		msg := "图片生成失败"
		if len(resp.FailedItems) > 0 {
			msg = firstNonEmptyStr(resp.FailedItems[0].ErrorMsg, msg)
		}
		// 先判审核：拒答的文案里也可能带 retry 字样，重试既不解决问题还会重复计费。
		if !isContentRejection(msg) && softFails < maxImageSoftRetries && isTransientSlotFailure(msg) {
			softFails++
			log.Printf("图片槽位软失败，补打 %d/%d：%s", softFails, maxImageSoftRetries, msg)
			if !sleepCtx(ctx, imageSoftRetryWait()) {
				s.logMedia(r, req.Model, statusClientClosed, start, ctx.Err())
				return nil, &imageproto.ProtocolError{
					Status: statusClientClosed, Type: "api_error",
					Code: "client_closed", Message: "客户端已断开",
				}
			}
			continue
		}

		status := http.StatusBadGateway
		code := "generation_failed"
		if isContentRejection(msg) {
			// 451 Unavailable For Legal Reasons：内容层拦截的标准信号，
			// 客户端据此提示用户改内容，而不是去查 key 或重试。
			code, status = "content_filter", http.StatusUnavailableForLegalReasons
		}
		s.logMedia(r, req.Model, status, start, errors.New(msg))
		return nil, &imageproto.ProtocolError{
			Status: status, Type: errorType(status), Code: code, Message: msg,
		}
	}
}

// imageNegKey 用「上游请求体」算 key：prompt、比例、档位、参考图都在里面，
// 任一项不同就是另一条内容。encoding/json 对 map 的 key 排序输出，结果稳定。
func imageNegKey(payload map[string]any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// isTransientSlotFailure 判定上游软失败是否值得原样补打。
//
// 只认上游自己给出的重试邀请，不猜：把参数错/审核拒重打一遍既费时又可能
// 重复计费。上游原文形如
// "request failed; please retry the same request shortly, or try again later ..."。
func isTransientSlotFailure(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "please retry") || strings.Contains(m, "try again later")
}

// sleepCtx 等待 d，期间 ctx 取消则返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// writeImageError 输出四键错误信封（message / type / param / code）。
func writeImageError(w http.ResponseWriter, perr *imageproto.ProtocolError) {
	status := perr.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	typ := perr.Type
	if typ == "" {
		typ = errorType(status)
	}
	writeFourKeyError(w, status, typ, perr.Param, perr.Code, perr.Message)
}

// imageErrorFromMedia 按「谁的错」翻译上游错误。
//
// 401/403 是服务方凭证问题 → 502（客户端改请求没用）；429 保留退避信号；
// 402 是配额/限额触顶 → 402 + insufficient_quota，让运维一眼区分
// 「限额了」与「请求写错了」。限额通常会重置，客户端应稍后重试。
func imageErrorFromMedia(err error) *imageproto.ProtocolError {
	// 池的两种容量错误先认：它们不是"上游错误"，报 502 会让运维去查上游。
	if errors.Is(err, pool.ErrNoAccount) {
		return &imageproto.ProtocolError{
			Status: http.StatusServiceUnavailable, Type: "api_error",
			Code: "no_account", Message: err.Error(),
		}
	}
	if errors.Is(err, pool.ErrAllBusy) {
		return &imageproto.ProtocolError{
			Status: http.StatusTooManyRequests, Type: "api_error",
			Code: "all_accounts_busy", Message: err.Error(),
		}
	}
	var me *media.Error
	if !errors.As(err, &me) {
		return &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "upstream_unreachable", Message: err.Error(),
		}
	}
	switch {
	case me.IsContent():
		// 上游直接回 451（内容/法务拦截）：原样透出，不要包成 502。
		return &imageproto.ProtocolError{
			Status: http.StatusUnavailableForLegalReasons, Type: "invalid_request_error",
			Code: "content_filter", Message: me.Message,
		}
	case me.IsBilling():
		return &imageproto.ProtocolError{
			Status: http.StatusPaymentRequired, Type: "api_error",
			Code: "insufficient_quota", Message: billingHint(me.Message),
		}
	case me.IsRequest():
		return &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_request", Message: me.Message,
		}
	case me.IsAuth():
		return &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "api_error",
			Code: "upstream_auth", Message: "上游账号不可用: " + me.Message,
		}
	case me.Code == http.StatusTooManyRequests || me.HTTPStatus == http.StatusTooManyRequests:
		return &imageproto.ProtocolError{
			Status: http.StatusTooManyRequests, Type: "api_error",
			Code: "rate_limited", Message: me.Message,
		}
	}
	return &imageproto.ProtocolError{
		Status: http.StatusBadGateway, Type: "upstream_error",
		Code: "upstream_error", Message: me.Message,
	}
}

// isContentRejection 判定上游的"拒答"文案（审核类）。
func isContentRejection(msg string) bool {
	m := strings.ToLower(msg)
	for _, kw := range []string{"sensitive", "risk", "content security", "content policy", "审核", "违规"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

// handleVoices 暴露可用音色。
func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	raw, err := s.callMedia(r.Context(), media.ToolVoiceList, map[string]any{})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	var resp struct {
		AvailableVoices []struct {
			VoiceID   string `json:"voice_id"`
			VoiceName string `json:"voice_name"`
		} `json:"available_voices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(resp.AvailableVoices))
	for _, v := range resp.AvailableVoices {
		out = append(out, map[string]any{"id": v.VoiceID, "name": v.VoiceName, "object": "voice"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}
