package server

import (
	"cwxu-algo/api/user/v1/plugin"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/api/user/v1/subscription"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/user/internal/service"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/transport/grpc"
)

// NewGRPCServer new a gRPC server.
func NewGRPCServer(c *conf.Server, logger log.Logger, profileService *service.ProfileService, subscriptionService *service.SubscriptionService, luoguPluginService *service.LuoguPluginService) *grpc.Server {
	var opts = []grpc.ServerOption{
		grpc.Middleware(
			recovery.Recovery(),
		),
	}
	if c.Grpc.Network != "" {
		opts = append(opts, grpc.Network(c.Grpc.Network))
	}
	if c.Grpc.Addr != "" {
		opts = append(opts, grpc.Address(c.Grpc.Addr))
	}
	if c.Grpc.Timeout != nil {
		opts = append(opts, grpc.Timeout(c.Grpc.Timeout.AsDuration()))
	}
	srv := grpc.NewServer(opts...)
	profile.RegisterProfileServer(srv, profileService)
	subscription.RegisterSubscriptionServer(srv, subscriptionService)
	plugin.RegisterLuoguPluginServer(srv, luoguPluginService)
	return srv
}
