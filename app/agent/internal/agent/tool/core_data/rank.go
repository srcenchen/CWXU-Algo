package core_data

import (
	"context"
	"encoding/json"
	"fmt"

	"cwxu-algo/api/core/v1/statistic"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

type RankTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewRankTool(reg *registry.Registrar, ctxs ...context.Context) *RankTool {
	return &RankTool{reg: reg, ctx: firstCtx(ctxs...)}
}

// AuthContext returns the elevated RPC context (for tests / diagnostics).
func (c *RankTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *RankTool) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *RankTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "rank",
			Description: "按日期区间获取提交或 AC 排行榜（scoreType=submit|ac），可按 groupId 筛选，-1 表示全部",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"startDate": map[string]interface{}{"type": "string", "description": "YYYY-MM-DD"},
					"endDate":   map[string]interface{}{"type": "string", "description": "YYYY-MM-DD"},
					"scoreType": map[string]interface{}{"type": "string", "description": "submit 或 ac"},
					"pageSize":  map[string]interface{}{"type": "integer", "description": "条数，默认 10，最大 50"},
					"groupId":   map[string]interface{}{"type": "integer", "description": "组 ID，-1 全部"},
				},
				"required": []string{"startDate", "endDate"},
			},
		},
	}
}

func (c *RankTool) AiInterface(jsonStr string) string {
	var p struct {
		StartDate string `json:"startDate"`
		EndDate   string `json:"endDate"`
		ScoreType string `json:"scoreType"`
		PageSize  int64  `json:"pageSize"`
		GroupId   int64  `json:"groupId"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.PageSize <= 0 {
		p.PageSize = 10
	}
	if p.PageSize > 50 {
		p.PageSize = 50
	}
	if p.ScoreType == "" {
		p.ScoreType = "submit"
	}
	if p.GroupId == 0 {
		p.GroupId = -1
	}
	conn, err := c.coreDataRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := statistic.NewStatisticClient(conn)
	res, err := cli.Rank(toolRPCContext(c.ctx), &statistic.RankReq{
		StartDate: p.StartDate,
		EndDate:   p.EndDate,
		ScoreType: p.ScoreType,
		Page:      1,
		PageSize:  p.PageSize,
		GroupId:   p.GroupId,
	})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	b, _ := json.Marshal(res)
	return fmt.Sprintf("排行榜 %s~%s type=%s: %s", p.StartDate, p.EndDate, p.ScoreType, string(b))
}

func firstCtx(ctxs ...context.Context) context.Context {
	if len(ctxs) > 0 && ctxs[0] != nil {
		return ctxs[0]
	}
	return context.Background()
}
