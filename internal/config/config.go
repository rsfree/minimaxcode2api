// Package config 负责网关配置的加载、默认值与热更新。
//
// 设计取舍（与 workbuddy2api 保持一致）：
//   - 单一 config.json，面板改配置直接写回同一文件，无需额外配置中心；
//   - 热更新走 atomic.Pointer[Config]：读路径永远读到一份完整快照，
//     不会出现「改到一半的配置」被请求读到；
//   - 敏感项（api_key / admin_password）允许用环境变量覆盖，便于容器部署。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Duration 让 JSON 里可以直接写 "60s" 这种可读时长。
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// 也接受纯数字（毫秒），方便老配置迁移。
		var n int64
		if err2 := json.Unmarshal(raw, &n); err2 != nil {
			return err
		}
		*d = Duration(time.Duration(n) * time.Millisecond)
		return nil
	}
	if strings.TrimSpace(s) == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Upstream 上游 LLM 网关（Anthropic Messages 协议）。
type Upstream struct {
	// BaseURL 到 /v1 为止，代码里再拼 /messages。
	BaseURL string `json:"base_url"`
	// MediaBaseURL 媒体网关（到 mcp 为止）。留空则由 BaseURL 推导：
	// .../llm/v1 -> .../mcp，所以切 region 时媒体域名自动跟着走。
	MediaBaseURL string `json:"media_base_url"`
	// AnthropicVersion 上游要求的协议版本头。
	AnthropicVersion string `json:"anthropic_version"`
	// UserAgent 对齐官方客户端指纹；留空则用内置默认值。
	UserAgent string `json:"user_agent"`
	// TimeoutSeconds 长思考场景给足时间；0 表示不限制。
	TimeoutSeconds int `json:"timeout_seconds"`
	// StreamIdleTimeoutSeconds 流式读超时（两帧之间允许的最长静默）。
	StreamIdleTimeoutSeconds int `json:"stream_idle_timeout_seconds"`
}

// OAuth 设备授权与刷新参数（来自客户端二进制 @mavis/oauth-core 的契约）。
type OAuth struct {
	DeviceCodeURL string `json:"device_code_url"`
	TokenURL      string `json:"token_url"`
	RevokeURL     string `json:"revoke_url"`
	ClientID      string `json:"client_id"`
	Scope         string `json:"scope"`
	Audience      string `json:"audience"`
}

// Pool 账号池调度参数。
type Pool struct {
	Strategy        string   `json:"strategy"` // weighted | round_robin
	MaxInFlight     int      `json:"max_in_flight"`
	MaxInFlightAcct int      `json:"max_in_flight_per_account"`
	Cooldown429     Duration `json:"cooldown_429"`
	Cooldown429Max  Duration `json:"cooldown_429_max"`
	Cooldown5xx     Duration `json:"cooldown_5xx"`
	BreakerFailures int      `json:"breaker_failures"`
	BreakerCooldown Duration `json:"breaker_cooldown"`
	RefreshBefore   Duration `json:"refresh_before"`
}

// Defaults 对外接口的缺省值。
type Defaults struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// ThinkingPolicy 决定 M3（可切换思考）默认行为：auto | off | on。
	ThinkingPolicy string `json:"thinking_policy"`
}

// Models 对外暴露的模型与别名。
type Models struct {
	Expose  []string          `json:"expose"`
	Aliases map[string]string `json:"aliases"`
	Hidden  []string          `json:"hidden"`
}

// Logging 请求归档与面板日志。
type Logging struct {
	RequestArchiveEnabled bool `json:"request_archive_enabled"`
	RequestRetentionDays  int  `json:"request_retention_days"`
	MaxEntries            int  `json:"max_entries"`
	// CaptureClientInfo 记录调用方 IP / UA，便于排查是谁在用。
	CaptureClientInfo bool `json:"capture_client_info"`
}

type Config struct {
	Listen string `json:"listen"`
	// APIKey 对外 /v1/* 的密钥；也用作面板登录口令（除非单独设置 AdminPassword）。
	APIKey string `json:"api_key"`
	// AdminPassword 面板登录口令，留空则回落到 APIKey。
	AdminPassword string `json:"admin_password"`
	// DataDir 账号、用量、日志的落盘目录。
	DataDir string `json:"data_dir"`
	// PublicBaseURL 面板里展示给用户的接入地址（可选）。
	PublicBaseURL string `json:"public_base_url"`
	// Region 账号体系所属区域：cn（国内版）或 intl（国际版）。
	// 只影响四个上游域名预设；自定义域名不会被覆盖。
	Region string `json:"region"`

	Upstream Upstream `json:"upstream"`
	OAuth    OAuth    `json:"oauth"`
	Pool     Pool     `json:"pool"`
	Defaults Defaults `json:"defaults"`
	Models   Models   `json:"models"`
	Logging  Logging  `json:"logging"`

	// 运行时字段，不写回文件。
	path string
}

