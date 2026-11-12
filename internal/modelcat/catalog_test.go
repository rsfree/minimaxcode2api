package modelcat

import (
	"strings"
	"testing"

	"github.com/linguo2625469/minimaxcode2api/internal/videoproto"
)

// 清单的基本卫生：id 唯一、别名不跨模型撞车、对话模型必须有能力值。
//
// 别名撞车是最隐蔽的一种坏：Lookup 先匹配到谁就返回谁，后加的那个别名会
// 永远静默失效（客户端看到模型名解析到另一个模型上去）。
func TestCatalogIntegrity(t *testing.T) {
	byName := map[string]string{} // 名字(小写) -> 归属 id
	for _, m := range All() {
		if m.ID == "" {
			t.Fatal("存在空 id")
		}
		if key := strings.ToLower(m.ID); byName[key] != "" {
			t.Fatalf("id %q 重复（已归 %s）", m.ID, byName[key])
		}
		byName[strings.ToLower(m.ID)] = m.ID
	}
	for _, m := range All() {
		for _, a := range m.Aliases {
			if a == "" {
				t.Fatalf("%s: 存在空别名", m.ID)
			}
			key := strings.ToLower(a)
			if owner, ok := byName[key]; ok && owner != m.ID {
				t.Fatalf("别名 %q 同时属于 %s 与 %s", a, owner, m.ID)
			}
			byName[key] = m.ID
		}
	}

	for _, m := range All() {
		switch m.Kind {
		case Chat:
			if m.ContextWindow <= 0 || m.MaxOutput <= 0 {
				t.Fatalf("%s: 对话模型缺上下文/输出上限", m.ID)
			}
			if m.Thinking != "forced_adaptive" && m.Thinking != "switchable" {
				t.Fatalf("%s: thinking = %q，未知取值", m.ID, m.Thinking)
			}
		case Image, Video:
			// 媒体模型没有 token 概念，带上这些数字只会让客户端误判能吃 messages。
			if m.ContextWindow != 0 || m.MaxOutput != 0 || m.Thinking != "" {
				t.Fatalf("%s: 媒体模型不该带对话能力字段", m.ID)
			}
		default:
			t.Fatalf("%s: 未知类别 %q", m.ID, m.Kind)
		}
	}
}

// 视频清单是「发现面」，videoproto 是「受理面」——两边必须一致。
//
// 只列不受理会变成"选了就报错"，只受理不列则是"看不见但能用"。这里用测试钉住
// 而不是让 modelcat import videoproto：类别目录不该依赖具体协议的实现。
func TestVideoEntriesAgreeWithProto(t *testing.T) {
	for _, m := range All() {
		if m.Kind != Video {
			continue
		}
		want, ok := videoproto.ResolveModel(m.ID)
		if !ok {
			t.Fatalf("%s: /v1/models 列了它，但 videoproto 不受理", m.ID)
		}
		for _, name := range append([]string{m.ID}, m.Aliases...) {
			got, ok := videoproto.ResolveModel(name)
			if !ok {
				t.Fatalf("%s 的别名 %q 解析不了", m.ID, name)
			}
			if got != want {
				t.Fatalf("%s 的别名 %q 解析到 %s，与主名 %s 不一致", m.ID, name, got, want)
			}
		}
	}
}

// Lookup 必须 id / 别名都认，且大小写不敏感（客户端经常把模型名转小写）。
func TestLookup(t *testing.T) {
	cases := map[string]string{
		"MiniMax-H3":         "MiniMax-H3",
		"h3":                 "MiniMax-H3",
		"MINIMAX-HAILUO-2.3": "MiniMax-Hailuo-2.3",
		"h2.3":               "MiniMax-Hailuo-2.3",
		"image":              "MiniMax-Image",
		"m3":                 "MiniMax-M3",
		"":                   "",
		"gpt-4o":             "",
	}
	for name, wantID := range cases {
		got, ok := Lookup(name)
		if wantID == "" {
			if ok {
				t.Fatalf("Lookup(%q) 不该命中，得到 %s", name, got.ID)
			}
			continue
		}
		if !ok || got.ID != wantID {
			t.Fatalf("Lookup(%q) = %q,%v，期望 %q", name, got.ID, ok, wantID)
		}
	}
}
