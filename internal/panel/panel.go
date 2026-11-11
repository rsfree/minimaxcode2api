// Package panel Web 管理台：账号运维、用量、日志、配置热改。
//
// 鉴权口径与对外 API 保持一致：用同一个 api_key（或单独设置的 admin_password）。
// 面板是自用工具，不做多用户/RBAC——那会把一个单人运维面板做成半成品后台。
package panel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
)

// Deps 面板依赖。
type Deps struct {
	Cfg     *config.Holder
	Store   *store.Store
	Pool    *pool.Pool
	OAuth   *oauth.Client
	Up      *upstream.Client
	Usage   *usage.Tracker
	Logs    *reqlog.Log
	Version string
	Start   time.Time
}

type Panel struct {
	d Deps

	mu     sync.Mutex
	device map[string]*oauth.DeviceAuth
}

func New(d Deps) *Panel {
	return &Panel{d: d, device: map[string]*oauth.DeviceAuth{}}
}

// Mount 把面板路由挂到主 mux。
func (p *Panel) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel/{$}", p.serveIndex)
	mux.HandleFunc("GET /panel/app.js", p.serveJS)
	mux.HandleFunc("GET /panel/style.css", p.serveCSS)
	mux.HandleFunc("GET /panel/favicon.svg", p.serveFavicon)

	mux.HandleFunc("POST /panel/api/login", p.login)
	mux.HandleFunc("GET /panel/api/overview", p.authed(p.overview))
	mux.HandleFunc("GET /panel/api/accounts", p.authed(p.accounts))
	mux.HandleFunc("POST /panel/api/accounts/device/start", p.authed(p.deviceStart))
	mux.HandleFunc("GET /panel/api/accounts/device/poll", p.authed(p.devicePoll))
	mux.HandleFunc("POST /panel/api/accounts/import", p.authed(p.importAccount))
	mux.HandleFunc("POST /panel/api/accounts/{id}/toggle", p.authed(p.accountToggle))
	mux.HandleFunc("POST /panel/api/accounts/{id}/remove", p.authed(p.accountRemove))
	mux.HandleFunc("POST /panel/api/accounts/{id}/reset", p.authed(p.accountReset))
	mux.HandleFunc("POST /panel/api/accounts/{id}/label", p.authed(p.accountRename))
	mux.HandleFunc("POST /panel/api/accounts/{id}/test", p.authed(p.accountTest))
	mux.HandleFunc("GET /panel/api/usage", p.authed(p.usageSeries))
	mux.HandleFunc("GET /panel/api/logs", p.authed(p.logs))
	mux.HandleFunc("GET /panel/api/models", p.authed(p.models))
	mux.HandleFunc("GET /panel/api/config", p.authed(p.getConfig))
	mux.HandleFunc("POST /panel/api/config", p.authed(p.setConfig))
	mux.HandleFunc("POST /panel/api/probe", p.authed(p.probe))
}

// ---------- 鉴权 ----------

func (p *Panel) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := p.d.Cfg.Load().AdminSecret()
		if want == "" {
			next(w, r)
			return
		}
		got := bearer(r)
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		if !eq(got, want) {
			writeErr(w, http.StatusUnauthorized, "invalid_key", "口令不正确")
			return
		}
		next(w, r)
	}
}

// login 校验口令，前端把 key 存在 localStorage。
func (p *Panel) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	want := p.d.Cfg.Load().AdminSecret()
	if want == "" || !eq(strings.TrimSpace(body.Key), want) {
		writeErr(w, http.StatusUnauthorized, "invalid_key", "口令不正确")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 概览 ----------

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	cfg := p.d.Cfg.Load()
	accounts := p.d.Store.List()
	enabled, cooling := 0, 0
	snap := p.d.Pool.Snapshot()
	for _, a := range accounts {
		if a.Disabled {
			continue
		}
		enabled++
		if s, ok := snap[a.ID]; ok && !s.Available {
			cooling++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        p.d.Version,
		"uptime_seconds": int64(time.Since(p.d.Start).Seconds()),
		"accounts": map[string]any{
			"total":   len(accounts),
			"enabled": enabled,
			"cooling": cooling,
		},
		"today":     p.d.Usage.Today(),
		"all_time":  p.d.Usage.AllTime(),
		"models":    cfg.Models.Expose,
		"base_url":  cfg.Upstream.BaseURL,
		"listen":    cfg.Listen,
		"endpoints": []string{"/v1/chat/completions", "/v1/messages", "/v1/models"},
		"strategy":  cfg.Pool.Strategy,
		"logs":      p.d.Logs.Len(),
	})
}

