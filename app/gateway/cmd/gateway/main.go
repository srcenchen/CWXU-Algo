package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-kratos/gateway/client"
	configv1 "github.com/go-kratos/gateway/api/gateway/config/v1"
	"github.com/go-kratos/gateway/config"
	configLoader "github.com/go-kratos/gateway/config/config-loader"
	"github.com/go-kratos/gateway/discovery"
	"github.com/go-kratos/gateway/middleware"
	"github.com/go-kratos/gateway/proxy"
	"github.com/go-kratos/gateway/proxy/debug"
	"github.com/go-kratos/gateway/server"

	_ "net/http/pprof"

	_ "github.com/go-kratos/gateway/discovery/consul"
	_ "github.com/go-kratos/gateway/middleware/bbr"
	"github.com/go-kratos/gateway/middleware/circuitbreaker"
	_ "github.com/go-kratos/gateway/middleware/cors"
	_ "github.com/go-kratos/gateway/middleware/jwt"
	_ "github.com/go-kratos/gateway/middleware/logging"
	_ "github.com/go-kratos/gateway/middleware/rewrite"
	_ "github.com/go-kratos/gateway/middleware/streamrecorder"
	_ "github.com/go-kratos/gateway/middleware/tracing"
	_ "github.com/go-kratos/gateway/middleware/transcoder"
	_ "go.uber.org/automaxprocs"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/transport"
	"golang.org/x/exp/rand"
)

var (
	ctrlName          string
	ctrlService       string
	discoveryDSN      string
	proxyAddrs        = newSliceVar(":8080")
	proxyConfig       string
	priorityConfigDir string
	withDebug         bool
)

type sliceVar struct {
	val        []string
	defaultVal []string
}

func newSliceVar(defaultVal ...string) sliceVar {
	return sliceVar{defaultVal: defaultVal}
}
func (s *sliceVar) Get() []string {
	if len(s.val) <= 0 {
		return s.defaultVal
	}
	return s.val
}
func (s *sliceVar) Set(val string) error {
	s.val = append(s.val, val)
	return nil
}
func (s *sliceVar) String() string { return fmt.Sprintf("%+v", *s) }

func init() {
	rand.Seed(uint64(time.Now().Nanosecond()))

	flag.BoolVar(&withDebug, "debug", false, "enable debug handlers")
	flag.Var(&proxyAddrs, "addr", "proxy address, eg: -addr 0.0.0.0:8080")
	flag.StringVar(&proxyConfig, "conf", "config.yaml", "config path, eg: -conf config.yaml")
	flag.StringVar(&priorityConfigDir, "conf.priority", "", "priority config directory, eg: -conf.priority ./canary")
	flag.StringVar(&ctrlName, "ctrl.name", os.Getenv("ADVERTISE_NAME"), "control gateway name, eg: gateway")
	flag.StringVar(&ctrlService, "ctrl.service", "", "control service host, eg: http://127.0.0.1:8000")
	flag.StringVar(&discoveryDSN, "discovery.dsn", "consul://127.0.0.1:8500", "discovery dsn, eg: consul://127.0.0.1:7070?token=secret&datacenter=prod")
}

func makeDiscovery() registry.Discovery {
	if discoveryDSN == "" {
		return nil
	}
	d, err := discovery.Create(discoveryDSN)
	if err != nil {
		log.Fatalf("failed to create discovery: %v", err)
	}
	return d
}

// discoveryEndpoints 收集配置中所有 discovery:/// 后端名（去重），用于就绪门控。
func discoveryEndpoints(bc *configv1.Gateway) []string {
	seen := make(map[string]struct{})
	var endpoints []string
	for _, e := range bc.Endpoints {
		for _, backend := range e.Backends {
			t, err := client.ParseTarget(backend.Target)
			if err != nil || t.Scheme != "discovery" || t.Endpoint == "" {
				continue
			}
			if _, ok := seen[t.Endpoint]; ok {
				continue
			}
			seen[t.Endpoint] = struct{}{}
			endpoints = append(endpoints, t.Endpoint)
		}
	}
	return endpoints
}

