package service

import (
	"net/http/httptest"
	"testing"
)

func TestObsidianPublishTokenFailsClosedWithoutEnvironment(t *testing.T) {
	t.Setenv(obsidianPluginPublishTokenEnv, "")
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Plugin-Publish-Token", "goalgo-obsidian-publish-2026")

	if obsidianPublishTokenOK(req) {
		t.Fatal("publish token must fail closed when environment is unset")
	}
}

func TestObsidianPublishTokenAcceptsConfiguredEnvironment(t *testing.T) {
	t.Setenv(obsidianPluginPublishTokenEnv, "configured-secret")
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Plugin-Publish-Token", "configured-secret")
	if !obsidianPublishTokenOK(req) {
		t.Fatal("configured publish token should be accepted")
	}
}

func TestConstantTimeTokenEqualRequiresEqualNonEmptyLength(t *testing.T) {
	if !constantTimeTokenEqual("configured-secret", "configured-secret") {
		t.Fatal("equal tokens should match")
	}
	if constantTimeTokenEqual("", "") || constantTimeTokenEqual("short", "configured-secret") ||
		constantTimeTokenEqual("configured-secreu", "configured-secret") {
		t.Fatal("empty, length-mismatched, or different tokens must not match")
	}
}

func TestValidateObsidianDownloadBase(t *testing.T) {
	t.Setenv(obsidianPluginCDNBaseEnv, "")
	tests := []struct {
		name    string
		version string
		base    string
		want    bool
	}{
		{"trusted exact directory", "1.2.3", "https://zhiyuansofts.cn/obsidian/goalgo-blog/1.2.3", true},
		{"trusted prerelease directory", "1.2.3-beta.1", "https://zhiyuansofts.cn/obsidian/goalgo-blog/1.2.3-beta.1/", true},
		{"arbitrary origin", "1.2.3", "https://evil.example/obsidian/goalgo-blog/1.2.3", false},
		{"http downgrade", "1.2.3", "http://zhiyuansofts.cn/obsidian/goalgo-blog/1.2.3", false},
		{"wrong version directory", "1.2.3", "https://zhiyuansofts.cn/obsidian/goalgo-blog/9.9.9", false},
		{"extra path", "1.2.3", "https://zhiyuansofts.cn/obsidian/goalgo-blog/1.2.3/main.js", false},
		{"userinfo confusion", "1.2.3", "https://zhiyuansofts.cn@evil.example/obsidian/goalgo-blog/1.2.3", false},
		{"query", "1.2.3", "https://zhiyuansofts.cn/obsidian/goalgo-blog/1.2.3?next=evil", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validObsidianDownloadBase(tt.version, tt.base); got != tt.want {
				t.Fatalf("validObsidianDownloadBase(%q, %q)=%v want %v", tt.version, tt.base, got, tt.want)
			}
		})
	}
}

func TestObsidianVersionRequiresStrictSemver(t *testing.T) {
	for _, version := range []string{"1.2", "v1.2.3", "1.2.3/", "1.2.3 evil", "01.2.3"} {
		if validObsidianVersion(version) {
			t.Fatalf("invalid semver accepted: %q", version)
		}
	}
	for _, version := range []string{"0.1.2", "1.2.3-beta.1", "1.2.3+build.7"} {
		if !validObsidianVersion(version) {
			t.Fatalf("valid semver rejected: %q", version)
		}
	}
}
