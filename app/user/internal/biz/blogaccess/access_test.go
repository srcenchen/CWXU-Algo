package blogaccess

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeVisibility(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", VisibilityPublic},
		{" PUBLIC ", VisibilityPublic},
		{"private", VisibilityPrivate},
		{"unlisted", VisibilityPrivate},
		{"hidden", VisibilityPrivate},
		{"password", VisibilityPassword},
	}
	for _, c := range cases {
		if got := NormalizeVisibility(c.in); got != c.want {
			t.Errorf("NormalizeVisibility(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestEvaluate_Public(t *testing.T) {
	a := ArticleAccess{Visibility: VisibilityPublic, OwnerID: 10}
	d := Evaluate(a, 0, false)
	if !d.CanSeeMeta || !d.CanSeeBody || d.RequiresPassword {
		t.Fatalf("public anonymous: %+v", d)
	}
	d2 := Evaluate(a, 99, false)
	if !d2.CanSeeBody {
		t.Fatalf("public other user: %+v", d2)
	}
}

func TestEvaluate_Private(t *testing.T) {
	a := ArticleAccess{Visibility: VisibilityPrivate, OwnerID: 10}
	// stranger
	d := Evaluate(a, 99, false)
	if d.CanSeeMeta || d.CanSeeBody {
		t.Fatalf("private stranger should see nothing: %+v", d)
	}
	if d.Reason != "private" {
		t.Fatalf("reason=%q", d.Reason)
	}
	// anonymous
	d0 := Evaluate(a, 0, false)
	if d0.CanSeeBody {
		t.Fatalf("private anon: %+v", d0)
	}
	// owner
	own := Evaluate(a, 10, false)
	if !own.CanSeeMeta || !own.CanSeeBody {
		t.Fatalf("owner must see private: %+v", own)
	}
	// password flag does not open private
	pw := Evaluate(a, 99, true)
	if pw.CanSeeBody {
		t.Fatalf("passwordOK must not open private: %+v", pw)
	}
}

func TestEvaluate_Password(t *testing.T) {
	a := ArticleAccess{Visibility: VisibilityPassword, OwnerID: 10, HasPassword: true}
	// teaser without password
	d := Evaluate(a, 5, false)
	if !d.CanSeeMeta || d.CanSeeBody || !d.RequiresPassword {
		t.Fatalf("password locked: %+v", d)
	}
	if d.Reason != "password_required" {
		t.Fatalf("reason=%q", d.Reason)
	}
	// unlocked
	ok := Evaluate(a, 5, true)
	if !ok.CanSeeBody || ok.RequiresPassword {
		t.Fatalf("password unlocked: %+v", ok)
	}
	// owner without password still full access
	own := Evaluate(a, 10, false)
	if !own.CanSeeBody || own.RequiresPassword {
		t.Fatalf("owner: %+v", own)
	}
}

func TestCanManage(t *testing.T) {
	if CanManage(1, 0, false) {
		t.Fatal("anon")
	}
	if !CanManage(1, 1, false) {
		t.Fatal("owner")
	}
	if CanManage(1, 2, false) {
		t.Fatal("other")
	}
	if !CanManage(1, 2, true) {
		t.Fatal("site admin")
	}
}

func TestExpandSyncOrgIDs_PrivateImpliesPublic(t *testing.T) {
	isSystem := func(id uint) bool { return id == 1 }
	// only private orgs
	got := ExpandSyncOrgIDs([]uint{3, 5}, 1, isSystem)
	want := []uint{1, 3, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// already has public
	got2 := ExpandSyncOrgIDs([]uint{3, 1}, 1, isSystem)
	if !reflect.DeepEqual(got2, []uint{3, 1}) {
		t.Fatalf("got2 %v", got2)
	}
	// only public — no expansion needed
	got3 := ExpandSyncOrgIDs([]uint{1}, 1, isSystem)
	if !reflect.DeepEqual(got3, []uint{1}) {
		t.Fatalf("got3 %v", got3)
	}
	// empty
	if ExpandSyncOrgIDs(nil, 1, isSystem) != nil {
		t.Fatal("nil selected")
	}
	// dedupe zeros
	got4 := ExpandSyncOrgIDs([]uint{0, 3, 3, 0}, 1, isSystem)
	if !reflect.DeepEqual(got4, []uint{1, 3}) {
		t.Fatalf("got4 %v", got4)
	}
}

func TestThemeEnabled(t *testing.T) {
	if ThemeEnabled(false, nil) {
		t.Fatal("default off")
	}
	if !ThemeEnabled(true, nil) {
		t.Fatal("global all")
	}
	f := false
	if ThemeEnabled(true, &f) {
		t.Fatal("per-user false overrides global")
	}
	tr := true
	if !ThemeEnabled(false, &tr) {
		t.Fatal("per-user true overrides global off")
	}
}

func TestValidVisibility(t *testing.T) {
	if !ValidVisibility("public") || !ValidVisibility("private") || !ValidVisibility("password") {
		t.Fatal("expected valid")
	}
	if ValidVisibility("nope") {
		t.Fatal("invalid")
	}
}

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
条件二和三涉及具体出现了哪些数字，用一个二进制状态掩码（` + "`mask`" + `）来存数字集合最方便。用更多填充文字确保超过默认上限。` + strings.Repeat("填充", 80)
	got := DefaultSummary(sample)
	if got == "" {
		t.Fatal("expected non-empty default summary")
	}
	if !strings.Contains(got, "数位 DP") {
		t.Fatalf("expected content-derived brief, got %q", got)
	}
	if len([]rune(got)) > DefaultSummaryMaxRunes+2 {
		t.Fatalf("too long: %d", len([]rune(got)))
	}
	// fenced code stripped
	withCode := "前言\n```go\nfmt.Println(1)\n```\n后记"
	s2 := DefaultSummary(withCode)
	if strings.Contains(s2, "Println") {
		t.Fatalf("code block should be stripped: %q", s2)
	}
	if !strings.Contains(s2, "前言") || !strings.Contains(s2, "后记") {
		t.Fatalf("kept prose: %q", s2)
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
