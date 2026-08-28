package main

import (
	"context"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/security"
	"cwxu-algo/app/core_data/internal/backupcoord"
	"cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/task"
	"flag"
	"fmt"
	"os"
	"time"

	"cwxu-algo/app/common/conf"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	"github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/go-kratos/kratos/v2/transport/http"

	_ "go.uber.org/automaxprocs"

	_ "cwxu-algo/app/core_data/internal/spider/platform"
)

// runForever 后台常驻任务：panic 后自动重启，直到 stop 关闭
func runForever(name string, stop <-chan struct{}, fn func()) {
	go func() {
		for {
			select {
			case <-stop:
				log.Infof("%s stopped", name)
				return
			default:
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Errorf("%s panic: %v，5s 后重启", name, r)
					}
				}()
				fn()
			}()
			select {
			case <-stop:
				log.Infof("%s stopped", name)
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

// go build -ldflags "-X main.Version=x.y.z"
var (
	// Name is the name of the compiled software.
	Name = "core-data"
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

func newApp(logger log.Logger, gs *grpc.Server, hs *http.Server, reg *discovery.Register, cm *service.Consumer, pfc *service.ProblemFetchConsumer, pac *service.ProblemAnalyzeConsumer, upc *service.UserProfileConsumer, mac *service.MailConsumer, spiderUseCase *service.SpiderUseCase, cron *task.CronTask, backup *backupcoord.Coordinator) *kratos.App {
	stopCh := make(chan struct{})
	runForever("spider-consumer", stopCh, cm.Consume)
	runForever("problem-fetch-consumer", stopCh, pfc.Consume)
	runForever("problem-analyze-consumer", stopCh, pac.Consume)
	runForever("user-profile-consumer", stopCh, upc.Consume)
	runForever("mail-consumer", stopCh, mac.Consume)
	runForever("client-sync-maintenance", stopCh, func() { spiderUseCase.RunClientSyncMaintenance(stopCh) })
	runForever("cron", stopCh, cron.Do)
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
		kratos.BeforeStop(func(ctx context.Context) error {
			log.Info("stopping background workers...")
			backupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := backup.Stop(backupCtx); err != nil {
				log.Warnf("backup shutdown wait: %v", err)
			}
			close(stopCh)
			cm.Stop()
			pfc.Stop()
			pac.Stop()
			upc.Stop()
			log.Info("stopping cron task...")
			cron.Stop()
			log.Info("background workers stop signal sent")
			return nil
		}),
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

	app, cleanup, err := wireApp(bc.Server, bc.Data, logger)
	if err != nil {
		panic(err)
	}
	defer cleanup()

	// start and wait for stop signal
	if err := app.Run(); err != nil {
		panic(err)
	}
}
