package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cwxu-algo/api/core/v1/health"
	"cwxu-algo/app/common/conf"
)

func TestCollectBackendServicesUsesConsulPassingInstances(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("passing") != "true" {
			t.Fatalf("unexpected consul request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/health/service/user" {
			_, _ = w.Write([]byte(`[{"Service":{"Service":"user"},"Checks":[{"Status":"passing"}]}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	svc := &HealthService{server: &conf.Server{RegDsn: server.URL}}
	items := svc.collectBackendServices(context.Background())

	assertBackendStatus(t, items, "core-data", "ok")
	assertBackendStatus(t, items, "user", "ok")
	assertBackendStatus(t, items, "agent", "unchecked")
	if findBackendService(items, "gateway") != nil {
		t.Fatal("gateway is not registered in Consul and must not be listed")
	}
}

func TestCollectBackendServicesReportsConsulErrorsWithoutBlocking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	svc := &HealthService{
		server:              &conf.Server{RegDsn: server.URL},
		backendProbeTimeout: 20 * time.Millisecond,
	}
	started := time.Now()
	items := svc.collectBackendServices(context.Background())
	if time.Since(started) > 60*time.Millisecond {
		t.Fatal("backend service probes ran serially instead of sharing a deadline")
	}
	assertBackendStatus(t, items, "core-data", "ok")
	item := findBackendService(items, "user")
	if item == nil || item.Status != "unchecked" || item.ErrMsg == "" {
		t.Fatalf("user status = %#v, want unchecked with error", item)
	}
}

func assertBackendStatus(t *testing.T, items []*health.HealthBackendServiceItem, name, want string) {
	t.Helper()
	item := findBackendService(items, name)
	if item == nil || item.Status != want {
		t.Fatalf("%s status = %#v, want %s", name, item, want)
	}
}

func findBackendService(items []*health.HealthBackendServiceItem, name string) *health.HealthBackendServiceItem {
	for _, item := range items {
		if item.Name == name {
			return item
		}
	}
	return nil
}