// ---------- 账号 ----------

type accountView struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	UserID       string `json:"user_id"`
	MaskedToken  string `json:"masked_token"`
	HasRefresh   bool   `json:"has_refresh"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresInSec int64  `json:"expires_in_sec"`
	Expired      bool   `json:"expired"`
	Disabled     bool   `json:"disabled"`
	Note         string `json:"note"`
	LastError    string `json:"last_error"`
	CreatedAt    string `json:"created_at"`
	LastUsedAt   string `json:"last_used_at"`
	RequestCount int64  `json:"request_count"`
	PromptTokens int64  `json:"prompt_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Inflight     int    `json:"inflight"`
	Available    bool   `json:"available"`
	CooldownLeft int64  `json:"cooldown_left_sec"`
}

// viewOf 把账号实体转成对外视图；凭证只出掩码，明文永远不离开进程。
func viewOf(a store.Account, snap map[string]pool.Stats, now time.Time) accountView {
	v := accountView{
		ID:           a.ID,
		Label:        a.Label,
		UserID:       a.UserID,
		MaskedToken:  mask(a.AccessToken),
		HasRefresh:   a.RefreshToken != "",
		Disabled:     a.Disabled,
		Note:         a.Note,
		LastError:    a.LastError,
		CreatedAt:    a.CreatedAt,
		LastUsedAt:   a.LastUsedAt,
		RequestCount: a.RequestCount,
		PromptTokens: a.PromptTokens,
		OutputTokens: a.OutputTokens,
		Available:    !a.Disabled,
	}
	if !a.ExpiresAt().IsZero() {
		v.ExpiresAt = a.ExpiresAt().Local().Format("2006-01-02 15:04:05")
		v.ExpiresInSec = int64(a.ExpiresAt().Sub(now).Seconds())
		v.Expired = a.Expired(now)
	}
	if s, ok := snap[a.ID]; ok {
		v.Inflight = s.Inflight
		if !s.Available {
			v.Available = false
			v.CooldownLeft = int64(time.Until(s.CooldownUntil).Seconds())
		}
	}
	return v
}

func (p *Panel) accounts(w http.ResponseWriter, r *http.Request) {
	list := p.d.Store.List()
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	snap := p.d.Pool.Snapshot()
	now := time.Now()
	out := make([]accountView, 0, len(list))
	for _, a := range list {
		out = append(out, viewOf(a, snap, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// deviceStart 发起设备授权。
func (p *Panel) deviceStart(w http.ResponseWriter, r *http.Request) {
	d, err := p.d.OAuth.StartDeviceAuth()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "device_start_failed", err.Error())
		return
	}
	p.mu.Lock()
	// 顺手清理过期会话，避免长跑进程里越积越多。
	for code, old := range p.device {
		if old.Expired() {
			delete(p.device, code)
		}
	}
	p.device[d.DeviceCode] = d
	p.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               d.DeviceCode,
		"user_code":                 d.UserCode,
		"verification_uri":          d.VerificationURI,
		"verification_uri_complete": d.VerificationURIComplete,
		"expires_in":                d.ExpiresIn,
		"interval":                  d.Interval,
	})
}

// devicePoll 轮询一次授权结果。
func (p *Panel) devicePoll(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing_code", "缺少 code")
		return
	}
	p.mu.Lock()
	d, ok := p.device[code]
	p.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_code", "授权会话不存在或已过期")
		return
	}
	if d.Expired() {
		p.mu.Lock()
		delete(p.device, code)
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "授权已过期，请重新发起"})
		return
	}

	tok, status, err := p.d.OAuth.PollDeviceToken(d)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	if tok == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
		return
	}

	acct := p.saveToken(tok, "")
	p.mu.Lock()
	delete(p.device, code)
	p.mu.Unlock()
	// 凭证只回掩码：面板响应可能被截图/转发，明文 token 不该出现在线路上。
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"account": viewOf(acct, p.d.Pool.Snapshot(), time.Now()),
	})
}

