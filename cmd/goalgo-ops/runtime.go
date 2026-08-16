package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsprogress"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsroot"
)

// dotenvValue 读取 env 文件中指定键的值。
func dotenvValue(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") {
			return strings.TrimPrefix(trimmed, key+"="), nil
		}
	}
	return "", fmt.Errorf("%s 未定义于 %s", key, path)
}

// resolveInstallRoot 确定安装根目录：显式 --root > $GOALGO_ROOT > 默认 .env 的 GOALGO_ROOT > /opt/goalgo。
// 这样 init 里填写的 GOALGO_ROOT 会决定后续 install 的安装位置，避免 root 与 .env 挂载路径错位。
func resolveInstallRoot(path string) (*opsroot.Root, error) {
	if path != "" {
		return opsroot.Resolve(path)
	}
	if env := os.Getenv("GOALGO_ROOT"); env != "" {
		return opsroot.Resolve(env)
	}
	if value, err := dotenvValue("/opt/goalgo/.env", "GOALGO_ROOT"); err == nil && value != "" {
		return opsroot.Resolve(value)
	}
	return opsroot.Resolve("")
}

// carryAnchorEnv 当安装根不是默认锚点位置时，把锚点 .env 复制到安装根，保留 init 填写的值。
func carryAnchorEnv(root *opsroot.Root) error {
	anchor := "/opt/goalgo/.env"
	if filepath.Clean(anchor) == root.Join(".env") {
		return nil
	}
	data, err := os.ReadFile(anchor)
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(root.Path, 0o755); err != nil {
		return err
	}
	return os.WriteFile(root.Join(".env"), data, 0o600)
}

func installStart(ctx context.Context, compose *opscompose.Compose, progress *opsprogress.Progress) error {
	if err := compose.Version(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "goalgo-ops: 安装：初始化完成；未检测到 docker compose，请安装后执行 `goalgo-ops start`")
		return nil
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
