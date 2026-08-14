//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/user/internal/biz"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"
	"cwxu-algo/app/user/internal/server"
	"cwxu-algo/app/user/internal/service"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
	"gorm.io/gorm"
)

// provideUserDB 供需要直接访问 gorm.DB 的 provider（如工单服务）使用。
func provideUserDB(d *data.Data) *gorm.DB {
	return d.DB
}

// wireApp init kratos application.
func wireApp(*conf.Server, *conf.Data, log.Logger, *conf.SMTP, *conf.SupportCenter) (*kratos.App, func(), error) {
	panic(wire.Build(server.ProviderSet, service.ProviderSet, discovery.ProvideSet, data.ProviderSet, biz.ProviderSet, dal.ProviderSet, provideUserDB, newApp))
}