// saveToken 把一次授权结果落成账号。
func (p *Panel) saveToken(tok *oauth.Token, label string) store.Account {
	cfg := p.d.Cfg.Load()
	id := tok.Subject
	if id == "" {
		id = "acct-" + randomID(10)
	}
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()
	// 没有用户可读标识时（access_token 不是 JWT、也没填备注），用 ID 尾号拼一个
	// 可读标签，避免面板里一排账号全叫 "acct-xxxx"。
	defLabel := tok.Subject
	if defLabel == "" && len(id) > 4 {
		defLabel = "MiniMax 账号·" + id[len(id)-4:]
	}
	a := &store.Account{
		ID:           id,
		Label:        firstNonEmpty(label, defLabel, id),
		UserID:       tok.Subject,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAtMs:  expiresAt,
		Scopes:       tok.Scopes,
		ClientID:     cfg.OAuth.ClientID,
		Audience:     cfg.OAuth.Audience,
	}
	_ = p.d.Store.Add(a)
	p.d.Pool.Reset(id)
	out, _ := p.d.Store.Get(id)
	return out
}

// importAccount 手工导入 refresh_token / access_token。
//
// 关键取舍：**只要给了 access_token 就直接用，绝不去轮换 refresh_token。**
// 上游对 refresh_token 做了单次使用 + reuse detection：同一个 refresh_token
// 被网关和桌面客户端同时使用，会让整个 token family 被吊销（实测客户端本地
// 凭据会被清空、双方一起 401）。只有「仅给了 refresh_token」时，才不得不
// 刷一次换出 access_token。
func (p *Panel) importAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
		Label        string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	body.RefreshToken = strings.TrimSpace(body.RefreshToken)
	body.AccessToken = strings.TrimSpace(body.AccessToken)
	if body.RefreshToken == "" && body.AccessToken == "" {
		writeErr(w, http.StatusBadRequest, "missing_token", "至少提供 refresh_token 或 access_token")
		return
	}
	if body.AccessToken != "" && !strings.HasPrefix(body.AccessToken, "mmoat_") {
		writeErr(w, http.StatusBadRequest, "bad_access_token", "access_token 格式不对，应以 mmoat_ 开头")
		return
	}
	if body.RefreshToken != "" && !strings.HasPrefix(body.RefreshToken, "mmort_") {
		writeErr(w, http.StatusBadRequest, "bad_refresh_token", "refresh_token 格式不对，应以 mmort_ 开头")
		return
	}

	var tok *oauth.Token
	var err error
	if body.AccessToken != "" {
		// 直接用，不轮换（轮换会打断客户端正在用的 token family）。
		tok = &oauth.Token{
			AccessToken:  body.AccessToken,
			RefreshToken: body.RefreshToken,
			TokenType:    "Bearer",
			ExpiresIn:    int((12 * time.Hour).Seconds()),
			Scopes:       []string{p.d.Cfg.Load().OAuth.Scope},
			Subject:      oauth.SubjectFromToken(body.AccessToken),
		}
	} else {
		tok, err = p.d.OAuth.Refresh(body.RefreshToken)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "refresh_failed", "refresh_token 不可用: "+err.Error())
			return
		}
	}
	acct := p.saveToken(tok, body.Label)
	view := viewOf(acct, p.d.Pool.Snapshot(), time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": view})
}

func (p *Panel) accountToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := p.d.Store.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	_, _, err := p.d.Store.Update(id, func(x *store.Account) { x.Disabled = !a.Disabled })
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	if a.Disabled {
		p.d.Pool.Reset(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": !a.Disabled})
}

// accountRename 只改展示名，不碰凭证。
func (p *Panel) accountRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		writeErr(w, http.StatusBadRequest, "empty_label", "名称不能为空")
		return
	}
	if runes := []rune(label); len(runes) > 40 {
		label = string(runes[:40])
	}
	if _, ok := p.d.Store.Get(id); !ok {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if _, _, err := p.d.Store.Update(id, func(a *store.Account) { a.Label = label }); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": label})
}

