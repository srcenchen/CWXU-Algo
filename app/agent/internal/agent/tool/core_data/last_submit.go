package core_data

import (
	"context"
	"encoding/json"
	"fmt"

	"cwxu-algo/api/core/v1/submit_log"
	"cwxu-algo/app/common/utils"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

type LastSubmitTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewLastSubmitTool(reg *registry.Registrar, ctxs ...context.Context) *LastSubmitTool {
	return &LastSubmitTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *LastSubmitTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *LastSubmitTool) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *LastSubmitTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "last_submit_times",
			Description: "批量查询用户最后一次提交的 unix 时间戳（秒），用于识别长期未训练成员",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"userIds": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "integer"},
						"description": "用户 id 列表，单次最多 200",
					},
				},
				"required": []string{"userIds"},
			},
		},
	}
}

func (c *LastSubmitTool) AiInterface(jsonStr string) string {
	var p struct {
		UserIds []int64 `json:"userIds"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if len(p.UserIds) == 0 {
		return "userIds 为空"
	}
	if len(p.UserIds) > 200 {
		p.UserIds = p.UserIds[:200]
	}
	conn, err := c.coreDataRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := submit_log.NewSubmitClient(conn)
	res, err := cli.LastSubmitTime(toolRPCContext(c.ctx), &submit_log.LastSubmitTimeReq{UserIds: p.UserIds})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	var m map[int64]int64
	if err := utils.GobDecoder(res.TimeMap, &m); err != nil {
		return "解析失败: " + err.Error()
	}
	b, _ := json.Marshal(m)
	return fmt.Sprintf("最后提交时间: %s", string(b))
}

// PeriodACTool 周期 AC/提交统计（委托 statistic_period，继承 elevated ctx）
type PeriodACTool struct {
	inner *StatisticPeriod
}

func NewPeriodACTool(reg *registry.Registrar, ctxs ...context.Context) *PeriodACTool {
	return &PeriodACTool{inner: NewStatisticPeriod(reg, ctxs...)}
}

func (c *PeriodACTool) AuthContext() context.Context {
	if c == nil || c.inner == nil {
		return context.Background()
	}
	return c.inner.AuthContext()
}

func (c *PeriodACTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "user_period_stats",
			Description: "获取用户今日/本周/上周/本月等提交与 AC 周期统计（同 statistic_period）",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"userId": map[string]interface{}{"type": "integer", "description": "用户 id"},
				},
				"required": []string{"userId"},
			},
		},
	}
}

func (c *PeriodACTool) AiInterface(jsonStr string) string {
	if c == nil || c.inner == nil {
		return "工具未初始化"
	}
	return c.inner.AiInterface(jsonStr)
}
