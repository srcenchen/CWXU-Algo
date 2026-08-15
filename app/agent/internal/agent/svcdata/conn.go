package svcdata

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	grpc2 "google.golang.org/grpc"
)

// 工具调用高频且短促：按 registrar+服务名缓存长连接，避免每次调用 dial+Close。
// grpc.ClientConn / kratos HTTP Client 均并发安全，进程生命周期内复用即可；
// Bearer 等鉴权由每次 RPC 的 ctx metadata 携带，与连接无关。
var (
	sharedConnMu    sync.Mutex
	sharedGRPCConns = map[string]*grpc2.ClientConn{}
	sharedHTTPConns = map[string]*khttp.Client{}
)

func sharedConnKey(reg *registry.Registrar, service string) string {
	return fmt.Sprintf("%p|%s", reg, service)
}

// SharedGRPCConn 返回按服务名缓存的 gRPC 长连接（discovery:///<service>）。
// 调用方不得 Close；dial 失败不缓存，下次调用会重试。
func SharedGRPCConn(reg *registry.Registrar, service string) (*grpc2.ClientConn, error) {
	if reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	key := sharedConnKey(reg, service)
	sharedConnMu.Lock()
	defer sharedConnMu.Unlock()
	if conn, ok := sharedGRPCConns[key]; ok {
		return conn, nil
	}
	conn, err := grpc.DialInsecure(
		context.Background(),
		grpc.WithEndpoint("discovery:///"+service),
		grpc.WithDiscovery((*reg).(registry.Discovery)),
		grpc.WithTimeout(20*time.Second),
	)
	if err != nil {
		return nil, err
	}
	sharedGRPCConns[key] = conn
	return conn, nil
}

// SharedHTTPClient 返回按服务名缓存的服务发现 HTTP 客户端。调用方不得 Close。
func SharedHTTPClient(reg *registry.Registrar, service string) (*khttp.Client, error) {
	if reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	key := sharedConnKey(reg, service)
	sharedConnMu.Lock()
	defer sharedConnMu.Unlock()
	if cli, ok := sharedHTTPConns[key]; ok {
		return cli, nil
	}
	cli, err := khttp.NewClient(
		context.Background(),
		khttp.WithEndpoint("discovery:///"+service),
		khttp.WithDiscovery((*reg).(registry.Discovery)),
		khttp.WithTimeout(20*time.Second),
	)
	if err != nil {
		return nil, err
	}
	sharedHTTPConns[key] = cli
	return cli, nil
}
