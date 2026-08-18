package consul

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

func testEntry(address, port int) map[string]any {
	return map[string]any{
		"Node": map[string]any{
			"Node":    "test-node",
			"Address": "127.0.0.1",
		},
		"Service": map[string]any{
			"ID":      fmt.Sprintf("svc-%d", port),
			"Service": "user",
			"Address": "10.0.0.1",
			"Port":    port,
			"Tags":    []string{"version=dev"},
			"TaggedAddresses": map[string]any{
				"http": map[string]any{"Address": fmt.Sprintf("http://10.0.0.1:%d", address), "Port": address},
			},
		},
		"Checks": []map[string]any{{"CheckID": "serfHealth", "Status": "passing"}},
	}
}

// TestWatchSurvivesTransientConsulFailure reproduces the cold-start bug where
// the first resolve hits consul while it is still starting (HTTP 503 /
// connection refused) and every subsequent Watch would be stranded forever.
// The fixed registry must keep retrying and eventually deliver instances.
func TestWatchSurvivesTransientConsulFailure(t *testing.T) {
	var fails int32 = 2
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/health/service/user") {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&fails) > 0 {
			atomic.AddInt32(&fails, -1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = n
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{testEntry(8000, 9000)})
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClient(&api.Config{Address: u.Host})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(client)

	// First watch while consul is "down": poll must retry, not strand the name.
	w1, err := reg.Watch(context.Background(), "user")
	if err != nil {
		t.Fatalf("first Watch should not error on transient consul failure: %v", err)
	}
	defer w1.Stop()

	// Second watch for the same name must return a live watcher too.
	w2, err := reg.Watch(context.Background(), "user")
	if err != nil {
		t.Fatalf("second Watch must succeed: %v", err)
	}
	defer w2.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := 0
	for done < 2 {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for instances after transient failure (calls=%d)", atomic.LoadInt32(&calls))
		default:
		}
		time.Sleep(50 * time.Millisecond)
		svcs, err := w1.Next()
		if err != nil {
			t.Fatalf("w1.Next: %v", err)
		}
		if len(svcs) == 1 {
			done++
		}
		svcs, err = w2.Next()
		if err != nil {
			t.Fatalf("w2.Next: %v", err)
		}
		if len(svcs) == 1 {
			done++
		}
	}
}

// TestGetServiceFallsBackToLiveQuery ensures GetService works before the poll
// has populated the cache.
func TestGetServiceFallsBackToLiveQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{testEntry(8001, 9001)})
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClient(&api.Config{Address: u.Host})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(client)

	svcs, err := reg.GetService(context.Background(), "user")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("want 1 instance, got %d", len(svcs))
	}
	if svcs[0].ID != "svc-9001" {
		t.Fatalf("want svc-9001, got %q", svcs[0].ID)
	}
}