// Default 返回内置默认配置（首次启动直接可用）。
func Default() *Config {
	return &Config{
		Listen:  ":7864",
		APIKey:  "",
		DataDir: "./data",
		Region:  "cn",
		Upstream: Upstream{
			BaseURL:                  "https://agent.minimax.cn/mavis/api/v1/llm/v1",
			AnthropicVersion:         "2023-06-01",
			UserAgent:                "MiniMaxAgent Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
			TimeoutSeconds:           600,
			StreamIdleTimeoutSeconds: 300,
		},
		OAuth: OAuth{
			DeviceCodeURL: "https://account.minimax.cn/oauth2/device/code",
			TokenURL:      "https://account.minimax.cn/oauth2/token",
			RevokeURL:     "https://account.minimax.cn/oauth2/revoke",
			ClientID:      "mcode-public",
			Scope:         "agent.default",
			Audience:      "agent-backend",
		},
		Pool: Pool{
			Strategy:        "weighted",
			MaxInFlight:     8,
			MaxInFlightAcct: 3,
			Cooldown429:     Duration(60 * time.Second),
			Cooldown429Max:  Duration(30 * time.Minute),
			Cooldown5xx:     Duration(30 * time.Second),
			BreakerFailures: 3,
			BreakerCooldown: Duration(5 * time.Minute),
			RefreshBefore:   Duration(10 * time.Minute),
		},
		Defaults: Defaults{
			Model:          "MiniMax-M3.1-Flash",
			MaxTokens:      8192,
			ThinkingPolicy: "auto",
		},
		Models: Models{
			Expose: []string{"MiniMax-M3.1-Flash", "MiniMax-M3"},
			Aliases: map[string]string{
				"M3":           "MiniMax-M3",
				"M3.1-Flash":   "MiniMax-M3.1-Flash",
				"m3.1-flash":   "MiniMax-M3.1-Flash",
				"minimax-m3":   "MiniMax-M3",
				"MiniMax-M3.1": "MiniMax-M3.1-Flash",
			},
		},
		Logging: Logging{
			RequestArchiveEnabled: true,
			RequestRetentionDays:  7,
			MaxEntries:            2000,
			CaptureClientInfo:     true,
		},
	}
}

// Load 读配置；文件不存在时用默认值并生成一个（含随机 API Key）。
func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.path = path

	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		cfg.path = path
		cfg.applyEnv()
		cfg.ApplyRegionPreset()
		cfg.fillDefaults()
		return cfg, nil
	}

	// 首次启动：生成随机密钥并落盘，避免"默认空密钥"裸奔。
	// 环境变量必须先应用再落盘——容器场景常用 MMX2A_API_KEY/MMX2A_LISTEN
	// 注入，若跳过这一步，第一次启动会悄悄用随机密钥，直到重启才生效。
	cfg.APIKey = "sk-mmx-" + randomHex(16)
	cfg.applyEnv()
	cfg.ApplyRegionPreset()
	cfg.fillDefaults()
	if err := cfg.Save(); err != nil {
		return nil, fmt.Errorf("bootstrap config: %w", err)
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := strings.TrimSpace(os.Getenv("MMX2A_API_KEY")); v != "" {
		c.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("MMX2A_ADMIN_PASSWORD")); v != "" {
		c.AdminPassword = v
	}
	if v := strings.TrimSpace(os.Getenv("MMX2A_LISTEN")); v != "" {
		c.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("MMX2A_DATA_DIR")); v != "" {
		c.DataDir = v
	}
	if v := strings.TrimSpace(os.Getenv("MMX2A_REGION")); v != "" {
		c.Region = v
	}
}

// regionPreset 一个区域的四个上游域名。
//
// 国内版来自客户端 dist/main/config/constants.js 的 DOMAIN_URL / OPEN_PLATFORM 映射，
// 并由真实调用验证过；国际版来自同一张映射表的 en 分支（agent.minimax.io）与
// 主进程 CORS 白名单里的 account.minimax.io，域名可靠但**未用国际版账号实测**。
type regionPreset struct {
	UpstreamBase string
	DeviceCode   string
	Token        string
	Revoke       string
}

var regionPresets = map[string]regionPreset{
	"cn": {
		UpstreamBase: "https://agent.minimax.cn/mavis/api/v1/llm/v1",
		DeviceCode:   "https://account.minimax.cn/oauth2/device/code",
		Token:        "https://account.minimax.cn/oauth2/token",
		Revoke:       "https://account.minimax.cn/oauth2/revoke",
	},
	"intl": {
		UpstreamBase: "https://agent.minimax.io/mavis/api/v1/llm/v1",
		DeviceCode:   "https://account.minimax.io/oauth2/device/code",
		Token:        "https://account.minimax.io/oauth2/token",
		Revoke:       "https://account.minimax.io/oauth2/revoke",
	},
}

// normalizeRegion 容忍 intl / international / en / global 等写法。
func normalizeRegion(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "intl", "international", "en", "global", "overseas":
		return "intl"
	default:
		return "cn"
	}
}