func (p *Panel) accountReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p.d.Pool.Reset(id)
	p.d.Store.Update(id, func(a *store.Account) { a.LastError = "" })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := p.d.Store.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	// 尽力撤销 refresh_token；失败不阻塞删除（本地凭证已不可用才是重点）。
	if a.RefreshToken != "" {
		_ = p.d.OAuth.Revoke(a.RefreshToken)
	}
	if _, err := p.d.Store.Remove(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountTest 用该账号发一条最小请求，验证真的能出 token。
func (p *Panel) accountTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := p.d.Store.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if a.ExpiresWithin(time.Now(), 2*time.Minute) && a.RefreshToken != "" {
		tok, err := p.d.OAuth.Refresh(a.RefreshToken)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "刷新失败: " + err.Error()})
			return
		}
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()
		p.d.Store.Update(id, func(x *store.Account) {
			x.AccessToken = tok.AccessToken
			if tok.RefreshToken != "" {
				x.RefreshToken = tok.RefreshToken
			}
			x.ExpiresAtMs = exp
			x.Generation++
		})
		a, _ = p.d.Store.Get(id)
	}

	cfg := p.d.Cfg.Load()
	model := cfg.Defaults.Model
	start := time.Now()
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "只回复 ok"}},
	})
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()
	resp, err := p.d.Up.Do(ctx, a.AccessToken, body, false)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		apiErr := upstream.ParseError(resp.StatusCode, resp.Body)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": apiErr.Error()})
		return
	}
	var out convert.AnthropicResponse
	if err := upstream.ReadJSON(resp, &out); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	var text string
	for _, b := range out.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	latency := time.Since(start).Milliseconds()
	p.d.Pool.Success(id)
	p.d.Store.Update(id, func(x *store.Account) { x.LastError = "" })
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"latency_ms": latency,
		"model":      model,
		"reply":      text,
		"usage":      out.Usage,
	})
}

// ---------- 用量 / 日志 / 模型 ----------

func (p *Panel) usageSeries(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		fmt.Sscanf(v, "%d", &days)
	}
	if days < 1 || days > 90 {
		days = 14
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"today":    p.d.Usage.Today(),
		"all_time": p.d.Usage.AllTime(),
		"series":   p.d.Usage.Series(days),
	})
}

func (p *Panel) logs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": p.d.Logs.Recent(limit)})
}

func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	cfg := p.d.Cfg.Load()
	type mv struct {
		ID        string `json:"id"`
		Context   int    `json:"context_window"`
		MaxOutput int    `json:"max_output"`
		Thinking  string `json:"thinking"`
	}
	var out []mv
	seen := map[string]bool{}
	ids := append([]string{}, cfg.Models.Expose...)
	ids = append(ids, convert.KnownModels()...)
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cap, _ := convert.CapabilityOf(id)
		out = append(out, mv{ID: id, Context: cap.ContextWindow, MaxOutput: cap.MaxOutput, Thinking: cap.Thinking})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":     out,
		"aliases":    cfg.Models.Aliases,
		"hidden":     cfg.Models.Hidden,
		"default":    cfg.Defaults.Model,
		"max_tokens": cfg.Defaults.MaxTokens,
		"thinking":   cfg.Defaults.ThinkingPolicy,
	})
}

// ---------- 配置 ----------

func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := p.d.Cfg.Load()
	// 不回传明文密钥；只回是否存在，前端用占位符。
	type view struct {
		*config.Config
		APIKeyMasked string `json:"api_key_masked"`
	}
	writeJSON(w, http.StatusOK, view{Config: cfg, APIKeyMasked: mask(cfg.APIKey)})
}

