package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cwxu-algo/app/agent/internal/agent/svcdata"

	"github.com/go-kratos/kratos/v2/registry"
)

func jsonIndent(v interface{}) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func jsonUnmarshal(b []byte, v interface{}) error {
	return json.Unmarshal(b, v)
}

// httpDiscoveryGet 服务发现 HTTP GET（带 elevated Bearer）
func httpDiscoveryGet(ctx context.Context, reg *registry.Registrar, service, path string) ([]byte, int, error) {
	if reg == nil {
		return nil, 0, fmt.Errorf("registry 未配置")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 复用共享长连接客户端，避免每次调用 NewClient+Close
	client, err := svcdata.SharedHTTPClient(reg, service)
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
	if tok := svcdata.BearerFromContext(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return b, res.StatusCode, nil
}
