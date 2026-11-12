package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/minimaxcode2api/internal/signin"
	"github.com/linguo2625469/minimaxcode2api/internal/videotask"
)

// ---------- 任务（视频异步任务） ----------

// tasks 列出网关保存的视频任务快照。
//
// 数据源是 data/videotasks.json（上游查询必须带 model，而客户端轮询只带 id，
// 所以提交时就得记下来）。面板只读这里，不直接碰上游 —— 同步是显式动作。
func (p *Panel) tasks(w http.ResponseWriter, r *http.Request) {
	if p.d.VideoTasks == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0})
		return
	}
	limit := 200
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	items, total := p.d.VideoTasks.List(limit)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// taskSync 主动拉一次上游状态。
func (p *Panel) taskSync(w http.ResponseWriter, r *http.Request) {
	if p.d.RefreshTask == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "任务同步未启用")
		return
	}
	rec, err := p.d.RefreshTask(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, videotask.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		// 上游不认识/网络抖动都归到这里；本地记录原样回给前端，状态不丢。
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "code": "sync_failed"},
			"item":  rec,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": rec})
}

// taskDelete 删除终态任务记录（与对外 DELETE 协议口径一致：非终态不允许）。
func (p *Panel) taskDelete(w http.ResponseWriter, r *http.Request) {
	if p.d.DeleteTask == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "任务删除未启用")
		return
	}
	err := p.d.DeleteTask(r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, videotask.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, videotask.ErrNotTerminal):
		writeErr(w, http.StatusConflict, "not_terminal", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "delete_failed", err.Error())
	}
}

// ---------- 签到 ----------

// signinStatus 只读：每个账号今天第几天、能不能领、已领多少。
//
// 不在这里顺手 claim —— 打开面板就自动领，会让"看一眼"变成写操作，
// 也让前端没法区分"我按了"和"它自己领了"。
func (p *Panel) signinStatus(w http.ResponseWriter, r *http.Request) {
	if p.d.SigninStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "签到未启用")
		return
	}
	cfg := p.d.Cfg.Load()
	results, err := p.d.SigninStatus(r.Context(), "")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":  results,
		"timezone": cfg.Signin.Timezone,
		"auto":     cfg.Signin.Auto,
		"at":       cfg.Signin.At,
		"summary":  summarize(results),
	})
}

// signinClaim 立即签到（全池，或 body.account_id 指定的一个）。
func (p *Panel) signinClaim(w http.ResponseWriter, r *http.Request) {
	if p.d.SigninAccounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "签到未启用")
		return
	}
	var body struct {
		AccountID string `json:"account_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	results, err := p.d.SigninAccounts(r.Context(), strings.TrimSpace(body.AccountID))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"summary": summarize(results),
	})
}

// summary 给面板顶部卡片用的汇总（避免前端再算一遍）。
type signinSummary struct {
	Accounts int `json:"accounts"`
	Claimed  int `json:"claimed"`
	Points   int `json:"points"`
	Failed   int `json:"failed"`
	DayNo    int `json:"day_no"`
}

func summarize(results []signin.Result) signinSummary {
	s := signinSummary{Accounts: len(results)}
	for _, r := range results {
		if !r.OK {
			s.Failed++
			continue
		}
		if r.Claimed {
			s.Claimed++
			s.Points += r.Points
		}
		if r.DayNo > s.DayNo {
			s.DayNo = r.DayNo
		}
	}
	return s
}
