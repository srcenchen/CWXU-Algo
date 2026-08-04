package platform

import (
	"testing"
)

// 对公开 API 做轻量实测；网络/平台抖动时允许 Skip，不阻断 CI 本地无网。

func TestFetchRating_Codeforces(t *testing.T) {
	r, has, err := NewCodeforces{}.FetchRating("tourist")
	if err != nil {
		t.Skipf("network/API: %v", err)
	}
	if !has || r < 1000 {
		t.Fatalf("tourist rating unexpected: has=%v r=%d", has, r)
	}
	t.Logf("CF tourist=%d", r)
}

func TestFetchRating_AtCoder(t *testing.T) {
	r, has, err := NewAtCoder{}.FetchRating("tourist")
	if err != nil {
		t.Skipf("network/API: %v", err)
	}
	if !has || r < 1000 {
		t.Fatalf("tourist rating unexpected: has=%v r=%d", has, r)
	}
	t.Logf("AtCoder tourist=%d", r)
}

func TestFetchRating_LeetCode(t *testing.T) {
	r, has, err := NewLeetCode{}.FetchRating("zerotrac2")
	if err != nil {
		t.Skipf("network/API: %v", err)
	}
	if !has || r < 1000 {
		t.Fatalf("zerotrac2 rating unexpected: has=%v r=%d", has, r)
	}
	t.Logf("LeetCode zerotrac2=%d", r)
}

func TestFetchRating_NowCoder(t *testing.T) {
	// 978880410 = aoralsfout，公开主页有 Rating；WAF 拦截时应返回 err，不能静默 has=false
	r, has, err := NewNowCoder{}.FetchRating("978880410")
	if err != nil {
		t.Fatalf("FetchRating NowCoder: %v (若持续 WAF，检查浏览器态 UA)", err)
	}
	t.Logf("NowCoder 978880410 has=%v r=%d", has, r)
	if !has || r < 100 {
		t.Fatalf("expected real rating for 978880410, has=%v r=%d (silent empty = WAF/parse bug)", has, r)
	}
}

func TestFetchRating_Codeforces_UnratedEmpty(t *testing.T) {
	// 空 handle 应报错
	_, _, err := NewCodeforces{}.FetchRating("")
	if err == nil {
		t.Fatal("expected error for empty handle")
	}
}

func TestFetchRating_LuoGu(t *testing.T) {
	// 8457 = chen_zhe，有 Elo；983446 文档示例，通常无参赛
	lg := &NewLuoGu{}
	r, has, err := lg.FetchRating("8457")
	if err != nil {
		t.Skipf("network/login: %v", err)
	}
	if !has || r < 1000 {
		t.Fatalf("chen_zhe rating unexpected: has=%v r=%d", has, r)
	}
	t.Logf("LuoGu 8457=%d", r)

	_, has2, err := lg.FetchRating("983446")
	if err != nil {
		t.Skipf("network/login: %v", err)
	}
	t.Logf("LuoGu 983446 has=%v", has2)
}
