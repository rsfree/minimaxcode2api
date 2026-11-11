// Package usage 轻量用量统计：按天、按模型聚合 token 与请求数。
//
// 不做精确计费（上游只有 token 数，没有对外价格），只服务于面板上的
// "今天用了多少 / 哪个模型最费 / 有没有在报错"这三个问题。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Bucket 一个聚合桶。
type Bucket struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	PromptTokens int64 `json:"prompt_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// LatencyMs 累计耗时，算平均用。
	LatencyMs int64 `json:"latency_ms"`
	// CachedTokens 命中 prompt cache 的输入 token。
	CachedTokens int64 `json:"cached_tokens"`
}

// Day 一天的聚合。
type Day struct {
	Date    string            `json:"date"`
	Total   Bucket            `json:"total"`
	Models  map[string]Bucket `json:"models"`
	Account map[string]Bucket `json:"account,omitempty"`
}

type fileShape struct {
	Version int   `json:"version"`
	Days    []Day `json:"days"`
}

// Tracker 用量追踪器。
type Tracker struct {
	path string

	mu   sync.Mutex
	days map[string]*Day
}

// Open 加载历史用量。
func Open(path string) (*Tracker, error) {
	t := &Tracker{path: path, days: map[string]*Day{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return t, nil
	}
	var f fileShape
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	for i := range f.Days {
		d := f.Days[i]
		if d.Models == nil {
			d.Models = map[string]Bucket{}
		}
		if d.Account == nil {
			d.Account = map[string]Bucket{}
		}
		t.days[d.Date] = &d
	}
	return t, nil
}

// Record 记录一次请求。
func (t *Tracker) Record(model, account string, prompt, output, cached int, latency time.Duration, isErr bool) {
	date := time.Now().Format("2006-01-02")
	t.mu.Lock()
	d, ok := t.days[date]
	if !ok {
		d = &Day{Date: date, Models: map[string]Bucket{}, Account: map[string]Bucket{}}
		t.days[date] = d
	}
	apply := func(b *Bucket) {
		b.Requests++
		if isErr {
			b.Errors++
		}
		b.PromptTokens += int64(prompt)
		b.OutputTokens += int64(output)
		b.CachedTokens += int64(cached)
		b.LatencyMs += latency.Milliseconds()
	}
	apply(&d.Total)
	if model != "" {
		b := d.Models[model]
		apply(&b)
		d.Models[model] = b
	}
	if account != "" {
		b := d.Account[account]
		apply(&b)
		d.Account[account] = b
	}
	t.mu.Unlock()
}

// DayView 面板展示用。
type DayView struct {
	Date   string   `json:"date"`
	Total  Bucket   `json:"total"`
	Models []Series `json:"models"`
}

type Series struct {
	Name   string `json:"name"`
	Bucket `json:"bucket"`
}

// Series 返回最近 n 天（含今天）的聚合，按日期升序。
func (t *Tracker) Series(n int) []DayView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]DayView, 0, n)
	for i := n - 1; i >= 0; i-- {
		date := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		v := DayView{Date: date}
		if d, ok := t.days[date]; ok {
			v.Total = d.Total
			for name, b := range d.Models {
				v.Models = append(v.Models, Series{Name: name, Bucket: b})
			}
			sort.Slice(v.Models, func(a, b int) bool { return v.Models[a].Bucket.Requests > v.Models[b].Bucket.Requests })
		}
		out = append(out, v)
	}
	return out
}

// Today 今日总览。
func (t *Tracker) Today() Bucket {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d, ok := t.days[time.Now().Format("2006-01-02")]; ok {
		return d.Total
	}
	return Bucket{}
}

// AllTime 历史总计。
func (t *Tracker) AllTime() Bucket {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum Bucket
	for _, d := range t.days {
		sum.Requests += d.Total.Requests
		sum.Errors += d.Total.Errors
		sum.PromptTokens += d.Total.PromptTokens
		sum.OutputTokens += d.Total.OutputTokens
		sum.CachedTokens += d.Total.CachedTokens
		sum.LatencyMs += d.Total.LatencyMs
	}
	return sum
}

// Save 落盘（节流由调用方控制）。
func (t *Tracker) Save() error {
	t.mu.Lock()
	dates := make([]string, 0, len(t.days))
	for d := range t.days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	// 只保留最近 90 天，避免无限膨胀。
	if len(dates) > 90 {
		for _, d := range dates[:len(dates)-90] {
			delete(t.days, d)
		}
		dates = dates[len(dates)-90:]
	}
	f := fileShape{Version: 1, Days: make([]Day, 0, len(dates))}
	for _, d := range dates {
		f.Days = append(f.Days, *t.days[d])
	}
	t.mu.Unlock()

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".usage-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, t.path)
}
