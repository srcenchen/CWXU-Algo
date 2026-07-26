package core_data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cwxu-algo/api/core/v1/submit_log"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

type SubmitLogParms struct {
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	UserId    int    `json:"userId"`
	Limit     int    `json:"limit"`
}

type SubmitLog struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewSubmitLog(reg *registry.Registrar, ctxs ...context.Context) *SubmitLog {
	return &SubmitLog{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *SubmitLog) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *SubmitLog) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *SubmitLog) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "submit_log",
			Description: "获取指定用户id，获取从endDate开始，向前退limit条提交数据，需要注意的是，" +
				"Nowcoder 的平台如果contest出现main字样则代表我们只能拉取到正确的提交记录，不能说明用户只交了一次就对了，其他的平台不需要遵守这一条",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"endDate": map[string]interface{}{
						"type":        "string",
						"description": "结束时间，例如 20220101",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "从endDate开始，向前退limit条",
					},
					"userId": map[string]interface{}{
						"type":        "integer",
						"description": "用户id",
					},
				},
				"required": []string{"endDate", "limit", "userId"},
			},
		},
	}
}

func (c *SubmitLog) AiInterface(jsonStr string) string {
	scp := SubmitLogParms{}
	if err := json.Unmarshal([]byte(jsonStr), &scp); err != nil {
		return "参数错误"
	}
	res, err := c.Handle(scp.EndDate, scp.UserId, scp.Limit)
	if err != nil {
		return "查询失败" + err.Error()
	}
	return res
}

func (c *SubmitLog) Handle(endDate string, userId int, limit int) (string, error) {
	conn, err := c.coreDataRPC()
	if err != nil {
		return "", err
	}
	sb := submit_log.NewSubmitClient(conn)
	t, err := time.Parse("20060102", endDate)
	if err != nil {
		return "", errors.New("时间解析错误")
	}
	t = t.AddDate(0, 0, 1)
	res, err := sb.GetSubmitLog(
		toolRPCContext(c.ctx),
		&submit_log.GetSubmitLogReq{Limit: int64(limit), Cursor: t.Unix(), UserId: int64(userId)},
	)
	if err != nil {
		log.Error(err)
		return "", err
	}
	respJson, err := json.Marshal(res.Data)
	if err != nil {
		log.Error(err)
		return "", err
	}
	return fmt.Sprintf("用户id为%d在%s之前的最近%d条提交记录数据如下%s", userId, endDate, limit, string(respJson)), nil
}
