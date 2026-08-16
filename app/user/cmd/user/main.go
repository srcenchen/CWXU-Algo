package main

import (
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/security"
	"cwxu-algo/app/user/internal/data"
	"flag"
	"fmt"
	"os"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/go-kratos/kratos/v2/transport/http"

	_ "go.uber.org/automaxprocs"
)

// go build -ldflags "-X main.Version=x.y.z"
var (
	// Name is the name of the compiled software.
	Name = "user"
	// Version is the version of the compiled software.
	Version string = "v1"
	// flagconf is the config flag.
	flagconf string

	//id, _ = os.Hostname()
	id, _ = os.Hostname()
)

func init() {
	flag.StringVar(&flagconf, "conf", "./configs", "config path, eg: -conf config.yaml")
}

func newApp(logger log.Logger, gs *grpc.Server, hs *http.Server, reg *discovery.Register) *kratos.App {
	return kratos.New(
		kratos.ID(fmt.Sprintf("%s-%s-%s", id, Name, Version)),
		kratos.Name(Name),
		kratos.Version(Version),
		kratos.Metadata(map[string]string{}),
		kratos.Logger(logger),
		kratos.Server(
			gs,
			hs,
		),
		kratos.Registrar(reg.Reg),
	)
}

func main() {
	flag.Parse()
	logger := log.With(log.NewStdLogger(os.Stdout),
		"ts", log.DefaultTimestamp,
		"caller", log.DefaultCaller,
		"service.id", id,
		"service.name", Name,
		"service.version", Version,
		"trace.id", tracing.TraceID(),
		"span.id", tracing.SpanID(),
	)
	c := config.New(
		config.WithSource(
			file.NewSource(flagconf),
		),
	)
	defer c.Close()

	if err := c.Load(); err != nil {
		panic(err)
	}

	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		panic(err)
	}
	if err := security.Configure(bc.Server); err != nil {
		panic(err)
	}
	legacy := data.LegacySiteConfig{ConfigEncryptionKey: bc.Server.GetConfigEncryptionKey()}
	if bc.Smtp != nil {
		legacy.SMTPHost, legacy.SMTPPort, legacy.SMTPUsername, legacy.SMTPPassword, legacy.SMTPFrom = bc.Smtp.Host, int(bc.Smtp.Port), bc.Smtp.Username, bc.Smtp.Password, bc.Smtp.From
	}
	if bc.Agent != nil {
		legacy.AgentEndpoint, legacy.AgentModel, legacy.AgentSecret = bc.Agent.Endpoint, bc.Agent.Model, bc.Agent.Secret
	}
	if bc.AiAnalyze != nil {
		legacy.AiAnalyzeEndpoint, legacy.AiAnalyzeModel, legacy.AiAnalyzeSecret = bc.AiAnalyze.Endpoint, bc.AiAnalyze.Model, bc.AiAnalyze.Secret
	}

	app, cleanup, err := wireApp(bc.Server, bc.Data, logger, bc.SupportCenter, legacy)
	if err != nil {
		panic(err)
	}
	defer cleanup()

	// start and wait for stop signal
	if err := app.Run(); err != nil {
		panic(err)
	}
}
