//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/event"
	"cwxu-algo/app/core_data/internal/backupcoord"
	"cwxu-algo/app/core_data/internal/biz"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/server"
	"cwxu-algo/app/core_data/internal/service"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
)

// wireApp init kratos application.
func wireApp(*conf.Server, *conf.Data, log.Logger) (*kratos.App, func(), error) {
	panic(wire.Build(service.ProviderSet, server.ProviderSet, discovery.ProvideSet, data.ProviderSet, biz.ProviderSet, event.ProviderSet, task.ProviderSet, dal.ProviderSet, backupcoord.ProviderSet, newApp))
}
