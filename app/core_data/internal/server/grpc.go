package server

import (
	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/api/core/v1/spider"
	statistic2 "cwxu-algo/api/core/v1/statistic"
	"cwxu-algo/api/core/v1/submit_log"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/core_data/internal/service"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/selector"
	"github.com/go-kratos/kratos/v2/selector/wrr"
	"github.com/go-kratos/kratos/v2/transport/grpc"
)

// NewGRPCServer new a gRPC server.
// Contest 必须注册：agent 训练报告/日报通过 gRPC 拉比赛列表与历史，漏注册会导致「比赛表现」全空。
func NewGRPCServer(
	c *conf.Server,
	logger log.Logger,
	spiderService *service.SpiderService,
	submitLogService *service.SubmitLogService,
	statisticService *service.StatisticService,
	contestLogService *service.ContestLogService,
) *grpc.Server {
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
	selector.SetGlobalSelector(wrr.NewBuilder())
	spider.RegisterSpiderServer(srv, spiderService)
	submit_log.RegisterSubmitServer(srv, submitLogService)
	statistic2.RegisterStatisticServer(srv, statisticService)
	contest_log.RegisterContestServer(srv, contestLogService)
	return srv
}
