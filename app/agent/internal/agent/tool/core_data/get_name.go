package core_data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"cwxu-algo/api/user/v1/profile"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

type GetNameByIdJson struct {
	UserId int `json:"userId"`
}

type GetNameById struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewGetProfileById(reg *registry.Registrar, ctxs ...context.Context) *GetNameById {
	return &GetNameById{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *GetNameById) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *GetNameById) userRPC() (*grpc2.ClientConn, error) {
	if c == nil || c.reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(c.reg, "user")
}

func (c *GetNameById) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name:        "get_profile_by_user_id",
			Description: "根据用户id，获取用户资料包括姓名，邮箱等，将返回json",
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

func (c *GetNameById) AiInterface(jsonStr string) string {
	var req struct {
		UserId int `json:"userId"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &req); err != nil {
		return "参数错误"
	}
	res, err := c.Handle(req.UserId)
	if err != nil {
		return "查询失败" + err.Error()
	}
	return res
}

func (c *GetNameById) Handle(userId int) (string, error) {
	if userId == 0 {
		return "用户id不可以为0", errors.New("用户id不可以为0")
	}
	conn, err := c.userRPC()
	if err != nil {
		return "", err
	}
	sb := profile.NewProfileClient(conn)
	res, err := sb.GetById(
		toolRPCContext(c.ctx),
		&profile.GetByIdReq{UserId: int64(userId)},
	)
	if err != nil {
		log.Error(err)
		return "", err
	}
	respJson, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("用户id为%d的资料信息是%s", userId, respJson), nil
}
