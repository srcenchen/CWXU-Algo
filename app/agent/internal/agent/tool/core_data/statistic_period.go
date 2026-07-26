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

type StatisticPeriodParams struct {
	UserId int `json:"userId"`
}

type StatisticPeriod struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewStatisticPeriod(reg *registry.Registrar, ctxs ...context.Context) *StatisticPeriod {
	return &StatisticPeriod{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *StatisticPeriod) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *StatisticPeriod) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *StatisticPeriod) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "statistic_period",
			Description: "获取指定用户id的各时间段统计数据，包括今日、本周、上周、本月、上月、本年、去年和总数的AC记录以及全部提交记录",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"userId": map[string]interface{}{
						"type":        "integer",
						"description": "用户id",
					},
				},
				"required": []string{"userId"},
			},
		},
	}
}

func (c *StatisticPeriod) AiInterface(jsonStr string) string {
	spp := StatisticPeriodParams{}
	if err := json.Unmarshal([]byte(jsonStr), &spp); err != nil {
		return "参数错误"
	}
	res, err := c.Handle(spp.UserId)
	if err != nil {
		return "查询失败" + err.Error()
	}
	return res
}

func (c *StatisticPeriod) Handle(userId int) (string, error) {
	conn, err := c.coreDataRPC()
	if err != nil {
		return "", err
	}
	sb := statistic.NewStatisticClient(conn)
	res, err := sb.PeriodCount(
		toolRPCContext(c.ctx),
		&statistic.PeriodCountReq{UserId: int64(userId)},
	)
	if err != nil {
		log.Error(err)
		return "", err
	}
	respJson, err := json.Marshal(res)
	if err != nil {
		log.Error(err)
		return "", err
	}
	return fmt.Sprintf("用户id为%d的统计数据如下%s", userId, string(respJson)), nil
}
