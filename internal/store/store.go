// Package store 负责账号与运行时状态的落盘。
//
// 为什么不塞数据库：这个网关的写路径只有「登录一个账号 / token 刷新 / 改配置」，
// 读路径是每次请求选号（内存里完成）。JSON + 原子 rename 足够，且方便手改、
// 方便备份、方便在容器里挂一个 volume 就走。文件权限固定 0600：
// 里面是明文 token，和 workbuddy2api 的 auths/ 同一级别。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Account 一个 MiniMax Code 账号。
type Account struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	UserID string `json:"user_id,omitempty"`

	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	ExpiresAtMs  int64    `json:"expires_at_ms"`
	Scopes       []string `json:"scopes,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	Audience     string   `json:"audience,omitempty"`
	Generation   int      `json:"generation,omitempty"`

	Disabled  bool   `json:"disabled"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// LastError 最近一次失败原因，面板直接展示，省得翻日志。
	LastError string `json:"last_error,omitempty"`
	// LastUsedAt 最近一次被选中的时间（秒级），用于面板排序。
	LastUsedAt string `json:"last_used_at,omitempty"`
	// RequestCount / TokenCount 累计用量（面板概览用，不参与调度）。
	RequestCount int64 `json:"request_count"`
	PromptTokens int64 `json:"prompt_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// ExpiresAt 返回 access token 的过期时刻。
func (a *Account) ExpiresAt() time.Time {
	if a.ExpiresAtMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(a.ExpiresAtMs)
}

// Expired 是否已过期（留 30 秒余量，避免边界上打过去刚好失效）。
func (a *Account) Expired(now time.Time) bool {
	exp := a.ExpiresAt()
	if exp.IsZero() {
		// 没有过期信息（手工导入的裸 token）：当作长期有效，失败后靠 401 兜底。
		return false
	}
	return now.Add(30 * time.Second).After(exp)
}

// ExpiresWithin 是否会在 d 内过期。
func (a *Account) ExpiresWithin(now time.Time, d time.Duration) bool {
	exp := a.ExpiresAt()
	if exp.IsZero() {
		return false
	}
	return now.Add(d).After(exp)
}

type fileShape struct {
	Version  int        `json:"version"`
	Accounts []*Account `json:"accounts"`
}

// Store 账号集合，带互斥锁与原子落盘。
type Store struct {
	path string

	mu       sync.RWMutex
	accounts map[string]*Account
	order    []string
}

// Open 加载（或初始化）账号文件。
func Open(path string) (*Store, error) {
	s := &Store{path: path, accounts: map[string]*Account{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return s, nil
	}
	var f fileShape
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, a := range f.Accounts {
		if a == nil || a.ID == "" {
			continue
		}
		s.accounts[a.ID] = a
		s.order = append(s.order, a.ID)
	}
	return s, nil
}

// Path 账号文件路径。
func (s *Store) Path() string { return s.path }

// List 返回按创建顺序排列的账号副本。
func (s *Store) List() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.order))
	for _, id := range s.order {
		if a, ok := s.accounts[id]; ok {
			out = append(out, *a)
		}
	}
	return out
}

// Get 取账号副本。
func (s *Store) Get(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// Add 新增账号；ID 冲突时覆盖（便于重复导入同一账号时更新 token）。
func (s *Store) Add(a *Account) error {
	if a.ID == "" {
		return fmt.Errorf("account id required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	if old, ok := s.accounts[a.ID]; ok {
		a.CreatedAt = old.CreatedAt
		a.RequestCount = old.RequestCount
		a.PromptTokens = old.PromptTokens
		a.OutputTokens = old.OutputTokens
	} else {
		s.order = append(s.order, a.ID)
	}
	if a.CreatedAt == "" {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	cp := *a
	s.accounts[a.ID] = &cp
	s.mu.Unlock()
	return s.save()
}

// Update 修改账号（在写锁内完成读改写）。
func (s *Store) Update(id string, fn func(a *Account)) (Account, bool, error) {
	s.mu.Lock()
	a, ok := s.accounts[id]
	if !ok {
		s.mu.Unlock()
		return Account{}, false, nil
	}
	fn(a)
	a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	cp := *a
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return cp, true, err
	}
	return cp, true, nil
}

// TouchUsage 累加用量（高频调用，落盘由调用方按节奏触发）。
func (s *Store) TouchUsage(id string, prompt, output int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[id]; ok {
		a.RequestCount++
		a.PromptTokens += int64(prompt)
		a.OutputTokens += int64(output)
		a.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	}
}

// Remove 删除账号。
func (s *Store) Remove(id string) (bool, error) {
	s.mu.Lock()
	if _, ok := s.accounts[id]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	delete(s.accounts, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	return true, s.save()
}

// Save 显式落盘（改完 Usage 这类内存字段后调用）。
func (s *Store) Save() error { return s.save() }

func (s *Store) save() error {
	s.mu.RLock()
	f := fileShape{Version: 1, Accounts: make([]*Account, 0, len(s.order))}
	for _, id := range s.order {
		if a, ok := s.accounts[id]; ok {
			cp := *a
			f.Accounts = append(f.Accounts, &cp)
		}
	}
	s.mu.RUnlock()

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// SortByCreated 对账号列表按创建时间排序（面板/概览共用）。
func SortByCreated(list []Account) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
}
