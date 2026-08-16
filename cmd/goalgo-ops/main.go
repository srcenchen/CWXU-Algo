package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cwxu-algo/internal/opsadmin"
	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opsinstall"
	"cwxu-algo/internal/opslock"
	"cwxu-algo/internal/opsprogress"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsrelease"
	"cwxu-algo/internal/opsroot"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	command, commandArgs := args[0], args[1:]
	runner := opsexec.Real{}

	switch command {
	case "init":
		return cmdInit(commandArgs, runner)
	case "install":
		return cmdInstall(commandArgs, runner)
	case "admin-init":
		return cmdAdminInit(commandArgs, runner)
	case "backup":
		return cmdBackup(commandArgs, runner)
	case "restore":
		return cmdRestore(commandArgs, runner)
	case "config":
		return cmdConfig(commandArgs, runner)
	case "deploy", "rollback", "start", "stop", "restart", "status", "logs", "doctor":
		return cmdRuntime(command, commandArgs, runner)
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `goalgo-ops: GoAlgo 运维命令行工具

命令：
  init [--root] [--print]                交互式填写 .env（--print 仅打印模板）
  install [--root 目录] [--release-file 文件]   初始化目录并启动服务（默认解析 latest）
  admin-init [--root 目录] [--admin-config 文件] 创建首个站点管理员
  deploy [release-file] [--root 目录]           部署一个不可变发布版本
  rollback [--root 目录]                        回滚到上一个发布版本
  start | stop | restart | status | logs [参数] [--root 目录]
  doctor [--root 目录]                          检查安装是否健康
  backup verify|download [参数]                 离线校验 / 下载灾备归档
  restore [--file|--latest] [--key-file] ...    从归档恢复整实例（需 --replace --confirm RESTORE）
  config validate|export|import [参数]          校验 / 导出 / 导入配置与密钥

`)
}

func rootFromFlags(commandArgs []string) (string, error) {
	var root string
	flags := flag.NewFlagSet("root", flag.ContinueOnError)
	flags.StringVar(&root, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	if err := flags.Parse(commandArgs); err != nil {
		return "", err
	}
	return root, nil
}

func acquireForMutation(ctx context.Context, root *opsroot.Root) (*opslock.Lock, error) {
	return opslock.Acquire(lockPath(root), 0)
}

func lockPath(root *opsroot.Root) string {
	if override := os.Getenv("GOALGO_LOCK_FILE"); override != "" {
		return override
	}
	if root.IsProtectedInstall() {
		return "/run/lock/goalgo-ops.lock"
	}
	return root.Join("state", "goalgo-ops.lock")
}

func cmdRuntime(command string, args []string, runner opsexec.Command) int {
	ctx, stop := signalContext()
	defer stop()
	rootPath, err := rootFromFlags(args)
	if err != nil {
		return fail("参数", err)
	}
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
		return fail("根目录", err)
	}
	compose := &opscompose.Compose{Root: root, Run: runner}

	switch command {
	case "status", "logs", "doctor":
		// read-only commands do not hold the mutation lock
	default:
		lock, err := acquireForMutation(ctx, root)
		if err != nil {
			return fail("锁", err)
		}
		defer lock.Release()
	}

	switch command {
	case "start":
		return runtimeUp(ctx, compose)
	case "stop":
		return runtimeStop(ctx, compose)
	case "restart":
		return runtimeRestart(ctx, compose)
	case "status":
		return runtimeStatus(ctx, compose)
	case "logs":
		return runtimeLogs(ctx, compose, args)
	case "deploy":
		return runtimeDeploy(ctx, compose, args)
	case "rollback":
		return runtimeRollback(ctx, compose)
	case "doctor":
		return runtimeDoctor(ctx, compose)
	}
	usage()
	return 2
}

func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()
	return ctx, cancel
}

func fail(phase string, err error) int {
	fmt.Fprintf(os.Stderr, "goalgo-ops: %s: %v\n", phase, err)
	return 1
}

