package blogtext

import (
	"strings"
	"testing"
)

func TestAutoSurface(t *testing.T) {
	if !AutoSurface("public") {
		t.Fatal("public should surface")
	}
	if AutoSurface("private") || AutoSurface("password") {
		t.Fatal("private/password must not surface")
	}
}

func TestDefaultSummary(t *testing.T) {
	sample := `$N$ 最大能到 $10^{500}$，普通的遍历必超时，只能用数位 DP。我们在按位枚举填数时，需要维护状态来判断题目给的三个条件。
条件一是判断 3 的倍数，利用特征只需记录当前各数位之和对 3 的余数（` + "`rem`" + `）。
条件二和三涉及具体出现了哪些数字，用一个二进制状态掩码（` + "`mask`" + `）来存数字集合最方便。` + strings.Repeat("填充", 80)
	got := DefaultSummary(sample)
	if got == "" {
		t.Fatal("expected non-empty default summary")
	}
	if !strings.Contains(got, "数位 DP") {
		t.Fatalf("expected content-derived brief, got %q", got)
	}
	if strings.Contains(got, "$") {
		t.Fatalf("math delimiters should be stripped: %q", got)
	}
	if len([]rune(got)) > DefaultSummaryMaxRunes+len([]rune(defaultSummaryEllipsis)) {
		t.Fatalf("too long: %d", len([]rune(got)))
	}
	withCode := "前言\n```go\nfmt.Println(1)\n```\n后记"
	s2 := DefaultSummary(withCode)
	if strings.Contains(s2, "Println") {
		t.Fatalf("code block should be stripped: %q", s2)
	}
	if !strings.Contains(s2, "前言") || !strings.Contains(s2, "后记") {
		t.Fatalf("kept prose: %q", s2)
	}
}

func TestDefaultSummary_stripsHeadingLinkAndBold(t *testing.T) {
	src := `# [Digit Circus](https://vjudge.net/problem/AtCoder-abc465_e)
**涉及知识点：** [[数位DP]]、状态压缩
请你计算满足以下三个条件中恰好一个的整数 $x$ 的个数。
`
	got := DefaultSummary(src)
	if strings.Contains(got, "http") || strings.Contains(got, "**") || strings.Contains(got, "[[") {
		t.Fatalf("noise: %q", got)
	}
	if !strings.Contains(got, "Digit Circus") || !strings.Contains(got, "请你计算") {
		t.Fatalf("body: %q", got)
	}
	// Excerpt / DefaultSummary 同一实现
	if Excerpt(src, DefaultSummaryMaxRunes) != got {
		t.Fatal("Excerpt must match DefaultSummary at default max")
	}
}

func TestIsDefaultSummaryAndResolve(t *testing.T) {
	content := "普通的遍历必超时，只能用数位 DP。我们在按位枚举填数时需要维护状态。"
	def := DefaultSummary(content)
	if !IsDefaultSummary(def, content) {
		t.Fatal("generated must count as default")
	}
	if !IsDefaultSummary("", content) {
		t.Fatal("empty is default")
	}
	// 手写摘要已废弃：一律视为系统生成路径
	if !IsDefaultSummary("我手写的摘要", content) {
		t.Fatal("hand-written no longer special-cased")
	}
	if ResolveSummaryForSave("", content) != def {
		t.Fatal("empty save regenerates")
	}
	if ResolveSummaryForSave("  自定义  ", content) != def {
		t.Fatal("custom input must be ignored; always regenerate from content")
	}
}
