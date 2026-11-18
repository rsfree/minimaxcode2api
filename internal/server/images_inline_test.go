package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tinyPNG 复用 image_tools_test.go 里的 1×1 透明 PNG（同包，勿重复声明）。

// TestInlineImageRefs 锁定「客户端只给公网 URL 也能图生图」这条行为。
//
// 上游 input_urls 只认 cdn_url 或内联 base64；以前 /v1/images/generations
// 原样透传，任意公网 URL 会 502 generation_failed。
func TestInlineImageRefs(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatalf("测试数据损坏: %v", err)
	}
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	defer imgSrv.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success_items":[{"has_image_output":true,"output_url":"https://cdn.hailuoai.com/x.png"}]}`))
	}, "tok-1")
	f.srv.fetchAllowPrivate = true // httptest 监听 127.0.0.1

	cases := []struct {
		name      string
		in        string
		want      string
		wantCode  string
		shouldErr bool
	}{
		{"上游 CDN 原样", "https://cdn.hailuoai.com/mcp/a.png", "https://cdn.hailuoai.com/mcp/a.png", "", false},
		{"data URI 原样", "data:image/png;base64," + tinyPNG, "data:image/png;base64," + tinyPNG, "", false},
		{"裸 base64 原样", tinyPNG, tinyPNG, "", false},
		{"公网 URL 内联", imgSrv.URL + "/ok.png", "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), "", false},
		{"404 报 fetch_failed", imgSrv.URL + "/missing.png", "", "fetch_failed", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, perr := f.srv.inlineImageRefs(t.Context(), []string{c.in})
			if c.shouldErr {
				if perr == nil {
					t.Fatalf("期望失败，实际放行：%v", out)
				}
				if perr.Code != c.wantCode {
					t.Errorf("错误码期望 %s，实际 %s（%s）", c.wantCode, perr.Code, perr.Message)
				}
				if perr.Param != "image[0]" {
					t.Errorf("param 应定位到 image[0]，实际 %s", perr.Param)
				}
				return
			}
			if perr != nil {
				t.Fatalf("不该失败：%s", perr.Message)
			}
			if len(out) != 1 || out[0] != c.want {
				t.Errorf("结果不符：期望 %.60s… 实际 %.60s…", c.want, strings.Join(out, ","))
			}
		})
	}
}

func TestInlineImageRefsKeepsFourImageLimit(t *testing.T) {
	// 归一化不应改变张数：4 张仍 4 张，越界由 imageproto.Build 报错。
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {}, "tok-1")
	refs := []string{
		"data:image/png;base64," + tinyPNG,
		"data:image/png;base64," + tinyPNG,
		"data:image/png;base64," + tinyPNG,
		"data:image/png;base64," + tinyPNG,
	}
	out, perr := f.srv.inlineImageRefs(t.Context(), refs)
	if perr != nil {
		t.Fatalf("不该失败：%s", perr.Message)
	}
	if len(out) != 4 {
		t.Errorf("张数被改变：%d", len(out))
	}
}
