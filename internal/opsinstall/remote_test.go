package opsinstall

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchAssetPullsRemoteTemplate 验证 install/upgrade 优先从远端（gh-proxy raw）
// 拉取模板。
func TestFetchAssetPullsRemoteTemplate(t *testing.T) {
	const remoteContent = "remote: nginx resolver enabled\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deploy/docker/nginx.conf") {
			_, _ = w.Write([]byte(remoteContent))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Setenv("GOALGO_CONFIG_PROXY", srv.URL)
	t.Setenv("GOALGO_CONFIG_OWNER", "WXUProjects")
	t.Setenv("GOALGO_CONFIG_REPO", "CWXU-Algo")
	t.Setenv("GOALGO_CONFIG_BRANCH", "main")
	t.Setenv("GOALGO_CONFIG_REMOTE", "1")

	got, err := FetchAsset(context.Background(), "docker/nginx.conf")
	if err != nil {
		t.Fatalf("FetchAsset: %v", err)
	}
	if string(got) != remoteContent {
		t.Fatalf("want remote content %q, got %q", remoteContent, got)
	}
}

// TestFetchAssetFallsBackToEmbedded 验证远端不可达（404）时回退内嵌模板。
func TestFetchAssetFallsBackToEmbedded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	t.Setenv("GOALGO_CONFIG_PROXY", srv.URL)
	t.Setenv("GOALGO_CONFIG_REMOTE", "1")

	got, err := FetchAsset(context.Background(), "compose.yaml")
	if err != nil {
		t.Fatalf("FetchAsset: %v", err)
	}
	want, err := ReadAsset("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("fallback content must match embedded asset")
	}
}

// TestRemoteBaseURLUsesGhProxyPrefix 验证代理前缀拼接格式。
func TestRemoteBaseURLUsesGhProxyPrefix(t *testing.T) {
	t.Setenv("GOALGO_CONFIG_PROXY", "https://gh-proxy.com/")
	t.Setenv("GOALGO_CONFIG_OWNER", "WXUProjects")
	t.Setenv("GOALGO_CONFIG_REPO", "CWXU-Algo")
	t.Setenv("GOALGO_CONFIG_BRANCH", "main")
	base := remoteBaseURL()
	want := "https://gh-proxy.com/https://raw.githubusercontent.com/WXUProjects/CWXU-Algo/main/"
	if base != want {
		t.Fatalf("remoteBaseURL = %q, want %q", base, want)
	}
}

// TestRemoteDisabledUsesEmbeddedOnly 验证 GOALGO_CONFIG_REMOTE=0 时只用内嵌模板。
func TestRemoteDisabledUsesEmbeddedOnly(t *testing.T) {
	t.Setenv("GOALGO_CONFIG_REMOTE", "0")
	t.Setenv("GOALGO_CONFIG_PROXY", "http://127.0.0.1:1") // 不可达，禁用时不应请求
	got, err := FetchAsset(context.Background(), "compose.yaml")
	if err != nil {
		t.Fatalf("FetchAsset: %v", err)
	}
	want, err := ReadAsset("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("disabled remote must use embedded asset")
	}
}