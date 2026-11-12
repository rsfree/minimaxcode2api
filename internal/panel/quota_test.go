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
