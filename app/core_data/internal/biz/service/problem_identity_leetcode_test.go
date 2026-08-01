package service

import (
	"strings"
	"testing"
)

func TestParseLeetCodeIdentity(t *testing.T) {
	p, err := ParseProblemIdentity("LeetCode", "leetcode", "two-sum 两数之和")
	if err != nil {
		t.Fatal(err)
	}
	if p.ExternalID != "two-sum" || p.SkipBank || p.SkipFetch {
		t.Fatalf("got %+v", p)
	}
	if !strings.Contains(p.URL, "two-sum") {
		t.Fatalf("url=%s", p.URL)
	}
	if p.Title != "两数之和" {
		t.Fatalf("title=%s", p.Title)
	}

	// 剑指 Offer II / LCR：titleSlug 为混合大小写短码（非 two-sum 风格）
	lcr, err := ParseProblemIdentity("LeetCode", "leetcode", "iIQa4I LCR 038. 每日温度")
	if err != nil {
		t.Fatalf("LCR slug: %v", err)
	}
	if lcr.ExternalID != "iIQa4I" {
		t.Fatalf("LCR external=%q want iIQa4I", lcr.ExternalID)
	}
	if lcr.Title != "LCR 038. 每日温度" {
		t.Fatalf("LCR title=%q", lcr.Title)
	}
	if !strings.Contains(lcr.URL, "iIQa4I") {
		t.Fatalf("LCR url=%s", lcr.URL)
	}
	lcr2, err := ParseProblemIdentity("LeetCode", "leetcode", "8Zf90G 逆波兰表达式求值")
	if err != nil {
		t.Fatalf("LCR2: %v", err)
	}
	if lcr2.ExternalID != "8Zf90G" || lcr2.Title != "逆波兰表达式求值" {
		t.Fatalf("LCR2=%+v", lcr2)
	}

	_, err = ParseProblemIdentity("LeetCode", "leetcode", "lc-ac-problem-3")
	if err == nil {
		t.Fatal("synthetic should fail")
	}
	_, err = ParseProblemIdentity("LeetCode", "leetcode", "leetcode-submit")
	if err == nil {
		t.Fatal("calendar submit should fail")
	}
}
