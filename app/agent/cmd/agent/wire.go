//go:build wireinject
// +build wireinject

// The build tag makes sure the stub is not built in the final build.

package main

import (
	"cwxu-algo/app/agent/internal/agent"
	"cwxu-algo/app/agent/internal/biz"
	"cwxu-algo/app/agent/internal/data"
	"cwxu-algo/app/agent/internal/server"
	"cwxu-algo/app/agent/internal/service"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/event"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
)

// wireApp init kratos application.
func wireApp(*conf.Server, *conf.Data, log.Logger) (*kratos.App, func(), error) {
	panic(wire.Build(server.ProviderSet,
		discovery.ProvideSet,
		agent.ProviderSet,
		event.ProviderSet,
		biz.ProviderSet, data.ProviderSet, service.ProviderSet, newApp))
}
