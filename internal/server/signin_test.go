package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// 今日 status=2 才 POST claim；已领的日子必须只读不写。
//
// 这条是签到的核心不变量：上游对重复 claim 会直接报错，
// 如果无脑 POST，"今天已经签过"会被自动化记成失败，天天重试。
func TestSigninClaimsOnlyWhenClaimable(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()

		if got := r.URL.Query().Get("timezone_id"); got != "Asia/Shanghai" {
			t.Errorf("timezone_id = %q，期望 Asia/Shanghai", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/signin/status"):
			io.WriteString(w, `{"base_resp":{"status_code":0,"status_msg":"ok"},
				"data":{"scene":2,"days":[
					{"day_no":1,"points":400,"status":3,"is_today":false},
					{"day_no":2,"points":400,"status":2,"is_today":true}]}}`)
		case strings.HasSuffix(r.URL.Path, "/signin/claim"):
			io.WriteString(w, `{"base_resp":{"status_code":0,"status_msg":"ok"},
				"data":{"claim_id":"c1","claim_result":1,"day_no":2,"points":400}}`)
		default:
			t.Errorf("未预期的上游路径: %s", r.URL.Path)
		}
	}, "tok-a")

	resp, raw := f.post(t, "/signin", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}

	var out struct {
		OK          bool `json:"ok"`
		TotalPoints int  `json:"total_points"`
		Results     []struct {
			AccountID string `json:"account_id"`
			OK        bool   `json:"ok"`
			Claimed   bool   `json:"claimed"`
			DayNo     int    `json:"day_no"`
			Points    int    `json:"points"`
			Error     string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	if !out.OK || len(out.Results) != 1 {
		t.Fatalf("ok=%v results=%d，期望 true/1 (%s)", out.OK, len(out.Results), raw)
	}
	r0 := out.Results[0]
	if !r0.Claimed || r0.Points != 400 || r0.DayNo != 2 || r0.Error != "" {
		t.Fatalf("结果 = %+v，期望 claimed/400/day2 无错误", r0)
	}
	if out.TotalPoints != 400 {
		t.Fatalf("total_points = %d，期望 400", out.TotalPoints)
	}

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 2 || !strings.HasPrefix(got[0], "GET ") || !strings.HasPrefix(got[1], "POST ") {
		t.Fatalf("上游调用序列 = %v，期望 [GET status, POST claim]", got)
	}
}

// 今天已领（status=3）→ 必须零写操作，且不算失败。
func TestSigninSkipsWhenAlreadyClaimed(t *testing.T) {
	var mu sync.Mutex
	var wrote bool

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			wrote = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"base_resp":{"status_code":0},"data":{"scene":2,"days":[
			{"day_no":2,"points":400,"status":3,"is_today":true}]}}`)
	}, "tok-a")

	_, raw := f.post(t, "/signin", `{}`)
	var out struct {
		OK      bool `json:"ok"`
		Results []struct {
			OK      bool `json:"ok"`
			Claimed bool `json:"claimed"`
			Points  int  `json:"points"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	if !out.OK || len(out.Results) != 1 || !out.Results[0].OK {
		t.Fatalf("已签到的日子应算成功: %s", raw)
	}
	if out.Results[0].Claimed || out.Results[0].Points != 0 {
		t.Fatalf("不该领取: %+v", out.Results[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if wrote {
		t.Fatal("已签到时仍 POST 了 claim")
	}
}

// 单账号失败不能拖垮整批：坏号报错，好号照签。
func TestSigninIsolatesAccountFailure(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "bad") {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"invalid access token"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/signin/status") {
			io.WriteString(w, `{"base_resp":{"status_code":0},"data":{"scene":2,"days":[
				{"day_no":2,"points":400,"status":2,"is_today":true}]}}`)
			return
		}
		io.WriteString(w, `{"base_resp":{"status_code":0},"data":{"claim_id":"c","claim_result":1,"day_no":2,"points":400}}`)
	}, "good", "bad")

	_, raw := f.post(t, "/signin", `{}`)
	var out struct {
		OK      bool `json:"ok"`
		Results []struct {
			AccountID string `json:"account_id"`
			OK        bool   `json:"ok"`
			Claimed   bool   `json:"claimed"`
			Error     string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results 数量 = %d，期望 2 (%s)", len(out.Results), raw)
	}
	if out.OK {
		t.Fatalf("有账号失败时 ok 应为 false: %s", raw)
	}
	// 顺序与 Store.List 一致：acct-0=good, acct-1=bad。
	if !out.Results[0].OK || !out.Results[0].Claimed {
		t.Fatalf("好号应签到成功: %+v", out.Results[0])
	}
	if out.Results[1].OK || out.Results[1].Error == "" {
		t.Fatalf("坏号应报错: %+v", out.Results[1])
	}
}