func runtimeUp(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("启动", err)
	}
	if _, err := compose.Release(); err != nil {
		return fail("启动", err)
	}
	opsprogress.Note(os.Stderr, "创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return fail("启动", err)
	}
	if err := compose.Health(ctx); err != nil {
		return fail("启动", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		return fail("启动", err)
	}
	return 0
}

func runtimeStop(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Stop(ctx); err != nil {
		return fail("停止", err)
	}
	return 0
}

func runtimeRestart(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("重启", err)
	}
	if _, err := compose.Release(); err != nil {
		return fail("重启", err)
	}
	if err := compose.Restart(ctx); err != nil {
		return fail("重启", err)
	}
	opsprogress.Note(os.Stderr, "创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return fail("重启", err)
	}
	if err := compose.Health(ctx); err != nil {
		return fail("重启", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		return fail("重启", err)
	}
	return 0
}

func runtimeStatus(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("状态", err)
	}
	output, err := compose.Command(ctx, "ps")
	if err != nil {
		return fail("状态", err)
	}
	fmt.Fprint(os.Stdout, output)
	return 0
}

func runtimeLogs(ctx context.Context, compose *opscompose.Compose, args []string) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("日志", err)
	}
	if err := compose.Logs(ctx, args...); err != nil {
		return fail("日志", err)
	}
	return 0
}

func runtimeDeploy(ctx context.Context, compose *opscompose.Compose, args []string) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("部署", err)
	}
	candidate := ""
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		candidate, rest = rest[0], rest[1:]
	}
	if candidate == "" {
		chosen, err := deployCandidate(compose.Root.Path, compose.Root.Join("release.env"), opsprompt.New())
		if err != nil {
			return fail("部署", err)
		}
		candidate = chosen
	}
	var release *opsrelease.Release
	var err error
	release, err = opsrelease.ParseFile(candidate)
	if err != nil {
		return fail("部署", err)
	}
	current, err := opsrelease.ParseFile(compose.Root.Join("release.env"))
	if err != nil {
		return fail("部署", err)
	}
	active, err := compose.Release()
	if err != nil {
		return fail("部署", err)
	}
	if !releaseSame(release, active) {
		if err := release.WriteFile(compose.Root.Join("release.previous.env")); err != nil {
			return fail("部署", err)
		}
		if err := release.WriteFile(compose.Root.Join("release.env")); err != nil {
			return fail("部署", err)
		}
	}
	opsprogress.Note(os.Stderr, "拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		return fail("部署", err)
	}
	opsprogress.Note(os.Stderr, "创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		if rollbackErr := rollbackFiles(compose, release, current); rollbackErr != nil {
			return fail("部署", errors.Join(err, rollbackErr))
		}
		return fail("部署", err)
	}
	if err := compose.Health(ctx); err != nil {
		if rollbackErr := rollbackFiles(compose, release, current); rollbackErr != nil {
			return fail("部署", errors.Join(err, rollbackErr))
		}
		return fail("部署", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		if rollbackErr := rollbackFiles(compose, release, current); rollbackErr != nil {
			return fail("部署", errors.Join(err, rollbackErr))
		}
		return fail("部署", err)
	}
	return 0
}

func rollbackFiles(compose *opscompose.Compose, release, current *opsrelease.Release) error {
	if err := current.WriteFile(compose.Root.Join("release.env")); err != nil {
		return err
	}
	return release.WriteFile(compose.Root.Join("release.previous.env"))
}

func runtimeRollback(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("回滚", err)
	}
	current, err := opsrelease.ParseFile(compose.Root.Join("release.env"))
	if err != nil {
		return fail("回滚", err)
	}
	previous, err := opsrelease.ParseFile(compose.Root.Join("release.previous.env"))
	if err != nil {
		return fail("回滚", err)
	}
	if err := current.WriteFile(compose.Root.Join("release.previous.env")); err != nil {
		return fail("回滚", err)
	}
	if err := previous.WriteFile(compose.Root.Join("release.env")); err != nil {
		return fail("回滚", err)
	}
	opsprogress.Note(os.Stderr, "拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		return fail("回滚", err)
	}
	opsprogress.Note(os.Stderr, "创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return fail("回滚", err)
	}
	if err := compose.Health(ctx); err != nil {
		return fail("回滚", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		return fail("回滚", err)
	}
	return 0
}

