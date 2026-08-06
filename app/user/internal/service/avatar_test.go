package service

import "testing"

func TestLocalAvatarRelPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/api/user/static/avatar/27/20260730_ab.jpg", "avatar/27/20260730_ab.jpg"},
		{"/v1/user/static/avatar/27/20260730_ab.jpg", "avatar/27/20260730_ab.jpg"},
		{"https://algo.zhiyuansofts.cn/api/user/static/avatar/27/x.jpg", "avatar/27/x.jpg"},
		{"", ""},
		{"/api/user/static/site/27/x.jpg", ""},
		{"/avatar/27/a1b2.jpg", ""},
		{"/api/user/static/avatar/../etc/passwd", ""},
	}
	for _, c := range cases {
		if got := localAvatarRelPath(c.in); got != c.want {
			t.Errorf("localAvatarRelPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAvatarForStoreService(t *testing.T) {
	if got := normalizeAvatarForStore("https://zhiyuansofts.cn/avatar/27/a1b2.jpg"); got != "/avatar/27/a1b2.jpg" {
		t.Errorf("normalize full url: %s", got)
	}
	if got := normalizeAvatarForStore("/api/user/static/avatar/27/x.jpg"); got != "/api/user/static/avatar/27/x.jpg" {
		t.Errorf("local path must pass through: %s", got)
	}
	if got := normalizeAvatarForStore("https://example.com/a.png"); got != "https://example.com/a.png" {
		t.Errorf("external url must pass through: %s", got)
	}
}

func TestExpandAvatarBase(t *testing.T) {
	base := "https://cdn.example.com"
	if got := expandAvatarBase(base, "/avatar/27/a1b2.jpg"); got != "https://cdn.example.com/avatar/27/a1b2.jpg" {
		t.Errorf("expand: %s", got)
	}
	if got := expandAvatarBase(base, "/api/user/static/avatar/27/x.jpg"); got != "/api/user/static/avatar/27/x.jpg" {
		t.Errorf("local path pass-through: %s", got)
	}
	if got := expandAvatarBase("", "/avatar/27/a1b2.jpg"); got != "/avatar/27/a1b2.jpg" {
		t.Errorf("empty base: %s", got)
	}
}
