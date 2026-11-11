// Package server HTTP 前门：OpenAI 兼容接口 + Anthropic 直通 + 面板挂载。
//
// 分层：handler 只负责「选号 → 取新鲜 token → 打上游 → 转协议 → 记账」，
// 协议细节都在 convert，账号状态都在 pool，重试只发生在拿到非 200 时
// （流式一旦开始写响应头就不能换号了，这是 HTTP 的硬约束）。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
	"github.com/linguo2625469/minimaxcode2api/internal/convert"
	"github.com/linguo2625469/minimaxcode2api/internal/oauth"
	"github.com/linguo2625469/minimaxcode2api/internal/pool"
	"github.com/linguo2625469/minimaxcode2api/internal/reqlog"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
	"github.com/linguo2625469/minimaxcode2api/internal/upstream"
	"github.com/linguo2625469/minimaxcode2api/internal/usage"
	"github.com/linguo2625469/minimaxcode2api/internal/videotask"
)

// Version 网关版本。
const Version = "0.2.1"

// Server 网关主对象。
type Server struct {
	Cfg   *config.Holder
	Store *store.Store
	Pool  *pool.Pool
	OAuth *oauth.Client
	Up    *upstream.Client
	Usage *usage.Tracker
	Logs  *reqlog.Log
	Start time.Time
	Mux   *http.ServeMux

	// refreshLocks 按账号串行化刷新，避免并发请求同时用同一个 refresh_token
	// 触发轮换，把彼此刷成 invalid_grant。
	refreshLocks sync.Map
	usageDirty   chan struct{}
	// VideoTasks 视频任务快照（id -> model/参数/状态）。上游查询要求传 model，
	// 而协议客户端轮询只带 id，所以必须在提交时记下来；落盘以便重启后仍可查。
	VideoTasks *videotask.Store
}

// New 组装服务。
func New(
	cfg *config.Holder,
	st *store.Store,
	pl *pool.Pool,
	oc *oauth.Client,
	up *upstream.Client,
	tr *usage.Tracker,
	lg *reqlog.Log,
) *Server {
	s := &Server{
		Cfg:        cfg,
		Store:      st,
		Pool:       pl,
		OAuth:      oc,
		Up:         up,
		Usage:      tr,
		Logs:       lg,
		Start:      time.Now(),
		Mux:        http.NewServeMux(),
		usageDirty: make(chan struct{}, 1),
	}
	// 视频任务快照落盘；打不开就退化成内存库（视频不是关键路径，不该阻塞启动）。
	videoPath := filepath.Join(cfg.Load().DataDir, "videotasks.json")
	if vs, err := videotask.Open(videoPath); err == nil {
		s.VideoTasks = vs
	} else {
		log.Printf("视频任务库打开失败（降级为内存）: %v", err)
		s.VideoTasks = videotask.NewMemory()
	}
	s.routes()
	go s.usageFlusher()
	return s
}

func (s *Server) routes() {
	s.Mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChatCompletions))
	s.Mux.HandleFunc("POST /v1/messages", s.auth(s.handleMessages))
	s.Mux.HandleFunc("POST /v1/messages/count_tokens", s.auth(s.handleCountTokens))
	s.Mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	// 媒体接口：上游是 /mavis/api/v1/mcp/*，鉴权与文本一致。
	s.Mux.HandleFunc("POST /v1/images/generations", s.auth(s.handleImages))
	// 视频走「异步视频生成协议」的 canonical 路径。同时挂 /v1 与 /api/v3：
	// 前者是本网关的惯例，后者让 Ark SDK 只需改 base_url 就能直接用。
	for _, prefix := range []string{"/v1", "/api/v3"} {
		base := prefix + "/contents/generations/tasks"
		s.Mux.HandleFunc("POST "+base, s.auth(s.createVideoTask))
		s.Mux.HandleFunc("GET "+base, s.auth(s.listVideoTasks))
		s.Mux.HandleFunc("GET "+base+"/{id}", s.auth(s.getVideoTask))
		s.Mux.HandleFunc("DELETE "+base+"/{id}", s.auth(s.deleteVideoTask))
	}
	s.Mux.HandleFunc("GET /v1/voices", s.auth(s.handleVoices))
	s.Mux.HandleFunc("POST /v1/audio/speech", s.auth(s.handleSpeech))
	s.Mux.HandleFunc("POST /v1/audio/transcriptions", s.auth(s.handleTranscriptions))
	s.Mux.HandleFunc("POST /v1/audio/music", s.auth(s.handleMusic))
	s.Mux.HandleFunc("GET /v1/models/{id}", s.auth(s.handleModel))
	s.Mux.HandleFunc("GET /healthz", s.handleHealth)
	s.Mux.HandleFunc("GET /status", s.auth(s.handleStatus))
	s.Mux.HandleFunc("OPTIONS /", s.handleOptions)
	// 兜底：没有匹配到任何路由时自己回 JSON 404。
	// 不注册它的话，Go 的 ServeMux 会因为 "OPTIONS /" 是唯一能匹配的 pattern
	// 而回纯文本 405，客户端会以为"路径存在但方法错了"，实际是路径根本不存在。
	s.Mux.HandleFunc("/", s.handleNotFound)
}

