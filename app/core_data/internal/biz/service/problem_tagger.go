package service

import (
	"context"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/data/model"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/redis/go-redis/v9"
)

// ProblemTagger 使用官方 openai-go SDK 调用 OpenAI 兼容 Chat Completions。
// 优先读站点设置（Redis），yaml 仅作兜底。
type ProblemTagger struct {
	yaml   *conf.AiAnalyze
	rdb    *redis.Client
	mu     sync.Mutex
	client *openai.Client
	model  string
	secret string
	base   string
}

func NewProblemTagger(c *conf.AiAnalyze, rdb *redis.Client) *ProblemTagger {
	t := &ProblemTagger{yaml: c, rdb: rdb}
	t.reload(context.Background())
	return t
}

func (t *ProblemTagger) conf(ctx context.Context) *conf.AiAnalyze {
	rt := sitesettings.Load(ctx, t.rdb, nil).MergeFallback(nil, nil, t.yaml)
	return rt.AiAnalyzeConf()
}

func (t *ProblemTagger) reload(ctx context.Context) {
	c := t.conf(ctx)
	endpoint := strings.TrimSpace(c.Endpoint)
	secret := strings.TrimSpace(c.Secret)
	modelID := strings.TrimSpace(c.Model)
	base := normalizeOpenAIBaseURL(endpoint)
	t.mu.Lock()
	defer t.mu.Unlock()
	if secret == t.secret && modelID == t.model && base == t.base && t.client != nil {
		return
	}
	t.secret = secret
	t.model = modelID
	t.base = base
	if secret == "" || endpoint == "" {
		t.client = nil
		return
	}
	httpClient := &http.Client{Timeout: 10 * time.Minute}
	cli := openai.NewClient(
		option.WithAPIKey(secret),
		option.WithBaseURL(base),
		option.WithHTTPClient(httpClient),
	)
	t.client = &cli
}

// Ready 是否已配置可用
func (t *ProblemTagger) Ready() bool {
	if t == nil {
		return false
	}
	t.reload(context.Background())
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client != nil && t.model != ""
}

type aiAnalyzeResult struct {
	ProblemType        string               `json:"problem_type"`
	Difficulty         string               `json:"difficulty"`
	AlgorithmTags      []string             `json:"algorithm_tags"`
	SuggestedSolutions []model.SolutionMeta `json:"suggested_solutions"`
	ContentMD          string               `json:"content_md"` // 可选：AI 优化排版后的题面
}

