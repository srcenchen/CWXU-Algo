package service

import (
	"testing"

	"cwxu-algo/app/core_data/internal/data/model"
)

func TestNowCoderNoAccessIsPermanentNotTransient(t *testing.T) {
	msgs := []string{
		"NowCoder 题面暂无访问权限",
		"NowCoder 题面暂无访问权限，请稍后重试", // 历史文案
		"瞬时失败(退避30s, 自01-01 00:00起可重试至24h): NowCoder 题面暂无访问权限",
		"没有查看题目的权限",
	}
	for _, msg := range msgs {
		if !isPermanentFetchError(msg) {
			t.Errorf("expected permanent: %q", msg)
		}
		if isTransientFetchError(msg) {
			t.Errorf("expected not transient: %q", msg)
		}
		if !isNowCoderNoAccessError(msg) {
			t.Errorf("expected no-access: %q", msg)
		}
	}
}

func TestTransientStillCoversWAFAndDOM(t *testing.T) {
	cases := []string{
		"NowCoder 被 WAF 拦截，请稍后重试",
		"NowCoder 未找到题面 DOM，请稍后重试",
		"NowCoder 需要登录，请稍后重试",
	}
	for _, msg := range cases {
		if isPermanentFetchError(msg) {
			t.Errorf("expected not permanent: %q", msg)
		}
		if !isTransientFetchError(msg) {
			t.Errorf("expected transient: %q", msg)
		}
	}
}

func TestCFFetchStatementNotFoundIsPermanent(t *testing.T) {
	msgs := []string{
		"CF 未找到题面",
		"瞬时失败(退避10m0s, 自08-07 06:08起可重试至24h): CF 未找到题面", // 历史误标瞬时后管理员重试的文案
	}
	for _, msg := range msgs {
		if !isPermanentFetchError(msg) {
			t.Errorf("expected permanent: %q", msg)
		}
		if isTransientFetchError(msg) {
			t.Errorf("expected not transient: %q", msg)
		}
	}
}

func TestLuoGuPermissionErrorsArePermanent(t *testing.T) {
	cases := []string{
		`洛谷 status 401: {"errorMessage":"没有权限请求"}`,
		"洛谷 status 403: 403.6 IP Restricted",
		"洛谷 访问题面无权限",
	}
	for _, msg := range cases {
		p := &model.Problem{Platform: "LuoGu", ErrorMsg: msg}
		if !isLuoguNoAccessError(p) {
			t.Errorf("expected Luogu no-access: %q", msg)
		}
		if !isPermanentFetchErrorForProblem(p) {
			t.Errorf("expected permanent: %q", msg)
		}
		if isTransientFetchErrorForProblem(p) {
			t.Errorf("expected not transient: %q", msg)
		}
	}
}

func TestOtherLuoGuStatusErrorsRemainTransient(t *testing.T) {
	msg := "洛谷 status 503: service unavailable"
	p := &model.Problem{Platform: "LuoGu", ErrorMsg: msg}
	if isLuoguNoAccessError(p) {
		t.Fatalf("ordinary Luogu outage is not a permission error")
	}
	if !isTransientFetchErrorForProblem(p) {
		t.Fatalf("ordinary Luogu outage should remain transient")
	}
}

func TestCFOtherErrorsStillTransient(t *testing.T) {
	cases := []string{
		"CF 被 Cloudflare 拦截，请稍后重试或换网络",
		"CF status 429: too many requests",
		"CF status 503: Service Unavailable",
	}
	for _, msg := range cases {
		if isPermanentFetchError(msg) {
			t.Errorf("expected not permanent: %q", msg)
		}
		if !isTransientFetchError(msg) {
			t.Errorf("expected transient: %q", msg)
		}
	}
}
