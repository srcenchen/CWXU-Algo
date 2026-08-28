package cors

import (
	"net/http"
	"strings"
	"testing"

	config "github.com/go-kratos/gateway/api/gateway/config/v1"
	v1 "github.com/go-kratos/gateway/api/gateway/middleware/cors/v1"
	"github.com/go-kratos/gateway/middleware"
	"google.golang.org/protobuf/types/known/anypb"
)

func buildConfig(origins []string) *config.Middleware {
	v, err := anypb.New(&v1.Cors{
		AllowOrigins: origins,
	})
	if err != nil {
		panic(err)
	}
	return &config.Middleware{Options: v}
}

func TestCors(t *testing.T) {
	tests := []struct {
		Config     *config.Middleware
		Origin     string
		Method     string
		StatusCode int
	}{
		{
			Config:     &config.Middleware{},
			Method:     "POST",
			StatusCode: 200,
		},
		{
			Config:     &config.Middleware{},
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"google.com"}),
			Origin:     "https://youtube.com",
			Method:     "OPTIONS",
			StatusCode: 403,
		},
		{
			Config:     buildConfig([]string{"*.google.com"}),
			Origin:     "https://www.youtube.com",
			Method:     "OPTIONS",
			StatusCode: 403,
		},
		{
			Config:     buildConfig([]string{"*.google.com"}),
			Origin:     "https://www.google.com",
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"google.com"}),
			Origin:     "https://www.google.com",
			Method:     "OPTIONS",
			StatusCode: 403,
		},
		{
			Config:     buildConfig([]string{"google.com"}),
			Origin:     "https://google.com",
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"google.com"}),
			Origin:     "http://google.com",
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"GOOGLE.COM"}),
			Origin:     "http://google.com",
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"*.GOOGLE.COM"}),
			Origin:     "http://www.google.com",
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"*"}),
			Origin:     "http://google.com",
			Method:     "OPTIONS",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"google.com"}),
			Origin:     "http://google.com",
			Method:     "GET",
			StatusCode: 200,
		},
		{
			Config:     buildConfig([]string{"*.youtube.com"}),
			Origin:     "http://google.com",
			Method:     "GET",
			StatusCode: 403,
		},
	}
	for no, test := range tests {
		m, err := Middleware(test.Config)
		if err != nil {
			t.Fatal(err)
		}
		do := m(middleware.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return newResponse(200, make(http.Header))
		}))
		{
			req, err := http.NewRequest(test.Method, "/foo", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(corsOriginHeader, test.Origin)
			resp, err := do.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != test.StatusCode {
				t.Fatalf("%d want %d but got %d", no, test.StatusCode, resp.StatusCode)
			}
			if resp.StatusCode != 200 {
				continue
			}
			if test.Origin == "" {
				continue
			}
			if test.Method == "OPTIONS" {
				// preflightHeaders
				if v := resp.Header.Get(corsVaryHeader); v != corsOriginHeader {
					t.Fatalf("%d want %s but got %s", no, corsOriginHeader, v)
				}
				if v := resp.Header.Get(corsAllowCredentialsHeader); v != "true" {
					t.Fatalf("%d want %s but got %s", no, "true", v)
				}
				if v := resp.Header.Get(corsAllowMethodsHeader); v != strings.Join(defaultCorsMethods, ",") {
					t.Fatalf("%d want %s but got %s", no, defaultCorsMethods, v)
				}
				if v := resp.Header.Get(corsAllowHeadersHeader); v != strings.Join(defaultCorsHeaders, ",") {
					t.Fatalf("%d want %s but got %s", no, defaultCorsHeaders, v)
				}
				if v := resp.Header.Get(corsMaxAgeHeader); v != "600" {
					t.Fatalf("%d want %s but got %s", no, "600", v)
				}
			} else {
				// normalHeaders
				if v := resp.Header.Get(corsVaryHeader); v != corsOriginHeader {
					t.Fatalf("%d want %s but got %s", no, corsOriginHeader, v)
				}
				if v := resp.Header.Get(corsAllowCredentialsHeader); v != "true" {
					t.Fatalf("%d want %s but got %s", no, "true", v)
				}
				if v := resp.Header.Get(corsAllowMethodsHeader); v != strings.Join(defaultCorsMethods, ",") {
					t.Fatalf("%d want %s but got %s", no, defaultCorsMethods, v)
				}
			}
		}
	}
}

func TestIsOriginAllowedMatchesConfiguredOriginSchemeAndHostExactly(t *testing.T) {
	const extensionOrigin = "chrome-extension://phbnnpidffgnnajfdmgglbphjkbindkd"

	for _, test := range []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "configured extension origin", origin: extensionOrigin, want: true},
		{name: "same host with http scheme", origin: "http://phbnnpidffgnnajfdmgglbphjkbindkd", want: false},
		{name: "same host with https scheme", origin: "https://phbnnpidffgnnajfdmgglbphjkbindkd", want: false},
		{name: "same scheme and host with path", origin: extensionOrigin + "/not-an-origin", want: false},
		{name: "same scheme and host with query", origin: extensionOrigin + "?x=1", want: false},
		{name: "same scheme and host with userinfo", origin: "chrome-extension://user@phbnnpidffgnnajfdmgglbphjkbindkd", want: false},
		{name: "same scheme and host with fragment", origin: extensionOrigin + "#fragment", want: false},
		{name: "another extension", origin: "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isOriginAllowed(test.origin, []string{extensionOrigin}); got != test.want {
				t.Fatalf("isOriginAllowed(%q) = %t, want %t", test.origin, got, test.want)
			}
		})
	}
}

func TestCorsAllowsLuoguSyncExtensionRequestHeaders(t *testing.T) {
	config := buildConfig([]string{"chrome-extension://phbnnpidffgnnajfdmgglbphjkbindkd"})
	options := &v1.Cors{
		AllowOrigins: []string{"chrome-extension://phbnnpidffgnnajfdmgglbphjkbindkd"},
		AllowHeaders: []string{"Content-Type", "X-GoAlgo-Plugin-Token", "X-GoAlgo-Sync-Session"},
	}
	encoded, err := anypb.New(options)
	if err != nil {
		t.Fatal(err)
	}
	config.Options = encoded

	m, err := Middleware(config)
	if err != nil {
		t.Fatal(err)
	}
	do := m(middleware.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newResponse(http.StatusNoContent, make(http.Header))
	}))
	req, err := http.NewRequest(http.MethodOptions, "/v1/core/spider/luogu-sync/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(corsOriginHeader, "chrome-extension://phbnnpidffgnnajfdmgglbphjkbindkd")
	req.Header.Set(corsRequestHeadersHeader, "content-type,x-goalgo-plugin-token,x-goalgo-sync-session")
	resp, err := do.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got, want := resp.Header.Get(corsAllowHeadersHeader), "Content-Type,X-Goalgo-Plugin-Token,X-Goalgo-Sync-Session"; got != want {
		t.Fatalf("Access-Control-Allow-Headers = %q, want exactly %q", got, want)
	}
}
