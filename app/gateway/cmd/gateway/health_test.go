package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	configv1 "github.com/go-kratos/gateway/api/gateway/config/v1"
)

func TestWithLocalHealthDoesNotProxyLiveness(t *testing.T) {
	proxied := false
	handler := withLocalHealth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		proxied = true
	}), nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || proxied {
			t.Fatalf("%s: code=%d proxied=%v", path, recorder.Code, proxied)
		}
	}
}

func TestReadyzGateOnUnresolvedEndpoints(t *testing.T) {
	handler := withLocalHealth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), []string{"user", "core-data", "agent"})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: want 503, got %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", recorder.Code)
	}
}

func TestDiscoveryEndpointsCollectsBackends(t *testing.T) {
	cfg := &configv1.Gateway{Endpoints: []*configv1.Endpoint{
		{Path: "/v1/user/*", Backends: []*configv1.Backend{
			{Target: "discovery:///user"},
			{Target: "discovery:///user"},
		}},
		{Path: "/v1/core/*", Backends: []*configv1.Backend{
			{Target: "discovery:///core-data"},
		}},
		{Path: "/v1/agent/*", Backends: []*configv1.Backend{
			{Target: "discovery:///agent"},
		}},
		{Path: "/v1/direct/*", Backends: []*configv1.Backend{
			{Target: "127.0.0.1:8001"},
		}},
	}}
	got := discoveryEndpoints(cfg)
	want := []string{"user", "core-data", "agent"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}
