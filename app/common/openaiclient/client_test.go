package openaiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

func TestLLMCallTimeoutIsTenMinutes(t *testing.T) {
	if LLMCallTimeout != 10*time.Minute {
		t.Fatalf("LLMCallTimeout = %s, want 10m", LLMCallTimeout)
	}
}

func TestStreamCompletionWithCallbacksExposesReasoningAndContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"先分析\"},\"finish_reason\":\"\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{\\\"ok\\\":true}\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient("test", NormalizeBaseURL(srv.URL), time.Second)
	params := openai.ChatCompletionNewParams{Model: shared.ChatModel("test")}
	var reasoning, content string
	result, err := StreamCompletionWithCallbacks(context.Background(), client, params, func(chunk string) {
		reasoning += chunk
	}, func(chunk string) {
		content += chunk
	})
	if err != nil {
		t.Fatal(err)
	}
	if reasoning != "先分析" || content != `{"ok":true}` || result != content {
		t.Fatalf("reasoning=%q content=%q result=%q", reasoning, content, result)
	}
}

func TestReasoningDelta(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "reasoning content", raw: `{"reasoning_content":"先读题"}`, want: "先读题"},
		{name: "reasoning string", raw: `{"reasoning":"再判断标签"}`, want: "再判断标签"},
		{name: "reasoning text", raw: `{"reasoning_text":"最后整理"}`, want: "最后整理"},
		{name: "reasoning object text", raw: `{"reasoning":{"text":"对象推理"}}`, want: "对象推理"},
		{name: "reasoning array", raw: `{"reasoning":[{"text":"第一段"},{"content":"第二段"}]}`, want: "第一段第二段"},
		{name: "unknown", raw: `{"content":"answer"}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reasoningDelta(tt.raw); got != tt.want {
				t.Fatalf("reasoningDelta() = %q, want %q", got, tt.want)
			}
		})
	}
}
