package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/minimaxcode2api/internal/pool"
)

// get 发一个带鉴权的 GET（fixture 只提供了 POST 助手）。
func (f *fixture) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.front.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// 图片生成：请求要按槽位编号，响应要把 output_url 映射成 OpenAI 的 data[].url。
func TestImagesGeneration(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/image_synthesize" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"total_success":2,
			"success_items":[
				{"output_file":"slot_0.png","output_url":"https://cdn.test/a.png","has_image_output":true},
				{"output_file":"slot_1.png","output_url":"https://cdn.test/b.png","has_image_output":true}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations",
		`{"prompt":"一只橘猫","n":2,"size":"1792x1024","quality":"hd"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}

	mu.Lock()
	reqs, _ := got["requests"].([]any)
	mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("上游收到 %d 个槽位，期望 2，body=%v", len(reqs), got)
	}
	first, _ := reqs[0].(map[string]any)
	if first["output_file"] != "slot_0.png" {
		t.Fatalf("槽位名 = %v，期望 slot_0.png", first["output_file"])
	}
	if first["aspect_ratio"] != "16:9" {
		t.Fatalf("aspect_ratio = %v，1792x1024 应映射成 16:9", first["aspect_ratio"])
	}
	if first["resolution"] != "2K" {
		t.Fatalf("resolution = %v，quality=hd 应映射成 2K", first["resolution"])
	}

	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应: %v，body=%s", err, raw)
	}
	if len(out.Data) != 2 || out.Data[0].URL != "https://cdn.test/a.png" {
		t.Fatalf("data = %+v，期望两条 url", out.Data)
	}
}

// b64_json：必须按 magic bytes 判类型，而不是信上游的 Content-Type。
// 实测上游把 JPEG 标成 image/png，客户端按 header 解会坏。
func TestImagesB64SniffsRealType(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png") // 故意撒谎：内容是 JPEG
		_, _ = w.Write(jpeg)
	}))
	defer cdn.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/image_synthesize" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"success_items":[{"output_file":"slot_0.png",
			"output_url":"`+cdn.URL+`/x.png","has_image_output":true}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations",
		`{"prompt":"x","response_format":"b64_json"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 {
		t.Fatalf("解析响应: %v，body=%s", err, raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil {
		t.Fatalf("b64 解码失败: %v", err)
	}
	if len(decoded) < 3 || decoded[0] != 0xFF || decoded[1] != 0xD8 || decoded[2] != 0xFF {
		t.Fatalf("解码后不是 JPEG 原始字节: % x", decoded[:min(6, len(decoded))])
	}
}

// 402 是计费拦截：必须原样映射成 402，且不能换号重试（换号也一样扣不到）。
func TestImagesBillingNotRetried(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":402,"message":"plan balance has run out"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"prompt":"x"}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("状态码 = %d，期望 402，body=%s", resp.StatusCode, raw)
	}
	if n := f.upCalls(); n != 1 {
		t.Fatalf("上游被调用 %d 次，计费错误不应重试", n)
	}
	if !strings.Contains(string(raw), "insufficient_quota") {
		t.Fatalf("错误码未映射成 insufficient_quota: %s", raw)
	}
}

// 全部失败时不能装作成功，要把上游的第一条错误透出去。
func TestImagesAllFailedIs502(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"total_success":0,"total_failed":1,
			"success_items":[],"failed_items":[{"output_file":"slot_0.png","error_msg":"upstream internal error"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"prompt":"x"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream internal error") {
		t.Fatalf("上游错误信息没透出: %s", raw)
	}
}

// 审核拒答要归因成 400 content_filter —— 重试无用，客户端应改内容。
func TestImagesContentFilterIs400(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"total_success":0,"total_failed":1,
			"success_items":[],"failed_items":[{"output_file":"slot_0.png","error_msg":"prompt 违规"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"prompt":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "content_filter") {
		t.Fatalf("应归因成 content_filter: %s", raw)
	}
}

// 池的两种容量错误在媒体链路上必须与文本链路同口径：
// 没账号 → 503 no_account；全冷却/在途满 → 429 all_accounts_busy。
// 两者都要带 Retry-After，否则客户端会把冷却当 5xx 重试风暴。
func TestMediaCapacityErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"无账号", pool.ErrNoAccount, http.StatusServiceUnavailable, "no_account"},
		{"全冷却", pool.ErrAllBusy, http.StatusTooManyRequests, "all_accounts_busy"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		writeMediaError(w, c.err)
		raw := w.Body.String()
		if w.Code != c.status {
			t.Fatalf("%s: 状态码 = %d，期望 %d，body=%s", c.name, w.Code, c.status, raw)
		}
		if !strings.Contains(raw, c.code) {
			t.Fatalf("%s: 错误码应为 %s，body=%s", c.name, c.code, raw)
		}
		if w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: 缺 Retry-After", c.name)
		}
	}
}

// 402 的提示必须跟着上游措辞走：视频是"缺积分"，图片是"窗口限额"。
// 曾经一律加"不是余额不足"，把视频用户往反方向带。
func TestBillingHintMatchesUpstream(t *testing.T) {
	imgMsg := "the user's plan balance has run out; this is a billing limit on the user's own subscription plan"
	got := billingHint(imgMsg)
	if !strings.Contains(got, "限额触顶") || !strings.Contains(got, "无需充值") {
		t.Fatalf("图片口径不对: %s", got)
	}

	vidMsg := "the configured payment sources have insufficient allowance or credits for this video; this task costs 280 credits"
	got = billingHint(vidMsg)
	if !strings.Contains(got, "积分不足") || strings.Contains(got, "不是余额不足") {
		t.Fatalf("视频口径不对: %s", got)
	}

	// 认不出来的措辞就别乱加提示。
	other := "something unexpected"
	if billingHint(other) != other {
		t.Fatalf("未知措辞不该被加提示: %s", billingHint(other))
	}
}
