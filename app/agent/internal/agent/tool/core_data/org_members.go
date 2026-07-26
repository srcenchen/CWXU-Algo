package core_data

import (
	"context"
	"encoding/json"
	"fmt"

	"cwxu-algo/api/user/v1/profile"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

type OrgMembersTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewOrgMembersTool(reg *registry.Registrar, ctxs ...context.Context) *OrgMembersTool {
	return &OrgMembersTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *OrgMembersTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *OrgMembersTool) userRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "user")
}

func (c *OrgMembersTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "org_members",
			Description: "获取指定组织的全部成员 userId 列表",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"orgId": map[string]interface{}{"type": "integer", "description": "组织 id"},
				},
				"required": []string{"orgId"},
			},
		},
	}
}

func (c *OrgMembersTool) AiInterface(jsonStr string) string {
	var p struct {
		OrgId int64 `json:"orgId"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.OrgId <= 0 {
		return "orgId 无效"
	}
	conn, err := c.userRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := profile.NewProfileClient(conn)
	res, err := cli.GetUserIdsByOrg(toolRPCContext(c.ctx), &profile.GetUserIdsByOrgReq{OrgId: p.OrgId})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	b, _ := json.Marshal(res)
	return fmt.Sprintf("组织 %d 成员: %s", p.OrgId, string(b))
}

type GroupMembersTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewGroupMembersTool(reg *registry.Registrar, ctxs ...context.Context) *GroupMembersTool {
	return &GroupMembersTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *GroupMembersTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *GroupMembersTool) userRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "user")
}

func (c *GroupMembersTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "group_members",
			Description: "获取指定训练组的成员 userId 列表",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"groupId": map[string]interface{}{"type": "integer", "description": "组 id"},
				},
				"required": []string{"groupId"},
			},
		},
	}
}

func (c *GroupMembersTool) AiInterface(jsonStr string) string {
	var p struct {
		GroupId int64 `json:"groupId"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.GroupId <= 0 {
		return "groupId 无效"
	}
	conn, err := c.userRPC()
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := profile.NewProfileClient(conn)
	res, err := cli.GetUserIdsByGroup(toolRPCContext(c.ctx), &profile.GetUserIdsByGroupReq{GroupId: p.GroupId})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	b, _ := json.Marshal(res)
	return fmt.Sprintf("组 %d 成员: %s", p.GroupId, string(b))
}
