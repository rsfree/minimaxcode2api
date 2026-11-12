package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// 额度是旁路信息：60 秒内重复查询必须命中缓存，不能每次都打上游
// （面板每进一次概览就会问一次，不缓存会把上游打成筛子）。
func TestImageQuotaCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/minimax-cloud/api/v1/image/free-quota") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"total":10,"used":1,"remaining":9,"active":true}`)
	}, "tok1")

	acct := f.srv.Store.List()[0]
	q, err := f.srv.ImageQuotaForAccount(context.Background(), acct)
	if err != nil {
		t.Fatalf("首次取额度: %v", err)
	}
	if q.Total != 10 || q.Used != 1 || q.Remaining != 9 {
		t.Fatalf("额度 = %+v", q)
	}
	if _, err := f.srv.ImageQuotaForAccount(context.Background(), acct); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("60s 内应只打一次上游，实际 %d 次", n)
	}
}

// 上游报错时也要缓存（30s），否则面板刷新会把抖动放大成风暴；
// 同时错误必须原样返回，不能伪装成"0 额度"。
func TestImageQuotaErrorCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}, "tok1")

	acct := f.srv.Store.List()[0]
	for i := 0; i < 3; i++ {
		if _, err := f.srv.ImageQuotaForAccount(context.Background(), acct); err == nil {
			t.Fatalf("第 %d 次应返回错误", i+1)
		}
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("错误也应缓存，实际打了 %d 次", n)
	}
}
