package service

import (
	"context"
	"fmt"
	"time"

	_const "cwxu-algo/app/common/const"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/metadata"
)

// 隐藏最高权限 Agent 身份：仅用于服务端工具调用 / 域数据拉取，不暴露登录入口。
// userId 使用固定大号，避免与真实用户冲突；isSiteAdmin=true 以通过站管读路径。
const (
	AgentHiddenUserID   uint   = 900000001
	AgentHiddenUsername        = "__goalgo_agent__"
	AgentHiddenName            = "GoAlgo Agent"
	agentTokenTTL              = 2 * time.Hour
)

// IssueElevatedAgentToken 签发站管级隐藏 Agent JWT（可带 org 上下文）。
func IssueElevatedAgentToken(orgID uint) (string, error) {
	if _const.JWTPrivateKey() == nil {
		return "", fmt.Errorf("JWT private key 未配置")
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"userId":      AgentHiddenUserID,
		"username":    AgentHiddenUsername,
		"name":        AgentHiddenName,
		"roleId":      1,
		"roleIds":     "[1]",
		"isSiteAdmin": true,
		"orgId":       orgID,
		"orgRole":     "org_admin",
		"exp":         now.Add(agentTokenTTL).Unix(),
		"nbf":         now.Unix(),
		"iat":         now.Unix(),
		"iss":         "goalgo",
		"aud":         "goalgo-web",
		"agent":       true,
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(_const.JWTPrivateKey())
}

// ContextWithElevatedAgent 将 Agent Bearer 注入 outgoing gRPC metadata 与本地可解析载荷。
func ContextWithElevatedAgent(ctx context.Context, orgID uint) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	token, err := IssueElevatedAgentToken(orgID)
	if err != nil {
		return ctx, err
	}
	md := metadata.Pairs("authorization", "Bearer "+token)
	out := metadata.NewOutgoingContext(ctx, md)
	return out, nil
}

// IsElevatedAgentUser 是否为隐藏 Agent 账号
func IsElevatedAgentUser(userID uint) bool {
	return userID == AgentHiddenUserID
}
