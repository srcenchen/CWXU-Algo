package server

import (
	"cwxu-algo/api/agent/v1/summary"
	bizservice "cwxu-algo/app/agent/internal/biz/service"
	"cwxu-algo/app/agent/internal/data"
	"cwxu-algo/app/agent/internal/service"
	"cwxu-algo/app/common/conf"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/opsmetrics"
	authutil "cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/health"
	"cwxu-algo/app/common/utils/safeerrors"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/auth/jwt"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/transport/http"
	jwt2 "github.com/golang-jwt/jwt/v5"
)

// NewHTTPServer new an HTTP server.
func NewHTTPServer(c *conf.Server, logger log.Logger, d *data.Data, summaryService *service.SummaryService, summaryUC *bizservice.SummaryUseCase) *http.Server {
	var opts = []http.ServerOption{
		http.Middleware(
			recovery.Recovery(),
			safeerrors.Middleware(),
			opsmetrics.Middleware(d.RDB, "agent"),
			authutil.CookieBearer(),
			jwt.Server(func(token *jwt2.Token) (interface{}, error) {
				if token.Method != jwt2.SigningMethodRS256 {
					return nil, jwt2.ErrSignatureInvalid
				}
				return _const.JWTPublicKey(), nil
			}),
		),
	}
	if c.Http.Network != "" {
		opts = append(opts, http.Network(c.Http.Network))
	}
	if c.Http.Addr != "" {
		opts = append(opts, http.Address(c.Http.Addr))
	}
	if c.Http.Timeout != nil {
		opts = append(opts, http.Timeout(c.Http.Timeout.AsDuration()))
	}
	srv := http.NewServer(opts...)
	health.Register(srv, health.Checker{RDB: d.RDB})
	summary.RegisterSummaryHTTPServer(srv, summaryService)
	service.RegisterTrainingReportDownload(srv, summaryUC)
	return srv
}
