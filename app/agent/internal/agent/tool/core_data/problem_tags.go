package core_data

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cwxu-algo/api/core/v1/problem"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

// ProblemTagsTool 题目标签相关：全站标签表 / 用户标签画像 / 按题目 id 取标签。
// 供日报、周报、训练报告 function calling 使用。
type ProblemTagsTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewProblemTagsTool(reg *registry.Registrar, ctxs ...context.Context) *ProblemTagsTool {
	return &ProblemTagsTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *ProblemTagsTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *ProblemTagsTool) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *ProblemTagsTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "problem_tags",
			Description: "题目标签查询（AI 题库标签）。" +
				"action=list：全站热门标签及题量；" +
				"action=user_profile：某用户各标签 AC 雷达/掌握度；" +
				"action=by_ids：按题库 problemId 批量取标签/难度/标题。" +
				"写日报/周报时应用此工具分析知识点分布，禁止编造标签。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{
						"type":        "string",
						"description": "list | user_profile | by_ids",
						"enum":        []string{"list", "user_profile", "by_ids"},
					},
					"userId": map[string]interface{}{
						"type":        "integer",
						"description": "user_profile 时必填：用户 id",
					},
					"problemIds": map[string]interface{}{
						"type":        "array",
						"description": "by_ids 时必填：题库 problemId 列表（最多 40）",
						"items":       map[string]interface{}{"type": "integer"},
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "list 时返回标签数量上限，默认 50，最大 200",
					},
				},
				"required": []string{"action"},
			},
		},
	}
}

func (c *ProblemTagsTool) AiInterface(jsonStr string) string {
	var p struct {
		Action     string  `json:"action"`
		UserId     int64   `json:"userId"`
		ProblemIds []int64 `json:"problemIds"`
		Limit      int32   `json:"limit"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	conn, err := c.coreDataRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := problem.NewProblemClient(conn)
	ctx := toolRPCContext(c.ctx)

	switch action {
	case "list":
		lim := p.Limit
		if lim <= 0 {
			lim = 50
		}
		if lim > 200 {
			lim = 200
		}
		res, err := cli.ListTags(ctx, &problem.ListTagsReq{Limit: lim})
		if err != nil {
			log.Error(err)
			return "查询标签列表失败: " + err.Error()
		}
		b, _ := json.Marshal(res.GetData())
		return fmt.Sprintf("全站题目标签(top %d): %s", lim, string(b))

	case "user_profile":
		if p.UserId <= 0 {
			return "user_profile 需要 userId>0"
		}
		res, err := cli.UserProfile(ctx, &problem.UserProfileReq{UserId: p.UserId})
		if err != nil {
			log.Error(err)
			return "查询用户标签画像失败: " + err.Error()
		}
		type out struct {
			Radar        interface{} `json:"radar"`
			Platforms    interface{} `json:"platforms"`
			Difficulties interface{} `json:"difficulties"`
			TotalAC      int64       `json:"totalAc"`
		}
		o := out{
			Radar:        res.GetRadar(),
			Platforms:    res.GetPlatforms(),
			Difficulties: res.GetDifficulties(),
			TotalAC:      res.GetTotalAc(),
		}
		b, _ := json.Marshal(o)
		return fmt.Sprintf("用户 %d 标签/平台/难度画像: %s", p.UserId, string(b))

	case "by_ids":
		ids := p.ProblemIds
		if len(ids) == 0 {
			return "by_ids 需要 problemIds 非空"
		}
		if len(ids) > 40 {
			ids = ids[:40]
		}
		type item struct {
			ID         uint32   `json:"problemId"`
			Title      string   `json:"title"`
			Platform   string   `json:"platform"`
			ExternalID string   `json:"externalId"`
			Difficulty string   `json:"difficulty"`
			Tags       []string `json:"tags"`
			Status     string   `json:"status"`
		}
		out := make([]item, 0, len(ids))
		for _, id := range ids {
			if id <= 0 {
				continue
			}
			res, err := cli.Get(ctx, &problem.GetProblemReq{Id: uint32(id)})
			if err != nil || res == nil || res.GetData() == nil {
				out = append(out, item{ID: uint32(id), Title: "查询失败"})
				continue
			}
			d := res.GetData()
			out = append(out, item{
				ID:         d.GetId(),
				Title:      d.GetTitle(),
				Platform:   d.GetPlatform(),
				ExternalID: d.GetExternalId(),
				Difficulty: d.GetDifficulty(),
				Tags:       d.GetTags(),
				Status:     d.GetStatus(),
			})
		}
		b, _ := json.Marshal(out)
		return fmt.Sprintf("题目标签明细(%d题): %s", len(out), string(b))

	default:
		return "action 须为 list | user_profile | by_ids"
	}
}