// Handler 返回带通用中间件的入口。
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.cors(s.Mux))
}

// ---------- 中间件 ----------

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, v)
				writeOpenAIError(w, http.StatusInternalServerError, "internal_error", fmt.Sprintf("%v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-beta")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Max-Age", "86400")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleNotFound 是未匹配路由的兜底，回 JSON 而不是 Go 的纯文本。
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, http.StatusNotFound, "unknown_route",
		"没有这个接口："+r.Method+" "+r.URL.Path+"（见 GET /v1/models 与 README 的路由清单）")
}

// auth 校验对外 API Key（Bearer 或 x-api-key 均可，方便各种 SDK 直连）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.Cfg.Load().APIKey
		if key == "" {
			next(w, r)
			return
		}
		got := bearerToken(r)
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		if !constantEq(got, key) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "无效的 API Key")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func constantEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ---------- 基础端点 ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "minimaxcode2api",
		"version": Version,
		"uptime":  int64(time.Since(s.Start).Seconds()),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	accounts := s.Store.List()
	enabled := 0
	for _, a := range accounts {
		if !a.Disabled {
			enabled++
		}
	}
	snap := s.Pool.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "minimaxcode2api",
		"version":  Version,
		"uptime":   int64(time.Since(s.Start).Seconds()),
		"accounts": len(accounts),
		"enabled":  enabled,
		"today":    s.Usage.Today(),
		"all_time": s.Usage.AllTime(),
		"pool":     snap,
	})
}

// resolveModel 把别名解析成上游真实模型名。
func (s *Server) resolveModel(name string) string {
	cfg := s.Cfg.Load()
	name = strings.TrimSpace(name)
	if name == "" {
		return cfg.Defaults.Model
	}
	if v, ok := cfg.Models.Aliases[name]; ok {
		return v
	}
	// 大小写不敏感再试一次（客户端经常把模型名转小写）。
	for k, v := range cfg.Models.Aliases {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	for _, m := range append(cfg.Models.Expose, convert.KnownModels()...) {
		if strings.EqualFold(m, name) {
			return m
		}
	}
	return name
}

func (s *Server) isHidden(model string) bool {
	for _, h := range s.Cfg.Load().Models.Hidden {
		if h == model {
			return true
		}
	}
	return false
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// 扩展字段，方便客户端识别能力。
	ContextWindow int  `json:"context_window,omitempty"`
	MaxOutput     int  `json:"max_output,omitempty"`
	Reasoning     bool `json:"reasoning,omitempty"`
}

func (s *Server) modelList() []modelObject {
	cfg := s.Cfg.Load()
	seen := map[string]bool{}
	var out []modelObject
	add := func(id string) {
		if id == "" || seen[id] || s.isHidden(id) {
			return
		}
		seen[id] = true
		cap, known := convert.CapabilityOf(id)
		m := modelObject{ID: id, Object: "model", Created: 1735689600, OwnedBy: "minimax"}
		if known {
			m.ContextWindow = cap.ContextWindow
			m.MaxOutput = cap.MaxOutput
			m.Reasoning = true
		}
		out = append(out, m)
	}
	for _, id := range cfg.Models.Expose {
		add(id)
	}
	for _, id := range convert.KnownModels() {
		add(id)
	}
	return out
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := s.modelList()
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, m := range s.modelList() {
		if m.ID == id {
			writeJSON(w, http.StatusOK, m)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "model_not_found", "模型不存在: "+id)
}

// handleCountTokens 本地粗估：真正的 count_tokens 上游不提供（405），
// 用字符数/2.2 近似——够客户端做上下文裁剪，不承诺精确。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	var body struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var chars int
	for _, raw := range append([][]byte{body.System}, toRawSlice(body.Messages)...) {
		chars += len(raw)
	}
	for _, raw := range body.Tools {
		chars += len(raw)
	}
	// JSON 结构本身占字符，按经验折半再除 2.2。
	est := int(float64(chars/2) / 2.2)
	if est < 1 {
		est = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": est})
}

func toRawSlice(in []json.RawMessage) [][]byte {
	out := make([][]byte, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// ---------- 响应工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeOpenAIError 输出 OpenAI 形态错误。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errorType(status),
			// param 是 OpenAI 错误对象的固定成员，缺失会让严格的 SDK 反序列化报警。
			// 网关层拿不到字段级的形参名，固定 null 比编一个更诚实。
			"param": nil,
			"code":  code,
		},
	})
}

