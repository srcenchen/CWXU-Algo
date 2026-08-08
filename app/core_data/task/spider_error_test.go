package task

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatSpiderLastError_CF403HTML(t *testing.T) {
	raw := errors.New(`all platforms failed for user 2: 请求响应码错误 403, <html> <head><title>403 Forbidden</title></head> <body> <center><h1>403 Forbidden</h1></center> <hr><center>nginx/1.27.4</center> <script type="modul`)
	got := FormatSpiderLastError("CodeForces", raw)
	if !strings.Contains(got, "Codeforces") {
		t.Fatalf("want platform name, got %q", got)
	}
	if !strings.Contains(got, "一般不是账号问题") {
		t.Fatalf("want system fault hint, got %q", got)
	}
	if strings.Contains(got, "<html") || strings.Contains(got, "nginx") || strings.Contains(got, "all platforms") {
		t.Fatalf("must not leak raw html/wrapper: %q", got)
	}
	if strings.Contains(got, "请检查绑定的用户名") {
		t.Fatalf("403 must not blame username: %q", got)
	}
}

func TestFormatSpiderLastError_UserNotFound(t *testing.T) {
	raw := errors.New(`codeforces user.rating: handles: User with handle xxx not found`)
	got := FormatSpiderLastError("CodeForces", raw)
	if !strings.Contains(got, "请检查绑定的用户名") {
		t.Fatalf("want user fault, got %q", got)
	}
	if strings.Contains(got, "一般不是账号问题") {
		t.Fatalf("user fault should not say system: %q", got)
	}
}

func TestFormatSpiderLastError_Timeout(t *testing.T) {
	raw := errors.New(`发起http请求失败: context deadline exceeded`)
	got := FormatSpiderLastError("AtCoder", raw)
	if !strings.Contains(got, "AtCoder") || !strings.Contains(got, "超时") {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got, "一般不是账号问题") {
		t.Fatalf("timeout is system: %q", got)
	}
}

func TestClassifyStripsHTML(t *testing.T) {
	_, reason := classifySpiderErr(`请求响应码错误 502, <html>oops</html> more`)
	if strings.Contains(reason, "<") {
		t.Fatalf("reason still has html: %q", reason)
	}
}

func TestIsUserSideSpiderErr(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"leetcode user not found", `leetcode 用户不存在或资料不可见: abc`, true},
		{"leetcode ranking not found", `leetcode ranking: user abc not found in nearby pages for weekly-contest-400`, true},
		{"codeforces handle not found", `codeforces user.rating: handles: User with handle xxx not found`, true},
		{"luogu fe injection missing", `LuoGu sync failed user=35: 未找到 _feInjection`, true},
		{"timeout", `发起http请求失败: context deadline exceeded`, false},
		{"403 html", `请求响应码错误 403, <html>Forbidden</html>`, false},
		{"rate limited", `429 too many requests`, false},
		{"empty error", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsUserSideSpiderErr("LeetCode", errors.New(c.raw))
			if got != c.want {
				t.Fatalf("IsUserSideSpiderErr(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
	if IsUserSideSpiderErr("LeetCode", nil) {
		t.Fatal("nil error must not be user-side")
	}
}

func TestFormatSpiderLastError_LuoGuFeInjection(t *testing.T) {
	raw := errors.New(`LuoGu sync failed user=35: 未找到 _feInjection`)
	got := FormatSpiderLastError("LuoGu", raw)
	if !strings.Contains(got, "洛谷") {
		t.Fatalf("want platform name, got %q", got)
	}
	if !strings.Contains(got, "请稍后再试") {
		t.Fatalf("want transient hint, got %q", got)
	}
	if strings.Contains(got, "请检查绑定的用户名") {
		t.Fatalf("transient must not blame username: %q", got)
	}
	if strings.Contains(got, "一般不是账号问题") {
		t.Fatalf("transient must not say system: %q", got)
	}
}
