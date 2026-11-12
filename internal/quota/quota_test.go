package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Origin 要从 /mavis 长路径推回站点根：切 region 时额度接口跟着走。
func TestOrigin(t *testing.T) {
	cases := map[string]string{
		"https://agent.minimax.cn/mavis/api/v1/llm/v1": "https://agent.minimax.cn",
		"http://127.0.0.1:9999":                        "http://127.0.0.1:9999",
		"https://agent.minimax.io/mavis/api/v1/llm/v1": "https://agent.minimax.io",
		"":          "",
		"not a url": "",
	}
	for in, want := range cases {
		if got := Origin(in); got != want {
			t.Fatalf("Origin(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestImageFreeParses(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":10,"used":1,"remaining":9,"active":true,"start_ms":1,"end_ms":2}`))
	}))
	defer srv.Close()

	q, err := New(nil).ImageFree(context.Background(), srv.URL, "mmoat_test")
	if err != nil {
		t.Fatalf("ImageFree: %v", err)
	}
	if gotPath != "/minimax-cloud/api/v1/image/free-quota" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer mmoat_test" {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if q.Total != 10 || q.Used != 1 || q.Remaining != 9 || !q.Active {
		t.Fatalf("解析结果 = %+v", q)
	}
	if q.FetchedAt.IsZero() {
		t.Fatalf("FetchedAt 未填")
	}
}

// 401 要能被上层识别成"凭据问题"，不能当成"没有额度"。
func TestImageFreeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid access token"}`))
	}))
	defer srv.Close()

	if _, err := New(nil).ImageFree(context.Background(), srv.URL, "bad"); err == nil {
		t.Fatalf("401 应报错")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer bad.Close()
	if _, err := New(nil).ImageFree(context.Background(), bad.URL, "x"); err == nil {
		t.Fatalf("非 JSON 应报错")
	}

	empty := New(nil)
	if _, err := empty.ImageFree(context.Background(), "", "x"); err == nil {
		t.Fatalf("空 origin 应报错")
	}
}
