package core_data

import (
	"context"
	"encoding/json"
	"fmt"

	"cwxu-algo/api/core/v1/submit_log"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

// OrgSubmitFeedTool 组织内提交动态（userId=-1，依赖 elevated JWT org）
type OrgSubmitFeedTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewOrgSubmitFeedTool(reg *registry.Registrar, ctxs ...context.Context) *OrgSubmitFeedTool {
	return &OrgSubmitFeedTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *OrgSubmitFeedTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *OrgSubmitFeedTool) coreDataRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "core-data")
}

func (c *OrgSubmitFeedTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "org_submit_feed",
			Description: "组织内提交动态流（当前 elevated org 成员）。" +
				"用于分析团队近期刷题节奏、平台分布、语言与 AC 情况。limit 默认 20，最大 30。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "条数，默认 20，最大 30",
					},
					"cursor": map[string]interface{}{
						"type":        "integer",
						"description": "时间游标 unix 秒，0 表示最新一页",
					},
				},
			},
		},
	}
}

func (c *OrgSubmitFeedTool) AiInterface(jsonStr string) string {
	var p struct {
		Limit  int64 `json:"limit"`
		Cursor int64 `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.Limit <= 0 {
		p.Limit = 20
	}
	if p.Limit > 30 {
		p.Limit = 30
	}
	conn, err := c.coreDataRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := submit_log.NewSubmitClient(conn)
	res, err := cli.GetSubmitLog(toolRPCContext(c.ctx), &submit_log.GetSubmitLogReq{
		UserId: -1,
		Limit:  p.Limit,
		Cursor: p.Cursor,
	})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	// 压缩字段，控制 token
	type item struct {
		UserID   int64    `json:"userId"`
		UserName string   `json:"userName,omitempty"`
		Platform string   `json:"platform"`
		Problem  string   `json:"problem"`
		Title    string   `json:"title,omitempty"`
		Status   string   `json:"status"`
		Lang     string   `json:"lang,omitempty"`
		Time     int64    `json:"time"`
		Tags     []string `json:"tags,omitempty"`
	}
	out := make([]item, 0, len(res.GetData()))
	for _, v := range res.GetData() {
		if v == nil {
			continue
		}
		out = append(out, item{
			UserID:   v.UserId,
			UserName: v.UserName,
			Platform: v.Platform,
			Problem:  v.Problem,
			Title:    v.ProblemTitle,
			Status:   v.Status,
			Lang:     v.Lang,
			Time:     v.Time,
			Tags:     append([]string(nil), v.ProblemTags...),
		})
	}
	b, _ := json.Marshal(out)
	return fmt.Sprintf("组织提交动态(%d条): %s", len(out), string(b))
}
