// Package reqlog 进程内请求日志环形缓冲（面板「请求日志」视图的数据源）。
//
// 只保留最近的 N 条，重启即清空——真正的审计靠外部日志采集，这里的目标是
// "面板上能立刻看到刚才那条请求为什么失败"，所以宁可少存也不落盘膨胀。
package reqlog

import (
	"sync"
	"time"
)

// Entry 一条请求记录。
type Entry struct {
	Time      time.Time `json:"time"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Model     string    `json:"model"`
	Stream    bool      `json:"stream"`
	Status    int       `json:"status"`
	Duration  int64     `json:"duration_ms"`
	Account   string    `json:"account,omitempty"`
	AccountID string    `json:"account_id,omitempty"`
	Prompt    int       `json:"prompt_tokens"`
	Output    int       `json:"output_tokens"`
	Cached    int       `json:"cached_tokens"`
	TTFB      int64     `json:"ttfb_ms,omitempty"`
	ClientIP  string    `json:"client_ip,omitempty"`
	UA        string    `json:"ua,omitempty"`
	Error     string    `json:"error,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
}

// Log 固定容量的环形缓冲。
type Log struct {
	mu      sync.RWMutex
	buf     []Entry
	size    int
	next    int
	filled  bool
	maxSize int
}

func New(max int) *Log {
	if max <= 0 {
		max = 1000
	}
	return &Log{buf: make([]Entry, max), maxSize: max}
}

func (l *Log) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.next] = e
	l.next = (l.next + 1) % l.maxSize
	if l.next == 0 {
		l.filled = true
	}
	if l.size < l.maxSize {
		l.size++
	}
}

// Recent 返回最近 n 条，新的在前。
func (l *Log) Recent(n int) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > l.size {
		n = l.size
	}
	out := make([]Entry, 0, n)
	idx := l.next - 1
	for i := 0; i < n; i++ {
		if idx < 0 {
			idx = l.maxSize - 1
		}
		if !l.filled && idx >= l.size {
			// 未写满时，有效区间是 [0, size)
			idx--
			continue
		}
		out = append(out, l.buf[idx])
		idx--
	}
	return out
}

// Len 当前条数。
func (l *Log) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.size
}
