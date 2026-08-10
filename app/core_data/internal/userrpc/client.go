// Package userrpc 提供到 user 服务的进程内复用 gRPC 连接，避免每次请求 Dial/Close。
package userrpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/api/user/v1/subscription"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	grpc2 "google.golang.org/grpc"
)

var (
	mu   sync.Mutex
	conn *grpc2.ClientConn
)

// Conn 返回到 discovery:///user 的长连接（进程单例）。
// reg 为 *registry.Registrar（项目内惯例：指向同时实现 Discovery 的 Registrar）。
func Conn(reg *registry.Registrar) (*grpc2.ClientConn, error) {
	if reg == nil {
		return nil, fmt.Errorf("registry not configured")
	}
	disc, ok := (*reg).(registry.Discovery)
	if !ok || disc == nil {
		return nil, fmt.Errorf("registry is not Discovery")
	}

	mu.Lock()
	defer mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	c, err := grpc.DialInsecure(
		context.Background(),
		grpc.WithEndpoint("discovery:///user"),
		grpc.WithDiscovery(disc),
		grpc.WithTimeout(15*time.Second),
	)
	if err != nil {
		return nil, err
	}
	conn = c
	return conn, nil
}

// ProfileClient 复用长连接的 ProfileClient。调用方不要 Close 底层连接。
func ProfileClient(reg *registry.Registrar) (profile.ProfileClient, error) {
	c, err := Conn(reg)
	if err != nil {
		return nil, err
	}
	return profile.NewProfileClient(c), nil
}

// SubscriptionClient 复用长连接的 SubscriptionClient（AI 分析配额等）。调用方不要 Close 底层连接。
func SubscriptionClient(reg *registry.Registrar) (subscription.SubscriptionClient, error) {
	c, err := Conn(reg)
	if err != nil {
		return nil, err
	}
	return subscription.NewSubscriptionClient(c), nil
}
