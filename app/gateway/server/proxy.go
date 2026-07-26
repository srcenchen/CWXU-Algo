package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var (
	readHeaderTimeout = time.Second * 10
	readTimeout       = time.Second * 15
	writeTimeout      = time.Second * 15
	idleTimeout       = time.Second * 120
	// 环境变量显式配置时不再被 EnsureTimeoutAtLeast 动态抬高
	readTimeoutFromEnv  = false
	writeTimeoutFromEnv = false
)

func init() {
	var err error
	if v := os.Getenv("PROXY_READ_HEADER_TIMEOUT"); v != "" {
		if readHeaderTimeout, err = time.ParseDuration(v); err != nil {
			panic(err)
		}
	}
	if v := os.Getenv("PROXY_READ_TIMEOUT"); v != "" {
		if readTimeout, err = time.ParseDuration(v); err != nil {
			panic(err)
		}
		readTimeoutFromEnv = true
	}
	if v := os.Getenv("PROXY_WRITE_TIMEOUT"); v != "" {
		if writeTimeout, err = time.ParseDuration(v); err != nil {
			panic(err)
		}
		writeTimeoutFromEnv = true
	}
	if v := os.Getenv("PROXY_IDLE_TIMEOUT"); v != "" {
		if idleTimeout, err = time.ParseDuration(v); err != nil {
			panic(err)
		}
	}
}

// EnsureTimeoutAtLeast 依据端点最大超时抬高服务器 read/write 超时，
// 避免长端点（如 /v1/core/* 120s）尚未完成就被 writeTimeout(默认 15s) 掐断。
// 须在 NewProxy 之前调用；PROXY_READ_TIMEOUT / PROXY_WRITE_TIMEOUT 显式配置时以环境变量为准。
func EnsureTimeoutAtLeast(min time.Duration) {
	if min <= 0 {
		return
	}
	if !writeTimeoutFromEnv && writeTimeout < min {
		log.Infof("proxy write timeout raised to %s to cover the slowest endpoint timeout", min)
		writeTimeout = min
	}
	if !readTimeoutFromEnv && readTimeout < min {
		log.Infof("proxy read timeout raised to %s to cover the slowest endpoint timeout", min)
		readTimeout = min
	}
}

// ProxyServer is a proxy server.
type ProxyServer struct {
	*http.Server
}

// NewProxy new a gateway server.
func NewProxy(handler http.Handler, addr string) *ProxyServer {
	return &ProxyServer{
		Server: &http.Server{
			Addr: addr,
			Handler: h2c.NewHandler(handler, &http2.Server{
				IdleTimeout:          idleTimeout,
				MaxConcurrentStreams: math.MaxUint32,
			}),
			ReadTimeout:       readTimeout,
			ReadHeaderTimeout: readHeaderTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		},
	}
}

// Start the server.
func (s *ProxyServer) Start(ctx context.Context) error {
	log.Infof("proxy listening on %s", s.Addr)
	err := s.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop the server.
func (s *ProxyServer) Stop(ctx context.Context) error {
	log.Info("proxy stopping")
	return s.Shutdown(ctx)
}
