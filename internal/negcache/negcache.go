// Package negcache 进程内「负缓存」：记住某条内容被审核拒绝过，短时间内
// 同样的请求直接回同一条错误，不再打上游。
//
// 为什么需要它：上游出图是槽位制、按张计费，一条被审核拒绝的 prompt 每次
// 重放都要真打一次上游（约 20s 占一个槽位）。批量跑同一批 prompt 时会反复
// 撞同一面墙 —— 既浪费额度，也挤占正常请求的槽位。审核判定在分钟级是稳定的，
// 缓存十分钟既挡得住重放，又不至于挡住用户改完内容再试。
//
// 只缓存**内容层面的终态拒绝**（451 / content_filter），不缓存任何可重试的
// 错误：把瞬时失败也缓存住，等于自己制造故障。
package negcache

import (
	"container/list"
	"sync"
	"time"
)

// Entry 一条被缓存的拒绝结果。
type Entry struct {
	Status  int
	Code    string
	Message string
	expire  time.Time
}

// Config 返回当前的 (ttl, max)。做成闭包而不是构造参数，是为了让面板改配置
// 后立刻生效 —— 缓存的生命周期比配置长，参数不能只在启动时读一次。
type Config func() (ttl time.Duration, max int)

// Cache 带 TTL 与容量上限的负缓存。零值不可用，请用 New。
type Cache struct {
	mu  sync.Mutex
	cfg Config
	ll  *list.List // Front = 最近使用
	m   map[string]*list.Element
}

type item struct {
	key string
	e   Entry
}

// New 构造缓存。cfg 为 nil 时视为关闭（Get 永远 miss，Put 不生效）。
func New(cfg Config) *Cache {
	return &Cache{cfg: cfg, ll: list.New(), m: map[string]*list.Element{}}
}

func (c *Cache) params() (time.Duration, int) {
	if c == nil || c.cfg == nil {
		return 0, 0
	}
	ttl, max := c.cfg()
	if max <= 0 {
		max = 1000
	}
	return ttl, max
}

// Get 查一条；命中且未过期时把条目移到队首。
func (c *Cache) Get(key string) (Entry, bool) {
	if c == nil || key == "" {
		return Entry{}, false
	}
	ttl, _ := c.params()
	if ttl <= 0 {
		return Entry{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return Entry{}, false
	}
	it := el.Value.(*item)
	if !time.Now().Before(it.e.expire) {
		c.ll.Remove(el)
		delete(c.m, key)
		return Entry{}, false
	}
	c.ll.MoveToFront(el)
	return it.e, true
}

// Put 写入一条；按当前 ttl 计算过期时间，超容量时淘汰最久未使用的。
func (c *Cache) Put(key string, e Entry) {
	if c == nil || key == "" {
		return
	}
	ttl, max := c.params()
	if ttl <= 0 || max <= 0 {
		return
	}
	e.expire = time.Now().Add(ttl)

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		it := el.Value.(*item)
		it.e = e
		c.ll.MoveToFront(el)
		return
	}
	c.m[key] = c.ll.PushFront(&item{key: key, e: e})
	for c.ll.Len() > max {
		el := c.ll.Back()
		if el == nil {
			break
		}
		c.ll.Remove(el)
		delete(c.m, el.Value.(*item).key)
	}
}

// Len 当前条数（含未清理的过期项）。
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
