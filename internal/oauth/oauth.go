// Package oauth 实现 MiniMax Code 的设备授权（Device Authorization Grant + PKCE）
// 与 refresh_token 刷新。
//
// 契约来自官方客户端二进制里 @mavis/oauth-core 的实现（mcode-public / agent.default /
// agent-backend），端点、参数、返回字段都按它对齐——服务端认的是这套组合，
// 少一个参数就可能被拒。
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
)

// 设备授权轮询状态（服务端可能用 OAuth 标准错误码，也可能用 status 字段）。
const (
	StatusPending  = "pending"
	StatusSlowDown = "slow_down"
	StatusDenied   = "denied"
)

// DeviceAuth 一次设备授权的会话状态，交给面板轮询。
type DeviceAuth struct {
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int       `json:"expires_in"`
	Interval                int       `json:"interval"`
	CodeVerifier            string    `json:"-"`
	CreatedAt               time.Time `json:"-"`
	// TokenParam 轮询时用哪个字段名：标准是 device_code；账号服务在某些形态下
	// 返回的只有 user_code（客户端里叫 account user-code polling）。
	TokenParam string `json:"-"`
}

// Expired 授权会话是否过期。
func (d *DeviceAuth) Expired() bool {
	return time.Now().After(d.CreatedAt.Add(time.Duration(d.ExpiresIn) * time.Second))
}

// Token 一次成功的授权结果。
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int
	Scopes       []string
	Subject      string
}

// Client OAuth 客户端。
type Client struct {
	cfg  *config.Holder
	http *http.Client
}

func New(cfg *config.Holder) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func pkcePair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

type deviceCodeResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURL         string `json:"verification_url"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

// StartDeviceAuth 发起一次设备授权。
func (c *Client) StartDeviceAuth() (*DeviceAuth, error) {
	o := c.cfg.Load().OAuth
	verifier, challenge, err := pkcePair()
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"client_id":             {o.ClientID},
		"scope":                 {o.Scope},
		"audience":              {o.Audience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	body, status, err := c.postForm(o.DeviceCodeURL, form, nil)
	if err != nil {
		return nil, err
	}
	var r deviceCodeResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("device code: bad json (http %d): %w", status, err)
	}
	if status != http.StatusOK || r.Error != "" {
		return nil, fmt.Errorf("device code rejected (http %d): %s %s", status, r.Error, r.ErrorDescription)
	}
	uri := firstNonEmpty(r.VerificationURI, r.VerificationURL)
	if r.DeviceCode == "" || r.UserCode == "" || uri == "" {
		return nil, fmt.Errorf("device code: incomplete response (http %d)", status)
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 300
	}
	if r.Interval <= 0 {
		r.Interval = 5
	}
	return &DeviceAuth{
		DeviceCode:              r.DeviceCode,
		UserCode:                r.UserCode,
		VerificationURI:         uri,
		VerificationURIComplete: r.VerificationURIComplete,
		ExpiresIn:               r.ExpiresIn,
		Interval:                r.Interval,
		CodeVerifier:            verifier,
		CreatedAt:               time.Now(),
		TokenParam:              "device_code",
	}, nil
}

type tokenResp struct {
	AccessToken      string          `json:"access_token"`
	RefreshToken     string          `json:"refresh_token"`
	TokenType        string          `json:"token_type"`
	ExpiresIn        int             `json:"expires_in"`
	Scope            json.RawMessage `json:"scope"`
	Status           string          `json:"status"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

func (r *tokenResp) scopes() []string {
	if len(r.Scope) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(r.Scope, &s); err == nil {
		return strings.Fields(s)
	}
	var arr []string
	if err := json.Unmarshal(r.Scope, &arr); err == nil {
		return arr
	}
	return nil
}

