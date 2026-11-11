package panel

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
)

//go:embed ui/index.html
var indexHTML []byte

//go:embed ui/app.js
var appJS []byte

//go:embed ui/style.css
var styleCSS []byte

//go:embed ui/favicon.svg
var faviconSVG []byte

// 严格 CSP：脚本只允许同源文件（页面里没有内联脚本/内联事件），
// 样式允许内联（进度条宽度、动态色块需要 style="..."），图片允许 data:。
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func writeStatic(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (p *Panel) serveIndex(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "text/html; charset=utf-8", indexHTML)
}

func (p *Panel) serveJS(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "application/javascript; charset=utf-8", appJS)
}

func (p *Panel) serveCSS(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "text/css; charset=utf-8", styleCSS)
}

func (p *Panel) serveFavicon(w http.ResponseWriter, r *http.Request) {
	writeStatic(w, r, "image/svg+xml", faviconSVG)
}
