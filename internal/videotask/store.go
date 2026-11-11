// Package videotask 保存视频任务的 canonical 快照。
//
// 为什么需要它：上游查询接口的 model 是必填的，而协议客户端轮询时只带 id。
// 所以提交成功那一刻必须把 id → model/参数 记下来；重启后也要能查，
// 故落盘到 data/videotasks.json（原子写，0600）。
package videotask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TTL 任务记录的保留时长。协议里任务 ID 的寿命是 7 天，过期的直接 prune。
const TTL = 7 * 24 * time.Hour

// Params 是要原样回显给客户端的参数。
type Params struct {
	Resolution            string `json:"resolution,omitempty"`
	Ratio                 string `json:"ratio,omitempty"`
	Duration              *int   `json:"duration,omitempty"`
	GenerateAudio         *bool  `json:"generate_audio,omitempty"`
	Watermark             *bool  `json:"watermark,omitempty"`
	Seed                  *int   `json:"seed,omitempty"`
	ServiceTier           string `json:"service_tier,omitempty"`
	SafetyIdentifier      string `json:"safety_identifier,omitempty"`
	ExecutionExpiresAfter *int   `json:"execution_expires_after,omitempty"`
	Priority              *int   `json:"priority,omitempty"`
	OutputFormat          string `json:"output_format,omitempty"`
}

// Record 一个视频任务的完整快照（canonical 语义）。
type Record struct {
	ID            string `json:"id"`
	ClientModel   string `json:"client_model"`
	UpstreamModel string `json:"upstream_model"`
	Status        string `json:"status"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`

	Params Params `json:"params"`

	VideoURL     string `json:"video_url,omitempty"`
	LastFrameURL string `json:"last_frame_url,omitempty"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	// Warnings 是提交期被忽略的字段（上游不支持），轮询时一并回给调用方。
	Warnings []string `json:"warnings,omitempty"`

	// Unnormalized 表示上游返回了六态之外的状态，已收敛为 running。
	Unnormalized bool `json:"unnormalized_status,omitempty"`

	// Usage 只有上游明确给出时才有意义；媒体接口目前不返回 token 用量。
	HasUsage   bool  `json:"has_usage,omitempty"`
	UsageTotal int64 `json:"usage_total,omitempty"`
}

// Store 内存索引 + 原子落盘。
type Store struct {
	mu   sync.RWMutex
	path string
	m    map[string]*Record
}

type file struct {
	Tasks []*Record `json:"tasks"`
}

// Open 从磁盘加载（文件不存在视为空库）。
func Open(path string) (*Store, error) {
	s := &Store{path: path, m: map[string]*Record{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		// 坏文件不阻塞启动：视频任务不是关键路径，丢了可以重新提交。
		return s, nil
	}
	cutoff := time.Now().Add(-TTL).Unix()
	for _, rec := range f.Tasks {
		if rec == nil || rec.ID == "" {
			continue
		}
		if rec.CreatedAt > 0 && rec.CreatedAt < cutoff {
			continue
		}
		s.m[rec.ID] = rec
	}
	return s, nil
}

// NewMemory 无持久化的实例（测试或落盘失败时用）。
func NewMemory() *Store { return &Store{m: map[string]*Record{}} }

// Put 写入并落盘。
func (s *Store) Put(rec *Record) {
	if rec == nil || rec.ID == "" {
		return
	}
	s.mu.Lock()
	s.m[rec.ID] = rec
	s.mu.Unlock()
	s.save()
}

// Get 取一条（返回副本，避免调用方直接改到库里的指针）。
func (s *Store) Get(id string) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.m[id]
	if !ok {
		return nil, false
	}
	cp := *rec
	return &cp, true
}

// List 按创建时间倒序返回前 limit 条。
func (s *Store) List(limit int) ([]*Record, int) {
	s.mu.RLock()
	out := make([]*Record, 0, len(s.m))
	for _, rec := range s.m {
		cp := *rec
		out = append(out, &cp)
	}
	total := len(out)
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

// Delete 删除一条记录。
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	_, ok := s.m[id]
	delete(s.m, id)
	s.mu.Unlock()
	if ok {
		s.save()
	}
	return ok
}

// save 原子写：临时文件 + rename，避免半截 JSON。
func (s *Store) save() {
	if s.path == "" {
		return
	}
	s.mu.RLock()
	f := file{Tasks: make([]*Record, 0, len(s.m))}
	for _, rec := range s.m {
		cp := *rec
		f.Tasks = append(f.Tasks, &cp)
	}
	s.mu.RUnlock()
	sort.Slice(f.Tasks, func(i, j int) bool { return f.Tasks[i].CreatedAt < f.Tasks[j].CreatedAt })

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
