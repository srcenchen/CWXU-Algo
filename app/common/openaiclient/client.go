package openaiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// LLMCallTimeout is the shared upper bound for one backend model operation.
const LLMCallTimeout = 10 * time.Minute

// NormalizeBaseURL 将配置 endpoint 规范为 openai-go 的 BaseURL（需含 /v1/ 前缀路径）。
// SDK 会再拼 chat/completions → 最终 .../v1/chat/completions
//
// 支持：
//   - https://api.openai.com/v1
//   - http://host:8001/api        → http://host:8001/api/v1/
//   - http://host/v1/chat/completions → http://host/v1/
//   - https://gateway.example.com/custom → https://gateway.example.com/custom/v1/
func NormalizeBaseURL(endpoint string) string {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(ep, "/chat/completions") {
		ep = strings.TrimRight(strings.TrimSuffix(ep, "/chat/completions"), "/")
	}
	if !strings.HasSuffix(ep, "/v1") {
		ep = ep + "/v1"
	}
	return ep + "/"
}

// NewClient 创建 OpenAI-compatible 客户端（带超时与自定义 BaseURL）。
func NewClient(secret, base string, timeout time.Duration) *openai.Client {
	httpClient := &http.Client{Timeout: timeout}
	cli := openai.NewClient(
		option.WithAPIKey(secret),
		option.WithBaseURL(base),
		option.WithHTTPClient(httpClient),
	)
	return &cli
}

// StreamCompletion 流式拉取完整 assistant content，避免网关 ~60s 非流式切断。
func StreamCompletion(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams) (string, error) {
	return StreamCompletionWithCallback(ctx, client, params, nil)
}

func StreamCompletionWithCallback(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams, onChunk func(string)) (string, error) {
	return StreamCompletionWithCallbacks(ctx, client, params, nil, onChunk)
}

// StreamCompletionWithCallbacks exposes compatible providers' raw reasoning
// extension separately from the final assistant content.
func StreamCompletionWithCallbacks(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams, onReasoning, onContent func(string)) (string, error) {
	if client == nil {
		return "", fmt.Errorf("openai client 未配置")
	}
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		if len(stream.Current().Choices) > 0 {
			delta := stream.Current().Choices[0].Delta
			if onReasoning != nil {
				if reasoning := reasoningDelta(delta.RawJSON()); reasoning != "" {
					onReasoning(reasoning)
				}
			}
			if onContent != nil && delta.Content != "" {
				onContent(delta.Content)
			}
		}
		if !acc.AddChunk(stream.Current()) {
			return "", fmt.Errorf("AI stream chunk 累积失败")
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	if len(acc.Choices) == 0 {
		return "", fmt.Errorf("AI 返回空 choices")
	}
	return acc.Choices[0].Message.Content, nil
}

func reasoningDelta(raw string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return ""
	}
	for _, key := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
		if value, ok := fields[key]; ok {
			if text := reasoningText(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func reasoningText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for _, key := range []string{"text", "content"} {
			if value, ok := object[key]; ok {
				if part := reasoningText(value); part != "" {
					return part
				}
			}
		}
		return ""
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		var out strings.Builder
		for _, item := range items {
			out.WriteString(reasoningText(item))
		}
		return out.String()
	}
	return ""
}
