package blogimg

import "testing"

func TestAvatarObjectKeyFromAnyURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/avatar/27/a1b2.jpg", "/avatar/27/a1b2.jpg"},
		{"https://zhiyuansofts.cn/avatar/27/a1b2.jpg", "/avatar/27/a1b2.jpg"},
		{"http://cdn.example.com/avatar/27/a1b2.webp", "/avatar/27/a1b2.webp"},
		{"//cdn.example.com/avatar/27/a1b2.png", "/avatar/27/a1b2.png"},
		{"", ""},
		{"/api/user/static/avatar/27/20260730_ab.jpg", ""},
		{"/v1/user/static/avatar/27/20260730_ab.jpg", ""},
		{"/blog/27/a1b2.jpg", ""},
		{"/avatar/x/a1b2.jpg", ""},
		{"https://example.com/foo/avatar/27/a1b2.jpg", ""},
	}
	for _, c := range cases {
		if got := AvatarObjectKeyFromAnyURL(c.in); got != c.want {
			t.Errorf("AvatarObjectKeyFromAnyURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAvatarObjectKeyForHash(t *testing.T) {
	h := repeat('a', 64)
	key := AvatarObjectKeyForHash(27, h, ".jpg")
	if key != "/avatar/27/"+h+".jpg" {
		t.Errorf("unexpected key: %s", key)
	}
	if AvatarObjectKeyForHash(0, h, ".jpg") != "" {
		t.Error("userID 0 should return empty")
	}
	if AvatarObjectKeyForHash(27, "short", ".jpg") != "" {
		t.Error("invalid hash should return empty")
	}
}

func TestNormalizeAvatarForStore(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://zhiyuansofts.cn/avatar/27/a1b2.jpg", "/avatar/27/a1b2.jpg"},
		{"/avatar/27/a1b2.jpg", "/avatar/27/a1b2.jpg"},
		{"/api/user/static/avatar/27/20260730_ab.jpg", "/api/user/static/avatar/27/20260730_ab.jpg"},
		{"https://example.com/custom.png", "https://example.com/custom.png"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeAvatarForStore(c.in); got != c.want {
			t.Errorf("NormalizeAvatarForStore(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandAvatarURL(t *testing.T) {
	if got := ExpandAvatarURL("/avatar/27/a1b2.jpg", "https://cdn.example.com"); got != "https://cdn.example.com/avatar/27/a1b2.jpg" {
		t.Errorf("expand path key: %s", got)
	}
	if got := ExpandAvatarURL("https://old.host/avatar/27/a1b2.jpg", "https://cdn.example.com"); got != "https://cdn.example.com/avatar/27/a1b2.jpg" {
		t.Errorf("expand any-host url: %s", got)
	}
	if got := ExpandAvatarURL("/api/user/static/avatar/27/x.jpg", "https://cdn.example.com"); got != "/api/user/static/avatar/27/x.jpg" {
		t.Errorf("local path must pass through: %s", got)
	}
	if got := ExpandAvatarURL("/avatar/27/a1b2.jpg", ""); got != "/avatar/27/a1b2.jpg" {
		t.Errorf("empty base returns key: %s", got)
	}
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
