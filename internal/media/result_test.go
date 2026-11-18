package media

import "testing"

// 这些断言只是把"我们认哪些键"钉住，避免以后改提取逻辑时悄悄漏掉一种形态。
// 真值以接下来第一次真实调用为准：认不出来时调用方会显式报错，不会静默给空。
func TestOutputURLs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"顶层 output_url", `{"code":0,"output_url":"https://a/1.png"}`, []string{"https://a/1.png"}},
		{"顶层 cdn_url", `{"code":0,"cdn_url":"https://a/2.png"}`, []string{"https://a/2.png"}},
		{"success_items", `{"code":0,"success_items":[{"output_url":"https://a/3.mp4"}]}`, []string{"https://a/3.mp4"}},
		{"data 数组", `{"code":0,"data":[{"url":"https://a/4.png"}]}`, []string{"https://a/4.png"}},
		{"data 对象", `{"code":0,"data":{"output_url":"https://a/5.png"}}`, []string{"https://a/5.png"}},
		{"去重保序", `{"code":0,"output_url":"https://a/6.png",
			"success_items":[{"output_url":"https://a/6.png"},{"output_url":"https://a/7.png"}]}`,
			[]string{"https://a/6.png", "https://a/7.png"}},
		{"没有产物", `{"code":0,"message":"ok"}`, nil},
	}
	for _, c := range cases {
		got := OutputURLs([]byte(c.raw))
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v want %v", c.name, got, c.want)
			}
		}
	}
}

func TestTexts(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"results[].text", `{"code":0,"results":[{"text":"甲"},{"text":"乙"}]}`, []string{"甲", "乙"}},
		{"results 字符串数组", `{"code":0,"results":["甲","乙"]}`, []string{"甲", "乙"}},
		{"success_items 的 text_response", `{"code":0,"success_items":[{"text_response":"丙"}]}`, []string{"丙"}},
		{"data[].content", `{"code":0,"data":[{"content":"丁"}]}`, []string{"丁"}},
		{"顶层 transcription", `{"code":0,"transcription":"戊"}`, []string{"戊"}},
		{"认不出来", `{"code":0,"foo":"bar"}`, nil},
	}
	for _, c := range cases {
		got := Texts([]byte(c.raw))
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v want %v", c.name, got, c.want)
			}
		}
	}
}

// FirstArray 的布尔值表达的是"字段在不在"，不是"数组空不空"——
// 空数组是上游明确说没有结果，缺字段才是结构变了。
func TestFirstArray(t *testing.T) {
	if _, ok := FirstArray([]byte(`{"code":0,"results":[]}`)); !ok {
		t.Fatal("空数组也必须算找到")
	}
	list, ok := FirstArray([]byte(`{"code":0,"items":[{"a":1}]}`))
	if !ok || len(list) != 1 {
		t.Fatalf("items 未识别: %v %v", list, ok)
	}
	if _, ok := FirstArray([]byte(`{"code":0}`)); ok {
		t.Fatal("没有数组字段却报告找到了")
	}
}

func TestFailureText(t *testing.T) {
	if got := FailureText([]byte(`{"code":0,"failed_items":[{"error_msg":"额度不足"}]}`)); got != "额度不足" {
		t.Fatalf("failed_items: %q", got)
	}
	if got := FailureText([]byte(`{"error":"boom"}`)); got != "boom" {
		t.Fatalf("error string: %q", got)
	}
	if got := FailureText([]byte(`{"error":{"message":"bad"}}`)); got != "bad" {
		t.Fatalf("error object: %q", got)
	}
	if got := FailureText([]byte(`{"code":0,"message":"ok"}`)); got != "" {
		t.Fatalf("成功响应不该有失败原因: %q", got)
	}
}
