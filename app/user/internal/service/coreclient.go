package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cwxu-algo/app/common/discovery"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	grpc2 "google.golang.org/grpc"
)

// user→core-data 共享 gRPC 长连接：懒初始化后全进程复用，
// 避免每个请求都新建/关闭连接（连接内断线重连由 gRPC 自行处理）。
// 建连失败不缓存错误，下次调用会重试。
var (
	coreDataConnMu sync.Mutex
	coreDataConn   *grpc2.ClientConn
)

// sharedCoreDataConn 返回共享连接；调用方不得 Close。
func sharedCoreDataConn(reg *discovery.Register) (*grpc2.ClientConn, error) {
	coreDataConnMu.Lock()
	defer coreDataConnMu.Unlock()
	if coreDataConn != nil {
		return coreDataConn, nil
	}
	if reg == nil {
		return nil, fmt.Errorf("core-data 服务发现未就绪")
	}
	disc, ok := reg.Reg.(registry.Discovery)
	if !ok {
		return nil, fmt.Errorf("core-data 服务发现类型不支持")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialInsecure(
		ctx,
		grpc.WithEndpoint("discovery:///core-data"),
		grpc.WithDiscovery(disc),
		grpc.WithTimeout(20*time.Second),
	)
	if err != nil {
		return nil, err
	}
	coreDataConn = conn
	return coreDataConn, nil
}
