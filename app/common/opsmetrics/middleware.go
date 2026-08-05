package opsmetrics

import (
	"context"
	"time"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/redis/go-redis/v9"
)

// Middleware 统计 API 请求量与并发峰值（按服务名）+ 延迟样本
func Middleware(rdb *redis.Client, service string) middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req interface{}) (interface{}, error) {
			// 跳过健康检查
			if tr, ok := transport.FromServerContext(ctx); ok {
				op := tr.Operation()
				if op == "" || containsHealth(op) {
					return handler(ctx, req)
				}
			}
			start := time.Now()
			done := RecordAPIRequest(ctx, rdb, service)
			reply, err := handler(ctx, req)
			done(time.Since(start))
			return reply, err
		}
	}
}

func containsHealth(op string) bool {
	return op == "/healthz" || op == "/readyz" ||
		len(op) >= 7 && (op[len(op)-7:] == "healthz" || op[len(op)-6:] == "readyz")
}
