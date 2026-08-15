package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func miniredisRedis(t *testing.T, mr *miniredis.Miniredis) *redis.Client {
	t.Helper()
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// seedRuntime 将 Agent 配置写入 miniredis（等价 user 服务 PublishRedis）。
func seedRuntime(t *testing.T, rdb *redis.Client, endpoint, model, secret string) {
	t.Helper()
	rt := map[string]interface{}{
		"siteTitle":     "GoAlgo",
		"agentEndpoint": endpoint,
		"agentModel":    model,
		"agentSecret":   secret,
	}
	b, err := json.Marshal(rt)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), "site:runtime_config:v1", b, 0).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestChatCompleteStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"},"finish_reason":null}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"世界"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			w.Write([]byte(c + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	c := NewChat(rdb)
	seedRuntime(t, rdb, srv.URL+"/v1", "test-model", "test-secret")
	out, err := c.Complete(context.Background(), []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "user"},
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if out != "你好世界" {
		t.Fatalf("output = %q, want %q", out, "你好世界")
	}
}

func TestChatCompleteEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	c := NewChat(miniredisRedis(t, mr))
	seedRuntime(t, miniredisRedis(t, mr), srv.URL+"/v1", "m", "s")
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestChatNotConfigured(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	c := NewChat(miniredisRedis(t, mr))
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected not-configured error")
	}
}
