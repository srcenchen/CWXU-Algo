package model

import "testing"

func TestIsAcceptedStatus(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"AC", true},
		{" ac ", true},
		{"OK", true},
		{"Accepted", true},
		{"ACCEPTED", true},
		{"正确", true},
		{"答案正确", true},
		{"WA", false},
		{"Wrong Answer", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsAcceptedStatus(c.in); got != c.want {
			t.Errorf("IsAcceptedStatus(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestFillIsAC(t *testing.T) {
	logs := []SubmitLog{
		{Status: "AC"},
		{Status: "WA"},
		{Status: "  accepted "},
	}
	FillIsACBatch(logs)
	if !logs[0].IsAC || logs[1].IsAC || !logs[2].IsAC {
		t.Fatalf("FillIsACBatch unexpected: %+v", logs)
	}
}

func TestIsLeetCodeSyntheticSubmit(t *testing.T) {
	if IsLeetCodeSyntheticSubmit("LeetCode", "lc-prob-123") {
		t.Fatal("lc-prob should show in activity")
	}
	if !IsLeetCodeSyntheticSubmit("LeetCode", "lc-cal-1-20260101-0") {
		t.Fatal("lc-cal should be hidden")
	}
	if !IsLeetCodeSyntheticSubmit("LeetCode", "lc-pad-1-0") {
		t.Fatal("lc-pad should be hidden")
	}
	if !IsLeetCodeSyntheticSubmit("LeetCode", "lc-ac-1-0") {
		t.Fatal("lc-ac should be hidden")
	}
	if IsLeetCodeSyntheticSubmit("CodeForces", "123") {
		t.Fatal("non-LC never synthetic")
	}
}

func TestCountsTowardDailyAC(t *testing.T) {
	// 官方合成不进日 AC
	if CountsTowardDailyAC("LeetCode", "lc-ac-2-100") {
		t.Fatal("lc-ac must not count toward daily AC")
	}
	// 最近通过进日 AC（与记录列表一致）
	if !CountsTowardDailyAC("LeetCode", "lc-prob-123") {
		t.Fatal("lc-prob should count toward daily AC")
	}
	if !CountsTowardDailyAC("CodeForces", "999") {
		t.Fatal("real OJ AC should count")
	}
}

func TestIsUOJSyntheticAC(t *testing.T) {
	if !IsUOJSyntheticAC("UOJ", "uoj-ac-1-42") {
		t.Fatal("uoj-ac should be synthetic")
	}
	if IsUOJSyntheticAC("UOJ", "12345") {
		t.Fatal("raw uoj id not synthetic")
	}
	if CountsTowardSubmitStat("UOJ", "uoj-ac-1-42") {
		t.Fatal("uoj-ac should not count toward submit heatmap")
	}
	if !IsSyntheticSubmitForFeed("UOJ", "uoj-ac-1-42") {
		t.Fatal("uoj-ac should hide from feed")
	}
}

func TestNormalizeSubmitID(t *testing.T) {
	cases := []struct {
		plat, in, want string
	}{
		{"LuoGu", "LuoGu:286690434", "286690434"},
		{"LuoGu", "286690434", "286690434"},
		{"LuoGu", "  LuoGu:99  ", "99"},
		{"CodeForces", "CodeForces:12345", "12345"},
		{"CodeForces", "CF:99", "99"},
		{"LeetCode", "lc-prob-1", "lc-prob-1"},
		{"LeetCode", "LeetCode:999", "999"},
		{"AtCoder", "AtCoder:abc", "abc"},
		{"", "LuoGu:1", "1"},
	}
	for _, c := range cases {
		if got := NormalizeSubmitID(c.plat, c.in); got != c.want {
			t.Errorf("NormalizeSubmitID(%q,%q)=%q want %q", c.plat, c.in, got, c.want)
		}
	}
}
