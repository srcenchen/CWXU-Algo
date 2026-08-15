package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/common/openaiclient"
	"cwxu-algo/app/common/sitesettings"

	"github.com/openai/openai-go/v3"
	"github.com/redis/go-redis/v9"
)

// llmCallTimeout 单轮 LLM 调用上限：模型/网络挂起时不至于占死 worker
const llmCallTimeout = 90 * time.Second

// Message 业务层与 transport 之间的中立纯文本消息。
type Message struct {
	Role    string
	Content string
}

type Chat struct {
	rdb      *redis.Client
	mu       sync.Mutex
	client   *openai.Client
	model    string
	endpoint string
	secret   string
}

func NewChat(rdb *redis.Client) *Chat {
	c := &Chat{rdb: rdb}
	c.reload(context.Background())
	return c
}

func (c *Chat) reload(ctx context.Context) {
	rt := sitesettings.Load(ctx, c.rdb, nil)
	c.reloadWithConfig(ctx, rt.AgentEndpoint, rt.AgentModel, rt.AgentSecret)
}

func (c *Chat) reloadWithConfig(ctx context.Context, endpoint, modelID, secret string) {
	endpoint = strings.TrimSpace(endpoint)
	modelID = strings.TrimSpace(modelID)
	secret = strings.TrimSpace(secret)
	c.mu.Lock()
	defer c.mu.Unlock()
	if endpoint == c.endpoint && modelID == c.model && secret == c.secret && c.client != nil {
		return
	}
	c.endpoint = endpoint
	c.model = modelID
	c.secret = secret
	if endpoint == "" || modelID == "" || secret == "" {
		c.client = nil
		return
	}
	c.client = openaiclient.NewClient(secret, openaiclient.NormalizeBaseURL(endpoint), llmCallTimeout)
}

func (c *Chat) ensureConfig(ctx context.Context) (string, error) {
	c.reload(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || c.model == "" {
		return "", errors.New("日报周报模型未配置（请在站点设置中填写服务地址、模型与密钥）")
	}
	return c.model, nil
}

// Complete 纯文本补全（OpenAI-compatible 流式），用于预取数据后的文案生成。
func (c *Chat) Complete(ctx context.Context, msgs []Message) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.chat(ctx, msgs)
}

func (c *Chat) chat(ctx context.Context, msgs []Message) (resp string, err error) {
	defer func() {
		if err != nil {
			sitesettings.SetServiceStatus(ctx, c.rdb, sitesettings.ServiceAgent, sitesettings.StatusFail, err.Error())
		} else if strings.TrimSpace(resp) != "" {
			sitesettings.SetServiceStatus(ctx, c.rdb, sitesettings.ServiceAgent, sitesettings.StatusOK, "")
		}
	}()
	modelID, err := c.ensureConfig(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()

	openaiMsgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system":
			openaiMsgs = append(openaiMsgs, openai.SystemMessage(m.Content))
		case "user":
			openaiMsgs = append(openaiMsgs, openai.UserMessage(m.Content))
		case "assistant":
			openaiMsgs = append(openaiMsgs, openai.AssistantMessage(m.Content))
		}
	}
	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(modelID),
		Messages: openaiMsgs,
	}
	callCtx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	content, err := openaiclient.StreamCompletion(callCtx, client, params)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("模型返回空")
	}
	return content, nil
}