// withLocalHealth 处理 /healthz 与 /readyz：
// /healthz 恒 200 作存活探针；/readyz 在全部 discovery 后端解析出实例前返回 503，
// 使 compose 健康检查/上游依赖在冷启动完成前不把网关判为可用。
func withLocalHealth(next http.Handler, requiredEndpoints []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/readyz" && !allServicesResolved(requiredEndpoints) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"not_ready"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allServicesResolved(endpoints []string) bool {
	for _, ep := range endpoints {
		if !client.ServiceResolved(ep) {
			return false
		}
	}
	return true
}

func main() {
	flag.Parse()

	clientFactory := client.NewFactory(makeDiscovery())
	p, err := proxy.New(clientFactory, middleware.Create)
	if err != nil {
		log.Fatalf("failed to new proxy: %v", err)
	}

	ctx := context.Background()
	var ctrlLoader *configLoader.CtrlConfigLoader
	if ctrlService != "" {
		log.Infof("setup control service to: %q", ctrlService)
		ctrlLoader = configLoader.New(ctrlName, ctrlService, proxyConfig, priorityConfigDir)
		if err := ctrlLoader.Load(ctx); err != nil {
			log.Errorf("failed to do initial load from control service: %v, using local config instead", err)
		}
		if err := ctrlLoader.LoadFeatures(ctx); err != nil {
			log.Errorf("failed to do initial feature load from control service: %v, using default value instead", err)
		}
		go ctrlLoader.Run(ctx)
	}

	confLoader, err := config.NewFileLoader(proxyConfig, priorityConfigDir)
	if err != nil {
		log.Fatalf("failed to create config file loader: %v", err)
	}
	defer confLoader.Close()
	bc, err := confLoader.Load(context.Background())
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// writeTimeout/readTimeout 至少覆盖配置中最长的端点超时（+30s 余量），
	// 否则长端点（如 /v1/core/* 120s）会先被 HTTP server 超时切断。
	maxEndpointTimeout := time.Duration(0)
	for _, e := range bc.Endpoints {
		if t := e.Timeout.AsDuration(); t > maxEndpointTimeout {
			maxEndpointTimeout = t
		}
	}
	server.EnsureTimeoutAtLeast(maxEndpointTimeout + 30*time.Second)

	buildContext := client.NewBuildContext(bc)
	circuitbreaker.Init(buildContext, clientFactory)
	if err := p.Update(buildContext, bc); err != nil {
		log.Fatalf("failed to update service config: %v", err)
	}
	reloader := func() error {
		bc, err := confLoader.Load(context.Background())
		if err != nil {
			log.Errorf("failed to load config: %v", err)
			return err
		}
		buildContext := client.NewBuildContext(bc)
		circuitbreaker.SetBuildContext(buildContext)
		if err := p.Update(buildContext, bc); err != nil {
			log.Errorf("failed to update service config: %v", err)
			return err
		}
		log.Infof("config reloaded")
		return nil
	}
	confLoader.Watch(reloader)

	var serverHandler http.Handler = withLocalHealth(p, discoveryEndpoints(bc))
	if withDebug {
		debug.Register("proxy", p)
		debug.Register("config", confLoader)
		if ctrlLoader != nil {
			debug.Register("ctrl", ctrlLoader)
		}
		serverHandler = debug.MashupWithDebugHandler(p)
	}
	servers := make([]transport.Server, 0, len(proxyAddrs.Get()))
	for _, addr := range proxyAddrs.Get() {
		servers = append(servers, server.NewProxy(serverHandler, addr))
	}
	app := kratos.New(
		kratos.Name(bc.Name),
		kratos.Context(ctx),
		kratos.Server(
			servers...,
		),
	)
	if err := app.Run(); err != nil {
		log.Errorf("failed to run servers: %v", err)
	}
}
