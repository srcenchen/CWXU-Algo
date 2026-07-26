package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/agent/internal/agent/tool"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/sitesettings"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
)

const defaultMaxRounds = 8

// llmCallTimeout 单轮 LLM 调用上限：模型/网络挂起时不至于占死 worker
const llmCallTimeout = 90 * time.Second

type Chat struct {
	yaml   *conf.Agent
	rdb    *redis.Client
	mu     sync.Mutex
	client *arkruntime.Client
	model  string
	secret string
}

func NewChat(yaml *conf.Agent, rdb *redis.Client) *Chat {
	c := &Chat{yaml: yaml, rdb: rdb}
	c.reload(context.Background())
	return c
}

func (c *Chat) runtime(ctx context.Context) *sitesettings.Runtime {
	rt := sitesettings.Load(ctx, c.rdb, nil)
	return rt.MergeFallback(nil, c.yaml, nil)
}

func (c *Chat) reload(ctx context.Context) {
	rt := c.runtime(ctx)
	modelID := strings.TrimSpace(rt.AgentModel)
	secret := strings.TrimSpace(rt.AgentSecret)
	c.mu.Lock()
	defer c.mu.Unlock()
	if secret == c.secret && modelID == c.model && c.client != nil {
		return
	}
	c.model = modelID
	c.secret = secret
	if secret == "" {
		c.client = nil
		return
	}
	c.client = arkruntime.NewClientWithApiKey(secret)
}

func (c *Chat) ensureClient(ctx context.Context) (*arkruntime.Client, string, error) {
	c.reload(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || c.model == "" {
		return nil, "", errors.New("AI 总结模型未配置（请在站点设置中填写）")
	}
	return c.client, c.model, nil
}

// Complete 纯文本补全（不携带工具），用于预取数据后的文案生成。
func (c *Chat) Complete(ctx context.Context, messages []*model.ChatCompletionMessage) (string, error) {
	return c.Chat(ctx, messages)
}

// Chat 支持可选工具调用。maxRounds 防止无限循环；未知工具返回错误文本而非 panic。
func (c *Chat) Chat(ctx context.Context, messages []*model.ChatCompletionMessage, tools ...tool.AgentToolFactory) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client, modelID, err := c.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	finalResp := ""
	reg := map[string]tool.AgentToolFactory{}
	toolUse := make([]*model.Tool, 0, len(tools))
	for _, t := range tools {
		desc := t.Description()
		if desc == nil || desc.Function == nil || desc.Function.Name == "" {
			continue
		}
		reg[desc.Function.Name] = t
		toolUse = append(toolUse, desc)
	}

	for round := 0; round < defaultMaxRounds; round++ {
		req := model.CreateChatCompletionRequest{
			Model:    modelID,
			Messages: messages,
			Tools:    toolUse,
		}
		// 每轮调用带独立超时
		callCtx, cancel := context.WithTimeout(ctx, llmCallTimeout)
		resp, err := client.CreateChatCompletion(callCtx, &req)
		cancel()
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("模型返回空")
		}
		choice := resp.Choices[0]
		if choice.Message.Content != nil && choice.Message.Content.StringValue != nil {
			finalResp = *choice.Message.Content.StringValue
		}
		if choice.Message.ReasoningContent != nil {
			log.Debugf("reasoning: %s", truncate(*choice.Message.ReasoningContent, 500))
		}
		if choice.FinishReason != model.FinishReasonToolCalls || len(choice.Message.ToolCalls) == 0 {
			return finalResp, nil
		}
		messages = append(messages, &choice.Message)
		for _, toolCall := range choice.Message.ToolCalls {
			name := toolCall.Function.Name
			args := toolCall.Function.Arguments
			log.Infof("执行工具 %s %s", name, args)
			toolMsg := ""
			if t, ok := reg[name]; ok {
				toolMsg = t.AiInterface(args)
			} else {
				toolMsg = fmt.Sprintf("工具不存在: %s", name)
				log.Warnf("未知工具调用: %s", name)
			}
			toolMsg = sanitizeToolResult(toolMsg)
			log.Infof("工具结果 %s: %s", name, truncate(toolMsg, 500))
			messages = append(messages, &model.ChatCompletionMessage{
				Role:       model.ChatMessageRoleTool,
				Content:    &model.ChatCompletionMessageContent{StringValue: volcengine.String(toolMsg)},
				ToolCallID: toolCall.ID,
			})
		}
	}
	return finalResp, fmt.Errorf("工具调用超过最大轮次 %d", defaultMaxRounds)
}

// truncate 按 rune 截断，避免在多字节字符中间截出乱码
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// sanitizeToolResult 工具失败时避免模型把错误写进用户可见报告
func sanitizeToolResult(msg string) string {
	m := strings.TrimSpace(msg)
	if m == "" {
		return "无数据。请仅使用用户消息中的预置 JSON，不要在最终 HTML 中提及工具。"
	}
	lower := strings.ToLower(m)
	failHints := []string{"服务不可用", "连接失败", "registry", "查询失败", "内部服务器错误", "unavailable", "timeout", "deadline"}
	for _, h := range failHints {
		if strings.Contains(lower, strings.ToLower(h)) || strings.Contains(m, h) {
			return "工具暂不可用，忽略本工具结果。请仅使用用户消息预置 JSON 生成完整 HTML，不要在输出中提及工具或错误。"
		}
	}
	// 控制工具回传体积，防止挤掉最终 HTML（按 rune 截断，避免截出乱码）
	if r := []rune(m); len(r) > 6000 {
		return string(r[:6000]) + "\n...(truncated) 请优先使用预置 JSON。"
	}
	return m
}
