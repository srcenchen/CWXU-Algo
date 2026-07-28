package service

import "testing"

func TestTitleFromMarkdownH1(t *testing.T) {
	md := "# 啥博弈\n\n## 题意\n\n正文"
	if got := titleFromMarkdownH1(md); got != "啥博弈" {
		t.Fatalf("got %q", got)
	}
	if got := titleFromMarkdownH1("## 只有二级\n正文"); got != "" {
		t.Fatalf("unexpected %q", got)
	}
	if got := titleFromMarkdownH1("# QOJ.ac\n正文"); got != "" {
		t.Fatalf("brand h1 should skip, got %q", got)
	}
}

func TestShouldReplaceProblemTitle(t *testing.T) {
	if !shouldReplaceProblemTitle("QOJ", "QOJ.ac", "#19004. Foo") {
		t.Fatal("should replace brand")
	}
	if shouldReplaceProblemTitle("QOJ", "#19004. Foo", "QOJ.ac") {
		t.Fatal("should not replace good with brand")
	}
	if shouldReplaceProblemTitle("QOJ", "#19004. Foo", "#19004. Bar") {
		t.Fatal("should not replace good with another good via this helper")
	}
	if !shouldReplaceProblemTitle("QOJ", "", "#1. A") {
		t.Fatal("empty should accept")
	}
}

func TestCleanQOJSubmitTitle(t *testing.T) {
	if got := cleanQOJSubmitTitle("#19004. Local Maxima Game", "19004"); got != "#19004. Local Maxima Game" {
		t.Fatalf("got %q", got)
	}
	if got := cleanQOJSubmitTitle("QOJ.ac", "19004"); got != "#19004" {
		t.Fatalf("brand got %q", got)
	}
	if got := cleanQOJSubmitTitle("# 14806 Bitwise", "14806"); got != "#14806. Bitwise" {
		t.Fatalf("got %q", got)
	}
}
