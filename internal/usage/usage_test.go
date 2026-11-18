package usage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRecordMediaIsSeparateFromTokenBucket 锁定「媒体不污染 token 桶」这条约束。
//
// 背景：图片/视频没有 token（上游不返回）。如果并进 Total，面板上的
// 「今日请求」会暴涨而「今日 Token」不动，看起来像模型突然变便宜。
func TestRecordMediaIsSeparateFromTokenBucket(t *testing.T) {
	tr, err := Open(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tr.Record("MiniMax-M3.1-Flash", "acct-1", 100, 50, 20, 1200*time.Millisecond, false)
	tr.RecordMedia("MiniMax-Image", 15*time.Second, false)
	tr.RecordMedia("MiniMax-Image", 57*time.Second, false)
	tr.RecordMedia("image", 2*time.Second, true) // 失败请求没有 model，按路径归类

	today := tr.Today()
	if today.Requests != 1 {
		t.Errorf("文本桶 Today().Requests 应只统计文本调用，实际 %d", today.Requests)
	}
	if today.PromptTokens != 100 {
		t.Errorf("文本桶 token 被污染：%d", today.PromptTokens)
	}

	media := tr.TodayMedia()
	if media.Requests != 3 {
		t.Errorf("媒体桶应有 3 次调用，实际 %d", media.Requests)
	}
	if media.Errors != 1 {
		t.Errorf("媒体桶失败数应为 1，实际 %d", media.Errors)
	}
	if media.LatencyMs != 74000 {
		t.Errorf("媒体桶耗时应累计 74000ms，实际 %d", media.LatencyMs)
	}

	if all := tr.AllTimeMedia(); all.Requests != 3 {
		t.Errorf("AllTimeMedia 应为 3，实际 %d", all.Requests)
	}
	// 文本 AllTime 不应包含媒体
	if all := tr.AllTime(); all.Requests != 1 {
		t.Errorf("AllTime 文本请求应仍为 1，实际 %d", all.Requests)
	}
}

func TestRecordMediaSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	tr, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tr.RecordMedia("MiniMax-Image", 10*time.Second, false)
	if err := tr.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	if got := again.TodayMedia().Requests; got != 1 {
		t.Errorf("重启后媒体计数应保留，实际 %d", got)
	}
}

func TestSeriesCarriesMedia(t *testing.T) {
	tr, _ := Open(filepath.Join(t.TempDir(), "usage.json"))
	tr.RecordMedia("MiniMax-Image", time.Second, false)

	s := tr.Series(1)
	if len(s) != 1 {
		t.Fatalf("Series(1) 应有 1 天，实际 %d", len(s))
	}
	if s[0].MediaTotal.Requests != 1 {
		t.Errorf("Series 应带 media_total，实际 %+v", s[0].MediaTotal)
	}
	if len(s[0].Media) != 1 || s[0].Media[0].Name != "MiniMax-Image" {
		t.Errorf("Series 应带 media 明细，实际 %+v", s[0].Media)
	}
}
