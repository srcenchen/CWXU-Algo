package problem_fetch

import "testing"

func TestSelectVJudgeStatementPrefersOfficialChinese(t *testing.T) {
	briefs := []VJudgeStatementBrief{
		{Key: 1, Lang: "en", Type: "main", Official: true, PublicVisible: true},
		{Key: 2, Lang: "zh", Type: "user", PublicVisible: true},
		{Key: 3, Lang: "zh", Type: "main", Official: true, MainOfficial: true, PublicVisible: true},
	}
	got, ok := selectVJudgeStatement(briefs)
	if !ok || got.Key != 3 {
		t.Fatalf("selected=%+v ok=%v, want official Chinese main", got, ok)
	}
}

func TestSelectVJudgeStatementFallsBackToOfficialEnglish(t *testing.T) {
	briefs := []VJudgeStatementBrief{
		{Key: 1, Lang: "en", Type: "main", Official: true, PublicVisible: true},
		{Key: 2, Lang: "zh", Type: "user", PublicVisible: false},
	}
	got, ok := selectVJudgeStatement(briefs)
	if !ok || got.Key != 1 {
		t.Fatalf("selected=%+v ok=%v, want official English", got, ok)
	}
}

func TestValidateVJudgeIdentity(t *testing.T) {
	if err := validateVJudgeIdentity("LuoGu", "P1175", "洛谷", "P1175"); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	if err := validateVJudgeIdentity("CodeForces", "1791A", "洛谷", "1791A"); err == nil {
		t.Fatal("platform mismatch accepted")
	}
	if err := validateVJudgeIdentity("AtCoder", "abc397_e", "AtCoder", "abc397_f"); err == nil {
		t.Fatal("problem mismatch accepted")
	}
}

func TestVJudgeDescriptionToMarkdown(t *testing.T) {
	d := VJudgeDescription{
		Lang: "zh",
		Sections: []VJudgeSection{
			{Title: "题目描述", Value: VJudgeSectionValue{Format: "MD", Content: "给出一个数 $n$。"}},
			{Title: "输入", Value: VJudgeSectionValue{Format: "HTML", Content: "<p>一个整数 <var>n</var>。</p>"}},
			{Title: "样例", Value: VJudgeSectionValue{Format: "HTML", Content: `<table class="vjudge_sample"><tr><th>Input</th><th>Output</th></tr><tr><td><pre>1</pre></td><td><pre>2</pre></td></tr></table>`}},
		},
	}
	got, err := vjudgeDescriptionToMarkdown(d, "https://cdn.example/")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 题目描述", "给出一个数 $n$。", "## 输入", "$n$", "## 样例", "**输入**", "```\n1\n```", "**输出**", "```\n2\n```"} {
		if !containsText(got, want) {
			t.Fatalf("markdown missing %q:\n%s", want, got)
		}
	}
}

func TestVJudgeDescriptionReplacesCDNPlaceholder(t *testing.T) {
	d := VJudgeDescription{Sections: []VJudgeSection{{Title: "Hint", Value: VJudgeSectionValue{Format: "MD", Content: "![x](CDN_BASE_URL/image/a.png)"}}}}
	got, err := vjudgeDescriptionToMarkdown(d, "https://cdn.example/")
	if err != nil {
		t.Fatal(err)
	}
	if containsText(got, "CDN_BASE_URL") || !containsText(got, "https://cdn.example/image/a.png") {
		t.Fatalf("CDN placeholder not replaced: %s", got)
	}
}

func TestNormalizeVJudgeProblemID(t *testing.T) {
	cases := []struct {
		platform, input, want string
	}{
		{"LuoGu", "p1175", "P1175"},
		{"CodeForces", "1791A", "1791A"},
		{"AtCoder", "ABC397_E", "abc397_e"},
		{"QOJ", "1000", "1000"},
	}
	for _, tc := range cases {
		got, err := normalizeVJudgeProblemID(tc.platform, tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%s/%s = %q, %v; want %q", tc.platform, tc.input, got, err, tc.want)
		}
	}
}

func TestFetchWithSourcesSkipsDisabledSources(t *testing.T) {
	_, err := FetchWithSources(nil, "LuoGu", "P1175", "", nil, StatementSourcePolicy{}, "", "")
	if err == nil || err.Error() != "题面来源均已关闭" {
		t.Fatalf("error=%v, want disabled source error", err)
	}
}

func containsText(s, want string) bool {
	return len(want) == 0 || indexText(s, want) >= 0
}

func indexText(s, want string) int {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return i
		}
	}
	return -1
}
