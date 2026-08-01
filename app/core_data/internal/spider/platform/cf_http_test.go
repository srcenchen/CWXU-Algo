package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCfAPIBasesDefaultAndFallback(t *testing.T) {
	t.Setenv("CWXU_CF_API_BASE", "")
	t.Setenv("CWXU_CF_API_FALLBACKS", "")
	bases := cfAPIBases()
	if len(bases) != 1 || bases[0] != "https://codeforces.com" {
		t.Fatalf("default bases=%v", bases)
	}

	t.Setenv("CWXU_CF_API_BASE", "https://relay.example/cf/")
	t.Setenv("CWXU_CF_API_FALLBACKS", "https://codeforces.com, https://relay.example/cf, https://other")
	bases = cfAPIBases()
	if bases[0] != "https://relay.example/cf" {
		t.Fatalf("primary trim: %v", bases)
	}
	if len(bases) != 3 {
		t.Fatalf("dedupe fallbacks want 3 got %v", bases)
	}
}

func TestCfProxyURLPrefersCWXU(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy:1")
	t.Setenv("CWXU_CF_HTTP_PROXY", "socks5://127.0.0.1:7890")
	u := cfProxyURL()
	if u == nil || u.Scheme != "socks5" || u.Host != "127.0.0.1:7890" {
		t.Fatalf("got %#v", u)
	}
}

func TestCfGetJSONSuccessAndFallback(t *testing.T) {
	cfResetClient()
	primaryHits := 0
	okHits := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-home"))
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		okHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"OK","result":[]}`))
	}))
	defer good.Close()

	t.Setenv("CWXU_CF_API_BASE", bad.URL)
	t.Setenv("CWXU_CF_API_FALLBACKS", good.URL)
	t.Setenv("CWXU_CF_HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	cfResetClient()

	body, used, err := cfGetJSON(context.Background(), "/api/user.status?handle=x&from=1&count=1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if used != good.URL {
		t.Fatalf("used base %s want %s", used, good.URL)
	}
	if !strings.Contains(string(body), `"OK"`) {
		t.Fatalf("body=%s", body)
	}
	if okHits < 1 {
		t.Fatalf("good server not hit")
	}
	// primary 应至少试过（403 重试）
	if primaryHits < 1 {
		t.Fatalf("primary not tried")
	}
}

func TestCfGetJSONLiveOptional(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	cfResetClient()
	t.Setenv("CWXU_CF_API_BASE", "https://codeforces.com")
	t.Setenv("CWXU_CF_API_FALLBACKS", "")
	// 不强制清代理：本机若有系统/TUN 代理可借力
	body, _, err := cfGetJSON(context.Background(), "/api/user.status?handle=tourist&from=1&count=1")
	if err != nil {
		t.Skipf("live CF unreachable from this env: %v", err)
	}
	if !strings.Contains(string(body), `"status"`) {
		t.Fatalf("unexpected body %s", body[:min(80, len(body))])
	}
}
