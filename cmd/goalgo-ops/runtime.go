package main

import (
	"context"
	"fmt"
	"os"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsprogress"
	"cwxu-algo/internal/opsprompt"
)

func installStart(ctx context.Context, compose *opscompose.Compose, progress *opsprogress.Progress) error {
	if err := compose.Version(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "goalgo-ops: 安装：初始化完成；未检测到 docker compose，请安装后执行 `goalgo-ops start`")
		return nil
	}
	progress.Sub("拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		return fmt.Errorf("拉取镜像：%w", err)
	}
	progress.Sub("创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return fmt.Errorf("启动服务：%w", err)
	}
	progress.Sub("等待健康并冒烟")
	if err := compose.Health(ctx); err != nil {
		return fmt.Errorf("健康检查：%w", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		return fmt.Errorf("冒烟测试：%w", err)
	}
	opsprogress.Done(os.Stderr, "安装完成")
	return nil
}

// deployCandidate 在缺省时交互选择 release 文件；非 TTY 时返回当前 release.env。
func deployCandidate(rootPath, current string, prompt *opsprompt.Prompter) (string, error) {
	if prompt.TTY {
		return prompt.String("release 文件路径", current)
	}
	return current, nil
}
