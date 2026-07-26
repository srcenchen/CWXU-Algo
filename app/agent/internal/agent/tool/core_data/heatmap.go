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

// HeatmapTool 热力图（可指定 isAc）；userId=0 为组织维度（依赖 elevated JWT org）
type HeatmapTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewHeatmapTool(reg *registry.Registrar, ctxs ...context.Context) *HeatmapTool {
	return &HeatmapTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *HeatmapTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *HeatmapTool) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *HeatmapTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "heatmap",
			Description: "获取用户或组织在日期区间内的每日提交/AC 热力。userId>0 个人；userId=0 组织聚合。isAc=true 只计 AC",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"userId":    map[string]interface{}{"type": "integer", "description": "用户 id，0=组织"},
					"startDate": map[string]interface{}{"type": "string", "description": "YYYY-MM-DD 或 YYYYMMDD"},
					"endDate":   map[string]interface{}{"type": "string", "description": "YYYY-MM-DD 或 YYYYMMDD"},
					"isAc":      map[string]interface{}{"type": "boolean", "description": "是否只统计 AC"},
				},
				"required": []string{"userId", "startDate", "endDate"},
			},
		},
	}
}

func (c *HeatmapTool) AiInterface(jsonStr string) string {
	var p struct {
		UserId    int64  `json:"userId"`
		StartDate string `json:"startDate"`
		EndDate   string `json:"endDate"`
		IsAc      bool   `json:"isAc"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	conn, err := c.coreDataRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := statistic.NewStatisticClient(conn)
	res, err := cli.Heatmap(toolRPCContext(c.ctx), &statistic.HeatmapReq{
		UserId:    p.UserId,
		StartDate: p.StartDate,
		EndDate:   p.EndDate,
		IsAc:      p.IsAc,
	})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	b, _ := json.Marshal(res)
	return fmt.Sprintf("热力 user=%d %s~%s isAc=%v: %s", p.UserId, p.StartDate, p.EndDate, p.IsAc, string(b))
}