func runtimeDoctor(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("诊断", err)
	}
	if err := opsexec.RequireCommand(ctx, compose.Run, "docker"); err != nil {
		return fail("诊断", err)
	}
	if err := opsexec.RequireCommand(ctx, compose.Run, "curl"); err != nil {
		return fail("诊断", err)
	}
	if err := compose.Version(ctx); err != nil {
		return fail("诊断", err)
	}
	if _, err := compose.Release(); err != nil {
		return fail("诊断", err)
	}
	for _, relative := range []string{"config", "secrets", "data", "state", "logs"} {
		if info, err := os.Stat(compose.Root.Join(relative)); err != nil || !info.IsDir() {
			return fail("诊断", fmt.Errorf("目录缺失：%s", relative))
		}
	}
	for _, relative := range []string{"secrets/app.env", "secrets/jwt_private_key.pem", "secrets/jwt_public_key.pem", "secrets/postgres_password", "secrets/redis_password", "secrets/rabbitmq_password", "secrets/backup_encryption_key"} {
		if info, err := os.Stat(compose.Root.Join(relative)); err != nil || info.IsDir() {
			return fail("诊断", fmt.Errorf("缺失或不可读：%s", relative))
		}
	}
	if err := compose.Config(ctx); err != nil {
		return fail("诊断", err)
	}
	fmt.Println("goalgo：配置健康")
	return 0
}

func releaseSame(a, b *opsrelease.Release) bool {
	if a == nil || b == nil {
		return false
	}
	for key, value := range a.Images {
		if b.Images[key] != value {
			return false
		}
	}
	return true
}

func cmdInstall(args []string, runner opsexec.Command) int {
	ctx, stop := signalContext()
	defer stop()
	var rootPath, releaseFile string
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	flags.StringVar(&releaseFile, "release-file", "", "从文件写入经过校验的 release.env（默认解析 <svc>-latest）")
	if err := flags.Parse(args); err != nil {
		return fail("安装", err)
	}
	root, err := resolveInstallRoot(rootPath)
	if err != nil {
		return fail("安装", err)
	}
	if root.IsProtectedInstall() && !opsroot.IsPrivileged() {
		return fail("安装", fmt.Errorf("安装到 %s 需要 root 权限", root.Path))
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("安装", err)
	}
	defer lock.Release()

	if err := carryAnchorEnv(root); err != nil {
		return fail("安装", err)
	}

	progress := opsprogress.New(5, os.Stderr)
	inst := opsinstall.New(root)

	progress.Step("初始化目录结构与模板")
	if err := inst.Scaffold(); err != nil {
		return fail("安装", err)
	}

	progress.Step("生成密钥与 JWT 密钥对")
	if err := inst.Secrets(); err != nil {
		return fail("安装", err)
	}

	compose := &opscompose.Compose{Root: root, Run: runner}

	progress.Step("解析发布镜像")
	if releaseFile != "" {
		release, err := opsrelease.ParseFile(releaseFile)
		if err != nil {
			return fail("安装", err)
		}
		if err := release.WriteFile(root.Join("release.env")); err != nil {
			return fail("安装", err)
		}
	} else {
		release, err := compose.ResolveLatest(ctx)
		if err != nil {
			return fail("安装", err)
		}
		if err := release.WriteFile(root.Join("release.env")); err != nil {
			return fail("安装", err)
		}
	}

	progress.Sub("拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		return fail("安装", fmt.Errorf("拉取镜像：%w", err))
	}

	progress.Step("创建首个管理员（如无）")
	if !opsinstall.AdminCreated(root) {
		if err := opsadmin.Bootstrap(ctx, root, compose, "", opsprompt.New()); err != nil {
			return fail("安装", fmt.Errorf("创建管理员：%w", err))
		}
	} else {
		progress.Message("管理员已存在，跳过")
	}

	progress.Step("启动服务并冒烟")
	if err := installStart(ctx, compose, progress); err != nil {
		return fail("安装", err)
	}
	return 0
}