// setConfig 只接受白名单字段的部分更新，保存后热更新。
func (p *Panel) setConfig(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		APIKey          *string           `json:"api_key"`
		AdminPassword   *string           `json:"admin_password"`
		PublicBaseURL   *string           `json:"public_base_url"`
		Region          *string           `json:"region"`
		Model           *string           `json:"default_model"`
		MaxTokens       *int              `json:"max_tokens"`
		ThinkingPolicy  *string           `json:"thinking_policy"`
		Strategy        *string           `json:"strategy"`
		MaxInFlight     *int              `json:"max_in_flight"`
		MaxInFlightAcct *int              `json:"max_in_flight_per_account"`
		Aliases         map[string]string `json:"aliases"`
		Hidden          []string          `json:"hidden"`
		Expose          []string          `json:"expose"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	old := p.d.Cfg.Load()
	next := *old
	if patch.APIKey != nil && strings.TrimSpace(*patch.APIKey) != "" && !strings.Contains(*patch.APIKey, "•") {
		next.APIKey = strings.TrimSpace(*patch.APIKey)
	}
	if patch.AdminPassword != nil {
		next.AdminPassword = strings.TrimSpace(*patch.AdminPassword)
	}
	if patch.PublicBaseURL != nil {
		next.PublicBaseURL = strings.TrimSpace(*patch.PublicBaseURL)
	}
	if patch.Region != nil {
		next.Region = strings.TrimSpace(*patch.Region)
		next.ApplyRegionPreset()
	}
	if patch.Model != nil && strings.TrimSpace(*patch.Model) != "" {
		next.Defaults.Model = strings.TrimSpace(*patch.Model)
	}
	if patch.MaxTokens != nil && *patch.MaxTokens > 0 {
		next.Defaults.MaxTokens = *patch.MaxTokens
	}
	if patch.ThinkingPolicy != nil {
		switch *patch.ThinkingPolicy {
		case "auto", "off", "on":
			next.Defaults.ThinkingPolicy = *patch.ThinkingPolicy
		}
	}
	if patch.Strategy != nil {
		switch *patch.Strategy {
		case "weighted", "round_robin":
			next.Pool.Strategy = *patch.Strategy
		}
	}
	if patch.MaxInFlight != nil && *patch.MaxInFlight > 0 {
		next.Pool.MaxInFlight = *patch.MaxInFlight
	}
	if patch.MaxInFlightAcct != nil && *patch.MaxInFlightAcct > 0 {
		next.Pool.MaxInFlightAcct = *patch.MaxInFlightAcct
	}
	if patch.Aliases != nil {
		next.Models.Aliases = patch.Aliases
	}
	if patch.Hidden != nil {
		next.Models.Hidden = patch.Hidden
	}
	if patch.Expose != nil && len(patch.Expose) > 0 {
		next.Models.Expose = patch.Expose
	}

	if err := next.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	p.d.Cfg.Store(&next)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// probe 用当前第一个可用账号打一次上游，面板"连通性自检"用。
func (p *Panel) probe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = p.d.Cfg.Load().Defaults.Model
	}
	accounts := p.d.Store.List()
	var target *store.Account
	for i := range accounts {
		if !accounts[i].Disabled {
			target = &accounts[i]
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_account", "没有可用账号")
		return
	}
	start := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 32,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	})
	ctx, cancel := contextWithTimeout(r, 90*time.Second)
	defer cancel()
	resp, err := p.d.Up.Do(ctx, target.AccessToken, payload, false)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		apiErr := upstream.ParseError(resp.StatusCode, resp.Body)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": apiErr.Error()})
		return
	}
	var out convert.AnthropicResponse
	if err := upstream.ReadJSON(resp, &out); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"latency_ms": time.Since(start).Milliseconds(),
		"model":      model,
		"usage":      out.Usage,
	})
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func bearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func eq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func mask(s string) string {
	if len(s) <= 12 {
		return strings.Repeat("•", len(s))
	}
	return s[:8] + strings.Repeat("•", 6) + s[len(s)-4:]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func randomID(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		for i := range b {
			b[i] = alphabet[i%len(alphabet)]
		}
		return string(b)
	}
	for i := range b {
		b[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(b)
}
