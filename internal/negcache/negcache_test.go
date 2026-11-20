package negcache

import (
	"testing"
	"time"
)

func static(ttl time.Duration, max int) Config {
	return func() (time.Duration, int) { return ttl, max }
}

func TestPutGet(t *testing.T) {
	c := New(static(time.Minute, 10))
	c.Put("k", Entry{Status: 451, Code: "content_filter", Message: "blocked"})

	e, ok := c.Get("k")
	if !ok {
		t.Fatal("刚写入的条目应命中")
	}
	if e.Status != 451 || e.Code != "content_filter" || e.Message != "blocked" {
		t.Fatalf("条目字段丢失: %+v", e)
	}
	if _, ok := c.Get("nope"); ok {
		t.Fatal("未写入的 key 不该命中")
	}
}

func TestTTLExpiry(t *testing.T) {
	ttl := 30 * time.Millisecond
	c := New(static(ttl, 10))
	c.Put("k", Entry{Status: 451})
	if _, ok := c.Get("k"); !ok {
		t.Fatal("过期前应命中")
	}
	time.Sleep(ttl + 10*time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("过期后不该命中")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("过期项应被清除，剩余 %d 条", n)
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(static(time.Minute, 2))
	c.Put("a", Entry{Status: 451})
	c.Put("b", Entry{Status: 451})
	// 访问 a，让 b 变成最久未用
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 应命中")
	}
	c.Put("c", Entry{Status: 451}) // 触发淘汰
	if _, ok := c.Get("b"); ok {
		t.Fatal("b 是最久未用的，应被淘汰")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s 不该被淘汰", k)
		}
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("容量上限 2，实际 %d", n)
	}
}

func TestDisabled(t *testing.T) {
	c := New(static(0, 10))
	c.Put("k", Entry{Status: 451})
	if _, ok := c.Get("k"); ok {
		t.Fatal("ttl=0 时应视为关闭")
	}

	// nil 缓存同样要安全（Server 未装配时不能 panic）。
	var nilCache *Cache
	if _, ok := nilCache.Get("k"); ok {
		t.Fatal("nil 缓存不该命中")
	}
	nilCache.Put("k", Entry{})
	if n := nilCache.Len(); n != 0 {
		t.Fatalf("nil 缓存 Len 应为 0，实际 %d", n)
	}
}

func TestConfigHotReload(t *testing.T) {
	ttl := time.Minute
	c := New(func() (time.Duration, int) { return ttl, 10 })
	c.Put("k", Entry{Status: 451})
	if _, ok := c.Get("k"); !ok {
		t.Fatal("应命中")
	}
	// 面板把 ttl 改成 0（关闭）后，缓存应立即失效，而不是等重启。
	ttl = 0
	if _, ok := c.Get("k"); ok {
		t.Fatal("ttl 改成 0 后不该命中")
	}
}
