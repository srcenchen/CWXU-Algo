package main

import (
	"errors"
	"flag"

	"cwxu-algo/internal/opsadmin"
	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opslock"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsroot"
)

func cmdAdminInit(args []string, runner opsexec.Command) int {
	ctx, stop := signalContext()
	defer stop()
	var rootPath, adminConfig string
	flags := flag.NewFlagSet("admin-init", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	flags.StringVar(&adminConfig, "admin-config", "", "管理员配置文件路径（必须 root 所有、0600、非符号链接；成功后删除）")
	if err := flags.Parse(args); err != nil {
		return fail("参数", err)
	}
	root, err := resolveRegisteredRoot(rootPath)
	if err != nil {
		return fail("根目录", err)
	}
	if root.IsProtectedInstall() && !opsroot.IsPrivileged() {
		return fail("管理员", errors.New("创建首个管理员需要 root 权限"))
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("锁", err)
	}
	defer lock.Release()
	compose := &opscompose.Compose{Root: root, Run: runner}
	if err := opsadmin.Bootstrap(ctx, root, compose, adminConfig, opsprompt.New()); err != nil {
		return fail("管理员", err)
	}
	return 0
}
