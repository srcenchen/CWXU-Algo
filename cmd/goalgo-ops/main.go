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
	"time"

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
	case "deploy", "rollback", "start", "stop", "restart", "status", "logs", "doctor", "upgrade", "uninstall":
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
  upgrade [--root 目录]                        从 ACR 拉取最新镜像并升级（失败自动回滚）
  uninstall [--root 目录] [--yes]              彻底删除配置/密钥/数据/容器，并询问是否删除镜像
  rollback [--root 目录]                        回滚到上一个发布版本
  start | stop | restart | status | logs [参数] [--root 目录]
  doctor [--root 目录]                          检查安装是否健康
  backup verify [参数]                          离线校验灾备归档
  restore --file <路径|URL> [--key-file] ...    从归档恢复整实例（需 --replace --confirm RESTORE）
  config validate|export|import [参数]          校验 / 导出 / 导入配置与密钥

`)
}

func extractRoot(args []string) (string, []string, error) {
	var root string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--root" {
			if root != "" {
				return "", nil, fmt.Errorf("--root 只能指定一次")
			}
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--root 缺少路径")
			}
			root = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--root=") {
			if root != "" {
				return "", nil, fmt.Errorf("--root 只能指定一次")
			}
			root = strings.TrimPrefix(arg, "--root=")
			if root == "" {
				return "", nil, fmt.Errorf("--root 缺少路径")
			}
			continue
		}
		rest = append(rest, arg)
	}
	return root, rest, nil
}

func acquireForMutation(ctx context.Context, root *opsroot.Root) (*opslock.Lock, error) {
	return opslock.Acquire(lockPath(root), 0)
}

func lockPath(root *opsroot.Root) string {
	if override := os.Getenv("GOALGO_LOCK_FILE"); override != "" {
		return override
	}
	return "/run/lock/goalgo-ops.lock"
}

func cmdRuntime(command string, args []string, runner opsexec.Command) int {
	ctx, stop := signalContext()
	defer stop()
	rootPath, commandArgs, err := extractRoot(args)
	if err != nil {
		return fail("参数", err)
	}
	root, err := resolveRegisteredRoot(rootPath)
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
		return runtimeLogs(ctx, compose, commandArgs)
	case "deploy":
		return runtimeDeploy(ctx, compose, commandArgs)
	case "rollback":
		return runtimeRollback(ctx, compose)
	case "upgrade":
		return runtimeUpgrade(ctx, compose)
	case "uninstall":
		return runtimeUninstall(ctx, compose, commandArgs)
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
	previous, err := opsrelease.ParseFile(compose.Root.Join("release.previous.env"))
	if err != nil && !os.IsNotExist(err) {
		return fail("部署", err)
	}
	if releaseSame(release, current) {
		fmt.Fprintln(os.Stdout, "当前版本已在运行")
		return 0
	}
	if err := applyRelease(ctx, compose, release, current, previous); err != nil {
		return fail("部署", err)
	}
	return 0
}

func restoreReleaseFiles(compose *opscompose.Compose, current, previous *opsrelease.Release) error {
	if err := current.WriteFile(compose.Root.Join("release.env")); err != nil {
		return err
	}
	if previous == nil {
		if err := os.Remove(compose.Root.Join("release.previous.env")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return previous.WriteFile(compose.Root.Join("release.previous.env"))
}

func applyRelease(ctx context.Context, compose *opscompose.Compose, candidate, current, previous *opsrelease.Release) error {
	if err := current.WriteFile(compose.Root.Join("release.previous.env")); err != nil {
		return err
	}
	if err := candidate.WriteFile(compose.Root.Join("release.env")); err != nil {
		_ = restoreReleaseFiles(compose, current, previous)
		return err
	}
	runtimeMayHaveChanged := false
	apply := func() error {
		opsprogress.Note(os.Stderr, "拉取发布镜像")
		if err := compose.Pull(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		opsprogress.Note(os.Stderr, "创建并启动容器")
		runtimeMayHaveChanged = true
		if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
			return err
		}
		if err := compose.Health(ctx); err != nil {
			return err
		}
		return compose.Smoke(ctx)
	}
	if err := apply(); err != nil {
		if !runtimeMayHaveChanged {
			if restoreErr := restoreReleaseFiles(compose, current, previous); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("恢复版本文件失败：%w", restoreErr))
			}
			return err
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		rollbackErr := restoreRunningRelease(rollbackCtx, compose, current, previous)
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("恢复旧版本失败：%w", rollbackErr))
		}
		return err
	}
	return nil
}

func restoreRunningRelease(ctx context.Context, compose *opscompose.Compose, current, previous *opsrelease.Release) error {
	if err := restoreReleaseFiles(compose, current, previous); err != nil {
		return err
	}
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return err
	}
	if err := compose.Health(ctx); err != nil {
		return err
	}
	return compose.Smoke(ctx)
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
	if err := applyRelease(ctx, compose, previous, current, previous); err != nil {
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
	if err := requireBackupTools(ctx, compose.Run); err != nil {
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
	var rootPath, releaseFile, adminConfig string
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	flags.StringVar(&releaseFile, "release-file", "", "从文件写入经过校验的 release.env（默认解析 <svc>-latest）")
	flags.StringVar(&adminConfig, "admin-config", "", "管理员配置文件路径")
	if err := flags.Parse(args); err != nil {
		return fail("安装", err)
	}
	root, err := resolveInstallRoot(rootPath)
	if err != nil {
		return fail("安装", err)
	}
	if err := ensureInstallAvailable(root); err != nil {
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

	if err := ensureInstallAvailable(root); err != nil {
		return fail("安装", err)
	}
	progress := opsprogress.New(6, os.Stderr)
	inst := opsinstall.New(root)

	progress.Step("初始化目录结构与模板")
	if err := inst.Scaffold(); err != nil {
		return fail("安装", err)
	}
	compose := &opscompose.Compose{Root: root, Run: runner}
	if err := compose.ValidateRoot(); err != nil {
		return fail("安装", err)
	}

	progress.Step("生成密钥与 JWT 密钥对")
	if err := inst.Secrets(); err != nil {
		return fail("安装", err)
	}

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
		if err := opsrelease.LatestTagRelease().WriteFile(root.Join("release.env")); err != nil {
			return fail("安装", err)
		}
	}

	progress.Sub("拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		return fail("安装", fmt.Errorf("拉取镜像：%w", err))
	}

	progress.Step("启动服务")
	if err := installUp(ctx, compose, progress); err != nil {
		return fail("安装", err)
	}

	progress.Step("创建首个管理员（如无）")
	if !opsinstall.AdminCreated(root) {
		if err := opsadmin.Bootstrap(ctx, root, compose, adminConfig, opsprompt.New()); err != nil {
			return fail("安装", fmt.Errorf("创建管理员：%w", err))
		}
	} else {
		progress.Message("管理员已存在，跳过")
	}

	progress.Step("冒烟测试")
	if err := compose.Smoke(ctx); err != nil {
		return fail("安装", err)
	}
	if err := persistInstallRoot(root); err != nil {
		return fail("安装", fmt.Errorf("保存安装注册：%w", err))
	}
	opsprogress.Done(os.Stderr, "安装完成")
	return 0
}
