package audioproto

import "testing"

func TestResolveVoice(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		mapped bool
	}{
		{"alloy", "male-qn-qingse", true},
		{"Nova", "female-shaonv", true},
		{"ONYX", "male-qn-badao", true},
		{"  shimmer  ", "female-tianmei", true},
		{"ballad", "male-qn-badao-jingpin", true},
		// 上游音色原样透传，且大小写保持（voice_id 大小写敏感）。
		{"female-yujie", "female-yujie", false},
		{"English_Graceful_Lady", "English_Graceful_Lady", false},
		{"", "", false},
		{"   ", "", false},
	}
	for _, c := range cases {
		id, mapped := ResolveVoice(c.in)
		if id != c.wantID || mapped != c.mapped {
			t.Fatalf("ResolveVoice(%q) = %q,%v，期望 %q,%v", c.in, id, mapped, c.wantID, c.mapped)
		}
	}
}

func TestOpenAIVoicesIntegrity(t *testing.T) {
	// 映射目标必须落在上游 8 个基础预置音色（含 -jingpin 变体）内：
	// 拼错一个 id 上游会静默回落默认音色，从响应上根本看不出来。
	allowed := map[string]bool{
		"male-qn-qingse": true, "male-qn-jingying": true,
		"male-qn-badao": true, "male-qn-daxuesheng": true,
		"female-shaonv": true, "female-yujie": true,
		"female-chengshu": true, "female-tianmei": true,
	}
	for alias, id := range OpenAIVoices() {
		if alias != lower(alias) {
			t.Fatalf("别名 %q 必须是小写", alias)
		}
		base := id
		if len(base) > len("-jingpin") && base[len(base)-len("-jingpin"):] == "-jingpin" {
			base = base[:len(base)-len("-jingpin")]
		}
		if !allowed[base] {
			t.Fatalf("别名 %q 映射到未知音色 %q", alias, id)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func TestNormalizeSpeechFormat(t *testing.T) {
	cases := []struct {
		in     string
		format string
		native bool
		ok     bool
	}{
		{"", "mp3", true, true},
		{"mp3", "mp3", true, true},
		{"MP3", "mp3", true, true},
		{" wav ", "wav", false, true},
		{"pcm", "pcm", false, true},
		{"opus", "opus", false, true},
		{"aac", "aac", false, true},
		{"flac", "flac", false, true},
		{"ogg", "ogg", false, false},
		{"mp4", "mp4", false, false},
	}
	for _, c := range cases {
		format, native, ok := NormalizeSpeechFormat(c.in)
		if format != c.format || native != c.native || ok != c.ok {
			t.Fatalf("NormalizeSpeechFormat(%q) = %q,%v,%v，期望 %q,%v,%v",
				c.in, format, native, ok, c.format, c.native, c.ok)
		}
	}
}

func TestClampSpeed(t *testing.T) {
	cases := []struct {
		in      float64
		speed   float64
		clamped bool
		ok      bool
	}{
		{0, 0, false, true},
		{1, 1, false, true},
		{0.5, 0.5, false, true},
		{2, 2, false, true},
		{0.25, 0.5, true, true},
		{0.3, 0.5, true, true},
		{3, 2, true, true},
		{4, 2, true, true},
		{0.1, 0, false, false},
		{4.1, 0, false, false},
		{-1, 0, false, false},
	}
	for _, c := range cases {
		speed, clamped, ok := ClampSpeed(c.in)
		if speed != c.speed || clamped != c.clamped || ok != c.ok {
			t.Fatalf("ClampSpeed(%v) = %v,%v,%v，期望 %v,%v,%v",
				c.in, speed, clamped, ok, c.speed, c.clamped, c.ok)
		}
	}
}

func TestNormalizeTranscriptionFormat(t *testing.T) {
	cases := map[string]string{
		"":             TranscribeJSON,
		"json":         TranscribeJSON,
		"TEXT":         TranscribeText,
		"verbose_json": TranscribeVerboseJSON,
		"srt":          TranscribeSRT,
		"vtt":          TranscribeVTT,
	}
	for in, want := range cases {
		got, ok := NormalizeTranscriptionFormat(in)
		if !ok || got != want {
			t.Fatalf("NormalizeTranscriptionFormat(%q) = %q,%v，期望 %q,true", in, got, ok, want)
		}
	}
	if _, ok := NormalizeTranscriptionFormat("yaml"); ok {
		t.Fatal("yaml 不是合法取值，应返回 ok=false")
	}
	if !NeedsTimestamps(TranscribeSRT) || !NeedsTimestamps(TranscribeVTT) || NeedsTimestamps(TranscribeJSON) {
		t.Fatal("NeedsTimestamps 判定错误")
	}
}
