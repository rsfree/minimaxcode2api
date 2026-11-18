// Package panel 的实测档案：面板只读展示模型、配额、频控与协议验证结果。
//
// 数据默认编译进二进制（probes_default.json），同时允许 {data_dir}/probes.json
// 覆盖 —— 后续补测不需要重新构建镜像。文件只在通过 JSON 校验时才会被采用，
// 避免手写坏文件把整页打空。
package panel

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed probes_default.json
var probesDefault []byte

func (p *Panel) probes(w http.ResponseWriter, r *http.Request) {
	body := probesDefault
	if cfg := p.d.Cfg.Load(); cfg != nil && cfg.DataDir != "" {
		if raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "probes.json")); err == nil && json.Valid(raw) {
			body = raw
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}
