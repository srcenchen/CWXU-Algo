package discovery

import (
	"cwxu-algo/app/common/conf"

	"github.com/go-kratos/kratos/contrib/registry/consul/v2"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/google/wire"
	"github.com/hashicorp/consul/api"
)

type Register struct {
	Reg registry.Registrar
}

// NewConsulRegister 仅在各服务启动期由 wire 生成代码调用一次；
// 改为返回 error 会破坏所有服务的 wire 依赖图（签名冻结），
// 且注册中心连不上时启动本就无法继续，故保留 panic（fail-fast）。
func NewConsulRegister(data *conf.Server) *Register {
	client, err := api.NewClient(&api.Config{Address: data.RegDsn})
	if err != nil {
		panic("注册中心链接失败" + err.Error())
	}
	// 缩短健康检查与陈旧注册清理时间：
	// 默认 DeregisterCriticalServiceAfter=600s 会让冷启动后残留的旧实例
	// 在 Consul 里挂 10 分钟；改 60s 后旧注册（容器 IP 已失效）尽快清除，
	// 网关 discovery（passingOnly）只看到存活实例，降低转发到死节点的概率。
	reg := consul.New(client,
		consul.WithHealthCheckInterval(5),
		consul.WithDeregisterCriticalServiceAfter(60),
	)
	return &Register{Reg: reg}
}

var ProvideSet = wire.NewSet(NewConsulRegister)
