package config

import (
	"path/filepath"
	"testing"
)

// 首次启动（文件不存在）也必须尊重环境变量，否则容器里注入的
// MMX2A_API_KEY 会被随机密钥顶掉，直到重启才生效。
func TestBootstrapAppliesEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MMX2A_API_KEY", "sk-mmx-fixed-key")
	t.Setenv("MMX2A_LISTEN", ":19999")
	t.Setenv("MMX2A_DATA_DIR", filepath.Join(dir, "data"))

	cfg, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "sk-mmx-fixed-key" {
		t.Fatalf("api_key = %q，环境变量未生效", cfg.APIKey)
	}
	if cfg.Listen != ":19999" {
		t.Fatalf("listen = %q，环境变量未生效", cfg.Listen)
	}
	if cfg.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("data_dir = %q，环境变量未生效", cfg.DataDir)
	}
}

// 已有配置文件时，环境变量仍然覆盖文件值（部署时改端口/密钥不必改文件）。
func TestEnvOverridesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	base := Default()
	base.APIKey = "sk-mmx-from-file"
	base.Listen = ":7864"
	base.path = path
	if err := base.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("MMX2A_API_KEY", "sk-mmx-from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "sk-mmx-from-env" {
		t.Fatalf("api_key = %q，环境变量未覆盖文件值", cfg.APIKey)
	}
}

// region 切换要能一把换掉四个域名（国内版 -> 国际版）。
func TestRegionSwitch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	base := Default()
	base.APIKey = "sk-mmx-test"
	base.path = path
	if err := base.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("MMX2A_REGION", "intl")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != "intl" {
		t.Fatalf("region = %q，期望 intl", cfg.Region)
	}
	if got := cfg.Upstream.BaseURL; got != "https://agent.minimax.io/mavis/api/v1/llm/v1" {
		t.Fatalf("base_url = %q，未切到国际版", got)
	}
	if got := cfg.OAuth.TokenURL; got != "https://account.minimax.io/oauth2/token" {
		t.Fatalf("token_url = %q，未切到国际版", got)
	}

	// 再切回国内：预设值应被识别并换回，而不是留在 intl。
	t.Setenv("MMX2A_REGION", "cn")
	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := back.Upstream.BaseURL; got != "https://agent.minimax.cn/mavis/api/v1/llm/v1" {
		t.Fatalf("base_url = %q，未切回国内版", got)
	}
}

// 自定义域名不该被 region 预设抹掉。
func TestRegionPresetKeepsCustomDomain(t *testing.T) {
	cfg := Default()
	cfg.Upstream.BaseURL = "https://my-gateway.example.com/mavis/api/v1/llm/v1"
	cfg.Region = "intl"
	cfg.ApplyRegionPreset()
	if got := cfg.Upstream.BaseURL; got != "https://my-gateway.example.com/mavis/api/v1/llm/v1" {
		t.Fatalf("base_url = %q，自定义域名被覆盖了", got)
	}
	if got := cfg.OAuth.TokenURL; got != "https://account.minimax.io/oauth2/token" {
		t.Fatalf("token_url = %q，其余预设字段应切到国际版", got)
	}
}
