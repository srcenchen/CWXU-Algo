package core_data

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-kratos/kratos/v2/registry"
)

// discoveryHTTPGet 经服务发现对目标服务发 GET（带 elevated Bearer）。
// service 形如 "core-data" / "user"；path 含 query。
func discoveryHTTPGet(ctx context.Context, reg *registry.Registrar, service, path string) ([]byte, int, error) {
	if reg == nil {
		return nil, 0, fmt.Errorf("registry 未配置")
	}
	ctx = toolRPCContext(ctx)
	// 复用共享长连接客户端，避免每次工具调用 NewClient+Close
	client, err := SharedHTTPClient(reg, service)
	if err != nil {
		return nil, 0, err
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	if tok := BearerFromContext(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20)) // 2MB
	if err != nil {
		return nil, res.StatusCode, err
	}
	return b, res.StatusCode, nil
}