// ApplyRegionPreset 按区域重写四个域名。
//
// 只在字段为空、或仍等于「另一区域」的预设值时才改写 —— 这样把 region 从 cn
// 改成 intl 能一把切过去，同时不会抹掉用户显式配置的自定义域名。
func (c *Config) ApplyRegionPreset() {
	region := normalizeRegion(c.Region)
	c.Region = region
	p := regionPresets[region]
	alt := regionPresets["cn"]
	if region == "cn" {
		alt = regionPresets["intl"]
	}
	pick := func(cur, preset, other string) string {
		if cur == "" || cur == other {
			return preset
		}
		return cur
	}
	c.Upstream.BaseURL = pick(c.Upstream.BaseURL, p.UpstreamBase, alt.UpstreamBase)
	c.OAuth.DeviceCodeURL = pick(c.OAuth.DeviceCodeURL, p.DeviceCode, alt.DeviceCode)
	c.OAuth.TokenURL = pick(c.OAuth.TokenURL, p.Token, alt.Token)
	c.OAuth.RevokeURL = pick(c.OAuth.RevokeURL, p.Revoke, alt.Revoke)
}

// fillDefaults 把零值字段补齐，兼容手工写的残缺配置。
func (c *Config) fillDefaults() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = d.Upstream.BaseURL
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")
	if c.Upstream.AnthropicVersion == "" {
		c.Upstream.AnthropicVersion = d.Upstream.AnthropicVersion
	}
	if c.Upstream.UserAgent == "" {
		c.Upstream.UserAgent = d.Upstream.UserAgent
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = d.Upstream.TimeoutSeconds
	}
	if c.Upstream.StreamIdleTimeoutSeconds <= 0 {
		c.Upstream.StreamIdleTimeoutSeconds = d.Upstream.StreamIdleTimeoutSeconds
	}
	if c.OAuth.DeviceCodeURL == "" {
		c.OAuth.DeviceCodeURL = d.OAuth.DeviceCodeURL
	}
	if c.OAuth.TokenURL == "" {
		c.OAuth.TokenURL = d.OAuth.TokenURL
	}
	if c.OAuth.RevokeURL == "" {
		c.OAuth.RevokeURL = d.OAuth.RevokeURL
	}
	if c.OAuth.ClientID == "" {
		c.OAuth.ClientID = d.OAuth.ClientID
	}
	if c.OAuth.Scope == "" {
		c.OAuth.Scope = d.OAuth.Scope
	}
	if c.OAuth.Audience == "" {
		c.OAuth.Audience = d.OAuth.Audience
	}
	if c.Pool.Strategy == "" {
		c.Pool.Strategy = d.Pool.Strategy
	}
	if c.Pool.MaxInFlight <= 0 {
		c.Pool.MaxInFlight = d.Pool.MaxInFlight
	}
	if c.Pool.MaxInFlightAcct <= 0 {
		c.Pool.MaxInFlightAcct = d.Pool.MaxInFlightAcct
	}
	if c.Pool.Cooldown429 <= 0 {
		c.Pool.Cooldown429 = d.Pool.Cooldown429
	}
	if c.Pool.Cooldown429Max <= 0 {
		c.Pool.Cooldown429Max = d.Pool.Cooldown429Max
	}
	if c.Pool.Cooldown5xx <= 0 {
		c.Pool.Cooldown5xx = d.Pool.Cooldown5xx
	}
	if c.Pool.BreakerFailures <= 0 {
		c.Pool.BreakerFailures = d.Pool.BreakerFailures
	}
	if c.Pool.BreakerCooldown <= 0 {
		c.Pool.BreakerCooldown = d.Pool.BreakerCooldown
	}
	if c.Pool.RefreshBefore <= 0 {
		c.Pool.RefreshBefore = d.Pool.RefreshBefore
	}
	if c.Defaults.Model == "" {
		c.Defaults.Model = d.Defaults.Model
	}
	if c.Defaults.MaxTokens <= 0 {
		c.Defaults.MaxTokens = d.Defaults.MaxTokens
	}
	if c.Defaults.ThinkingPolicy == "" {
		c.Defaults.ThinkingPolicy = d.Defaults.ThinkingPolicy
	}
	if len(c.Models.Expose) == 0 {
		c.Models.Expose = d.Models.Expose
	}
	if c.Models.Aliases == nil {
		c.Models.Aliases = d.Models.Aliases
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = d.Logging.RequestRetentionDays
	}
	if c.Logging.MaxEntries <= 0 {
		c.Logging.MaxEntries = d.Logging.MaxEntries
	}
}

// AdminSecret 面板登录口令。
func (c *Config) AdminSecret() string {
	if strings.TrimSpace(c.AdminPassword) != "" {
		return c.AdminPassword
	}
	return c.APIKey
}

// Save 原子写回配置文件（先写临时文件再 rename）。
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config path unknown")
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, c.path)
}

func (c *Config) Path() string { return c.path }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退化为时间派生，仍然可用（仅首次 bootstrap 用）。
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))[:n*2]
	}
	return hex.EncodeToString(b)
}

// Holder 提供热更新的只读快照。
type Holder struct {
	ptr atomic.Pointer[Config]
}

func NewHolder(c *Config) *Holder {
	h := &Holder{}
	h.ptr.Store(c)
	return h
}

func (h *Holder) Load() *Config { return h.ptr.Load() }

func (h *Holder) Store(c *Config) { h.ptr.Store(c) }