// PollDeviceToken 轮询一次授权结果。
// 返回 (nil, "pending", nil) 表示还没授权；有 Token 表示成功。
func (c *Client) PollDeviceToken(d *DeviceAuth) (*Token, string, error) {
	o := c.cfg.Load().OAuth
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":  {o.ClientID},
	}
	if d.TokenParam == "user_code" {
		form.Set("user_code", d.UserCode)
	} else {
		form.Set("device_code", d.DeviceCode)
	}
	form.Set("code_verifier", d.CodeVerifier)

	body, status, err := c.postForm(o.TokenURL, form, nil)
	if err != nil {
		return nil, "", err
	}
	var r tokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, "", fmt.Errorf("token: bad json (http %d): %w", status, err)
	}
	// 服务端可能用 status 字段表达轮询态。
	switch strings.ToLower(r.Status) {
	case StatusPending, "":
	case StatusSlowDown:
		return nil, StatusSlowDown, nil
	case StatusDenied, "access_denied":
		return nil, "", fmt.Errorf("授权被拒绝")
	case "expired", "expired_token":
		return nil, "", fmt.Errorf("授权已过期")
	default:
		// 有 token 就走下面的成功分支。
		if r.AccessToken == "" {
			return nil, "", fmt.Errorf("未知授权状态: %s", r.Status)
		}
	}
	switch strings.ToLower(r.Error) {
	case "", "authorization_pending":
		if r.AccessToken == "" {
			return nil, StatusPending, nil
		}
	case StatusSlowDown:
		return nil, StatusSlowDown, nil
	case "access_denied":
		return nil, "", fmt.Errorf("授权被拒绝")
	case "expired_token":
		return nil, "", fmt.Errorf("授权已过期")
	default:
		return nil, "", fmt.Errorf("token 端点报错 (http %d): %s %s", status, r.Error, r.ErrorDescription)
	}
	if r.AccessToken == "" {
		return nil, StatusPending, nil
	}
	scopes := r.scopes()
	if len(scopes) == 0 {
		scopes = strings.Fields(o.Scope)
	}
	return &Token{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		TokenType:    firstNonEmpty(r.TokenType, "Bearer"),
		ExpiresIn:    r.ExpiresIn,
		Scopes:       scopes,
		Subject:      decodeSubject(r.AccessToken),
	}, "", nil
}

// Refresh 用 refresh_token 换新 token。
func (c *Client) Refresh(refreshToken string) (*Token, error) {
	o := c.cfg.Load().OAuth
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {o.ClientID},
		"scope":         {o.Scope},
		"audience":      {o.Audience},
	}
	body, status, err := c.postForm(o.TokenURL, form, nil)
	if err != nil {
		return nil, err
	}
	var r tokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		// 401 时 body 常是 {"error":"invalid_grant"}，上面已能解析；这里只兜底非 JSON。
		return nil, fmt.Errorf("refresh: bad json (http %d): %w", status, err)
	}
	if r.Error != "" {
		return nil, fmt.Errorf("refresh 失败 (http %d): %s %s", status, r.Error, r.ErrorDescription)
	}
	if r.AccessToken == "" {
		return nil, fmt.Errorf("refresh 未返回 access_token (http %d)", status)
	}
	if r.RefreshToken == "" {
		r.RefreshToken = refreshToken
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 6 * 3600
	}
	scopes := r.scopes()
	if len(scopes) == 0 {
		scopes = strings.Fields(o.Scope)
	}
	return &Token{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		TokenType:    firstNonEmpty(r.TokenType, "Bearer"),
		ExpiresIn:    r.ExpiresIn,
		Scopes:       scopes,
		Subject:      decodeSubject(r.AccessToken),
	}, nil
}

// Revoke 撤销 refresh_token（尽力而为，失败不阻塞本地清除）。
func (c *Client) Revoke(refreshToken string) error {
	o := c.cfg.Load().OAuth
	if o.RevokeURL == "" || refreshToken == "" {
		return nil
	}
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {o.ClientID},
	}
	_, _, err := c.postForm(o.RevokeURL, form, nil)
	return err
}

func (c *Client) postForm(endpoint string, form url.Values, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// decodeSubject 尽力从 token 里解出 sub（opaque token 时返回空）。
// SubjectFromToken 从 access_token 里取 subject，用作账号 ID。
// 不是 JWT 或解析失败时返回空串，调用方自行兜底。
func SubjectFromToken(tok string) string { return decodeSubject(tok) }

func decodeSubject(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub       string `json:"sub"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return firstNonEmpty(claims.Sub, claims.AccountID)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