// writeFourKeyError 输出协议的四键错误信封（message / type / param / code）。
//
// 图像与视频原本各写一份几乎相同的实现，只差 type 的兜底规则 —— 这种重复
// 已经让 param 写歪过一次，所以统一到这里，兜底规则仍由调用方给。
func writeFourKeyError(w http.ResponseWriter, status int, typ, param, code, msg string) {
	if status == 0 {
		status = http.StatusBadRequest
	}
	if typ == "" {
		typ = errorType(status)
	}
	var p any
	if param != "" {
		p = param
	}
	var c any
	if code != "" {
		c = code
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   p,
			"code":    c,
		},
	})
}

// writeAnthropicError 输出 Anthropic 形态错误（/v1/messages 直通使用）。
func writeAnthropicError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": code, "message": msg},
	})
}

func errorType(status int) string {
	switch {
	case status == 400:
		return "invalid_request_error"
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status == http.StatusPaymentRequired:
		// 额度触顶：请求本身没写错，重试也不会好 —— 归 api_error 而不是
		// invalid_request_error，否则客户端会去翻自己的参数。
		return "api_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// upstreamStatus 把上游错误映射成对下游更语义化的状态码。
func upstreamStatus(err error) (int, string) {
	var apiErr *upstream.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.AuthError():
			// 下游的 sk-mmx 已经过了鉴权，失效的是**网关自己的**上游账号。
			// 回 401 会让客户端以为自己 key 错了（重配、换 key 都无救），
			// 这是典型的网关侧故障 —— 502 + upstream_auth 才排得动障。
			return http.StatusBadGateway, "upstream_auth"
		case apiErr.Status == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "upstream_rate_limited"
		case apiErr.Status >= 400 && apiErr.Status < 500:
			// 上游认为请求非法，原样透出状态更利于客户端修正。
			return apiErr.Status, "upstream_bad_request"
		default:
			return http.StatusBadGateway, "upstream_error"
		}
	}
	if errors.Is(err, pool.ErrNoAccount) {
		return http.StatusServiceUnavailable, "no_account"
	}
	if errors.Is(err, pool.ErrAllBusy) {
		return http.StatusTooManyRequests, "all_accounts_busy"
	}
	if errors.Is(err, errNoRefreshToken) {
		return http.StatusUnauthorized, "token_expired"
	}
	return http.StatusBadGateway, "upstream_error"
}

// usageFlusher 每 5 秒把用量落盘一次，避免每条请求都写文件。
func (s *Server) usageFlusher() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		select {
		case <-s.usageDirty:
			if err := s.Usage.Save(); err != nil {
				log.Printf("[usage] save: %v", err)
			}
			if err := s.Store.Save(); err != nil {
				log.Printf("[accounts] save: %v", err)
			}
		default:
		}
	}
}

func (s *Server) markUsageDirty() {
	select {
	case s.usageDirty <- struct{}{}:
	default:
	}
}