func (t *ProblemTagger) Analyze(ctx context.Context, title, contentMD string) (res *aiAnalyzeResult, err error) {
	defer func() {
		if err != nil {
			sitesettings.SetServiceStatus(ctx, t.rdb, sitesettings.ServiceAiAnaly, sitesettings.StatusFail, err.Error())
		} else if res != nil {
			sitesettings.SetServiceStatus(ctx, t.rdb, sitesettings.ServiceAiAnaly, sitesettings.StatusOK, "")
		}
	}()
	t.reload(ctx)
	t.mu.Lock()
	client := t.client
	modelID := t.model
	t.mu.Unlock()
	if client == nil || modelID == "" {
		return nil, fmt.Errorf("题库 AI 未配置（请在站点设置中填写）")
	}
	// 节约 token：截断超长题面（翻译+排版需要更多上下文）
	content := contentMD
	if len(content) > 18000 {
		content = content[:18000] + "\n...(truncated)"
	}
	system := `你是算法题目标签分析器与题面「全文中文译者」。先完整翻译/中文化题面，再做轻量标签分析。不要长篇推理。
仅输出 JSON，不要 markdown 代码块，不要解释过程。

【绝对最高优先级：全文中文】
- 所有字符串字段的可见文字必须是中文，禁止把英文段落原样拷进 content_md。
- 包括：标题、题意描述、输入格式、输出格式、样例说明/解释、约束/数据范围、注意、提示、交互说明、评分方式等——凡给人看的句子都要译成通顺中文。
- problem_type、difficulty、algorithm_tags、suggested_solutions.name / brief_explanation 也必须中文。
- 英文算法名译中文：DP→动态规划，BFS→广度优先搜索，DFS→深度优先搜索，Dijkstra→最短路，Binary Search→二分查找，Two Pointers→双指针 等。
- 复杂度 time_complexity / space_complexity 可用 O(n)、O(n log n) 等写法。
- 禁止偷懒：不得只译标题、不得只改章节名而正文仍是英文、不得用「见原文」代替翻译。

【题面 content_md — 必须输出且必须为完整中文 Markdown，禁止空字符串】
1. 无论原题是英文、中英混杂还是其他语言：都要「完全翻译」为中文题面；专有名词（人名、公司名、平台名）可首次保留原文并附中文，其后用中文。
2. 若原题已是中文：优化排版与分段，修正乱码/粘连，不无故改题意；仍保证章节齐全、表述通顺。
3. 统一结构（标题用中文）：
   # 标题（中文）
   ## 题意
   ## 输入
   ## 输出
   ## 约束（或 数据范围）
   ## 样例（多个时 ### 样例 1 / 样例 2）
   ## 说明（样例解释也必须中文）
   ## 提示（若有）
4. 样例「输入/输出数据」用 fenced code block 原样保留（数字、换行、格式不改）；样例外的说明文字必须中文。
5. 数学公式用 $...$ 或 $$...$$（KaTeX）；禁止 $$$；变量名可保留英文字母。
6. 保留全部条件、约束与边界，不得删减；翻译后信息量不得少于原题。

字段：
- problem_type: 中文模块名（图论、动态规划、数据结构、数学、字符串、贪心等）
- difficulty: 只能是 简单 / 中等 / 困难
- algorithm_tags: 中文算法标签数组（2~6 个）
- suggested_solutions: 1~2 个，含 name, time_complexity, space_complexity, brief_explanation（中文，各一两句）
- content_md: 完整中文 Markdown 题面（必填，全文中文）
禁止分析用户代码；不要输出除 JSON 外的任何文字。`
	user := fmt.Sprintf("请将下列题目完整翻译/整理为中文题面，并完成标签分析。\n标题: %s\n\n原题面:\n%s", title, content)

	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel(modelID),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		Temperature: openai.Float(0.2),
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: ptrJSONObject(),
		},
	}

	contentStr, err := streamChat(ctx, client, params)
	if err != nil {
		// 部分兼容网关不支持 response_format，降级重试
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{}
		contentStr, err = streamChat(ctx, client, params)
		if err != nil {
			return nil, fmt.Errorf("openai chat completion stream: %w", err)
		}
	}
	contentStr = stripJSONFence(strings.TrimSpace(contentStr))
	var result aiAnalyzeResult
	if err := json.Unmarshal([]byte(contentStr), &result); err != nil {
		return nil, fmt.Errorf("反序列化 AI JSON 失败: %w body=%s", err, truncateStr(contentStr, 400))
	}
	result.Difficulty = normalizeDifficulty(result.Difficulty)
	result.AlgorithmTags = normalizeChineseTags(result.AlgorithmTags)
	result.ProblemType = strings.TrimSpace(result.ProblemType)
	// content_md 必填意图：若模型返回空则保留爬取原文（由调用方决定是否覆盖）
	result.ContentMD = strings.TrimSpace(result.ContentMD)
	// 清理 $$$ 为 $
	if result.ContentMD != "" {
		result.ContentMD = strings.ReplaceAll(result.ContentMD, "$$$", "$")
	}
	return &result, nil
}

// streamChat 流式拉取完整 assistant content，避免网关 ~60s 非流式切断
func streamChat(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams) (string, error) {
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

// normalizeChineseTags 去掉空白、过短、明显纯英文标签
func normalizeChineseTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || len([]rune(t)) < 2 {
			continue
		}
		if seen[t] {
			continue
		}
		// 纯 ASCII 英文短语大概率是漏译，丢弃
		if isASCIIWord(t) {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

func isASCIIWord(s string) bool {
	hasLetter := false
	for _, r := range s {
		if r > 127 {
			return false
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
		} else if r == ' ' || r == '-' || r == '_' {
			continue
		} else if r >= '0' && r <= '9' {
			continue
		} else {
			return false
		}
	}
	return hasLetter
}

// normalizeOpenAIBaseURL 将配置 endpoint 规范为 openai-go 的 BaseURL（需含 /v1/ 前缀路径）。
// SDK 会再拼 chat/completions → 最终 .../v1/chat/completions
//
// 支持:
//   - https://api.openai.com/v1
//   - http://host:8001/api        → http://host:8001/api/v1/
//   - http://host/v1/chat/completions → http://host/v1/
func normalizeOpenAIBaseURL(endpoint string) string {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if strings.HasSuffix(ep, "/chat/completions") {
		ep = strings.TrimSuffix(ep, "/chat/completions")
		ep = strings.TrimRight(ep, "/")
	}
	if !strings.HasSuffix(ep, "/v1") {
		if strings.HasSuffix(ep, "/api") {
			ep = ep + "/v1"
		} else {
			ep = ep + "/v1"
		}
	}
	return ep + "/"
}

func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```JSON")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

func normalizeDifficulty(d string) string {
	d = strings.TrimSpace(d)
	switch strings.ToLower(d) {
	case "easy", "简单", "入门":
		return "简单"
	case "medium", "中等", "中级":
		return "中等"
	case "hard", "困难", "高级":
		return "困难"
	default:
		if d == "" {
			return "中等"
		}
		return d
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func ptrJSONObject() *shared.ResponseFormatJSONObjectParam {
	p := shared.NewResponseFormatJSONObjectParam()
	return &p
}
