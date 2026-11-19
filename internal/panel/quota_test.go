package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
	"github.com/linguo2625469/minimaxcode2api/internal/quota"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
)

// /panel/api/quota 的形状是前端契约：字段少一个卡片就会空白。
// 同时验证：停用账号不进列表、上游错误如实透出（不伪装成 0 额度）。
func TestPanelQuotaEndpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.APIKey = "test-key"
	cfg.AdminPassword = ""
	cfg.DataDir = dir
	holder := config.NewHolder(cfg)

	st, err := store.Open(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for _, a := range []*store.Account{
		{ID: "a1", Label: "主号", AccessToken: "mmoat_x"},
		{ID: "a2", Label: "停用的", AccessToken: "mmoat_y", Disabled: true},
		{ID: "a3", Label: "", AccessToken: "mmoat_z"},
	} {
		if err := st.Add(a); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	p := New(Deps{
		Cfg: holder, Store: st, Start: time.Now(),
		Quota: func(_ context.Context, a store.Account) (*quota.ImageFree, error) {
			if a.ID == "a3" {
				return nil, errFakeQuota
			}
			return &quota.ImageFree{Total: 10, Used: 1, Remaining: 9, Active: true, FetchedAt: time.Now()}, nil
		},
	})
	mux := http.NewServeMux()
	p.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/panel/api/quota", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rr.Code, rr.Body.String())
	}

	var out struct {
		AnyActive bool `json:"any_active"`
		Accounts  []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Total     int    `json:"total"`
			Used      int    `json:"used"`
			Remaining int    `json:"remaining"`
			Active    bool   `json:"active"`
			Error     string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v，body=%s", err, rr.Body.String())
	}
	if !out.AnyActive {
		t.Fatalf("any_active 应为 true")
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("停用账号应被过滤，实际 %d 个: %+v", len(out.Accounts), out.Accounts)
	}
	if out.Accounts[0].ID != "a1" || out.Accounts[0].Total != 10 || out.Accounts[0].Remaining != 9 {
		t.Fatalf("首个账号额度不对: %+v", out.Accounts[0])
	}
	// 没名字的账号用 id 兜底，前端才不会显示空白。
	if out.Accounts[1].Label != "a3" {
		t.Fatalf("空 label 应回落到 id，实际 %q", out.Accounts[1].Label)
	}
	if out.Accounts[1].Error == "" {
		t.Fatalf("上游错误应如实透出: %+v", out.Accounts[1])
	}

	// 未带口令必须 401（面板数据不裸奔）。
	req2 := httptest.NewRequest(http.MethodGet, "/panel/api/quota", nil)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("无口令状态码 = %d，期望 401", rr2.Code)
	}
}

// 套餐/积分/窗口：三个池子各占一段，任一失败不得影响其余两段。
//
// 这是面板最容易被写坏的地方 —— 把三组数据合成一个 error 字段，用户就会在
// 窗口接口抖动时连积分都看不见。
func TestPanelQuotaPlanSection(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.APIKey = "test-key"
	cfg.AdminPassword = ""
	cfg.DataDir = dir
	holder := config.NewHolder(cfg)

	st, err := store.Open(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for _, a := range []*store.Account{
		{ID: "ok", Label: "好账号", AccessToken: "mmoat_a"},
		{ID: "no-window", Label: "窗口挂了", AccessToken: "mmoat_b"},
	} {
		if err := st.Add(a); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	p := New(Deps{
		Cfg: holder, Store: st, Start: time.Now(),
		Quota: func(_ context.Context, a store.Account) (*quota.ImageFree, error) {
			return &quota.ImageFree{Total: 10, Used: 1, Remaining: 9, Active: true, FetchedAt: time.Now()}, nil
		},
		Plan: func(_ context.Context, a store.Account) (*quota.Plan, error) {
			pl := &quota.Plan{
				Membership: &quota.Membership{
					Tier: "Go", TPRemains: 14291685264.12, HasTokenPlan: true,
					ExpiresAtMs: 1796947200000, CreditTotal: "4120",
					CreditFree: "4120", CreditPaid: "0", FetchedAt: time.Now(),
				},
			}
			if a.ID == "no-window" {
				pl.WindowError = "quota: 窗口用量 HTTP 403"
				return pl, nil
			}
			pl.Window = &quota.Window{
				ModelName: "general", IntervalUsedPercent: "0%", WeeklyUsedPercent: "12%",
				IntervalEndMs: 1795090324261, WeeklyEndMs: 1795609538106, FetchedAt: time.Now(),
			}
			return pl, nil
		},
	})
	mux := http.NewServeMux()
	p.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/panel/api/quota", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rr.Code, rr.Body.String())
	}

	var out struct {
		Accounts []struct {
			ID    string `json:"id"`
			Total int    `json:"total"`
			Plan  struct {
				Membership *struct {
					Tier      string  `json:"tier"`
					TPRemains float64 `json:"tp_remains"`
					Credit    string  `json:"credit_total"`
				} `json:"membership"`
				MembershipError string `json:"membership_error"`
				Window          *struct {
					IntervalUsedPercent string `json:"interval_used_percent"`
					WeeklyUsedPercent   string `json:"weekly_used_percent"`
				} `json:"window"`
				WindowError string `json:"window_error"`
			} `json:"plan"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v，body=%s", err, rr.Body.String())
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("账号数 = %d", len(out.Accounts))
	}

	byID := map[string]int{}
	for i, a := range out.Accounts {
		byID[a.ID] = i
	}
	ok := out.Accounts[byID["ok"]]
	if ok.Total != 10 {
		t.Errorf("图片次数池应保留，实际 total=%d", ok.Total)
	}
	if ok.Plan.Membership == nil || ok.Plan.Membership.Tier != "Go" {
		t.Fatalf("套餐未透出: %+v", ok.Plan)
	}
	if ok.Plan.Membership.Credit != "4120" {
		t.Errorf("积分 = %q（字符串不能被截断）", ok.Plan.Membership.Credit)
	}
	if ok.Plan.Membership.TPRemains != 14291685264.12 {
		t.Errorf("tp_remains = %v", ok.Plan.Membership.TPRemains)
	}
	if ok.Plan.Window == nil || ok.Plan.Window.WeeklyUsedPercent != "12%" {
		t.Errorf("窗口未透出: %+v", ok.Plan.Window)
	}

	// 窗口失败时：套餐/积分照常，窗口段单独带错误。
	nw := out.Accounts[byID["no-window"]]
	if nw.Plan.Membership == nil || nw.Plan.Membership.Credit != "4120" {
		t.Errorf("窗口失败不应牵连积分: %+v", nw.Plan)
	}
	if nw.Plan.Window != nil {
		t.Errorf("窗口应为空: %+v", nw.Plan.Window)
	}
	if nw.Plan.WindowError == "" {
		t.Error("窗口错误应透出")
	}
}
