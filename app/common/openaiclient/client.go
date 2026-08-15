package openaiclient

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

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
	if client == nil {
		return "", fmt.Errorf("openai client 未配置")
	}
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
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
