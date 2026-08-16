package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsdata"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opsprogress"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsrelease"
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

// runtimeUpgrade 解析 ACR <svc>-latest 的 digest 用于版本判断（持久化到 ~/.ops.data.json），
// 实际镜像仍用 latest 标签；有更新则原子升级，失败自动回滚。
func runtimeUpgrade(ctx context.Context, compose *opscompose.Compose) int {
	if err := compose.Root.RequireFiles(); err != nil {
		return fail("升级", err)
	}
	latest, err := compose.ResolveLatest(ctx)
	if err != nil {
		return fail("升级", err)
	}
	data, err := opsdata.Load()
	if err != nil {
		return fail("升级", err)
	}
	if sameDigests(data.Deploy.LastDigests, latest.Images) {
		fmt.Fprintln(os.Stdout, "已是最新版本")
		return 0
	}
	active, err := compose.Release()
	if err != nil {
		return fail("升级", err)
	}
	data.Deploy.LastDigests = latest.Images
	data.Deploy.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := data.Save(); err != nil {
		return fail("升级", err)
	}
	release := opsrelease.LatestTagRelease()
	if err := release.WriteFile(compose.Root.Join("release.previous.env")); err != nil {
		return fail("升级", err)
	}
	if err := release.WriteFile(compose.Root.Join("release.env")); err != nil {
		return fail("升级", err)
	}
	rollback := func() { _ = rollbackFiles(compose, release, active) }
	opsprogress.Note(os.Stderr, "拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		rollback()
		return fail("升级", err)
	}
	opsprogress.Note(os.Stderr, "创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		rollback()
		return fail("升级", err)
	}
	if err := compose.Health(ctx); err != nil {
		rollback()
		return fail("升级", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		rollback()
		return fail("升级", err)
	}
	opsprogress.Done(os.Stderr, "升级完成")
	return 0
}

func sameDigests(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// runtimeUninstall 彻底删除：容器（compose down -v）→ root 目录（配置/密钥/数据）→ ~/.ops.data.json →
// 询问是否删除 goalgo 镜像。--yes 跳过确认并删除镜像。
func runtimeUninstall(ctx context.Context, compose *opscompose.Compose, args []string) int {
	var yes bool
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.BoolVar(&yes, "yes", false, "跳过确认并删除 goalgo 镜像")
	if err := flags.Parse(args); err != nil {
		return fail("卸载", err)
	}
	if compose.Root.IsProtectedInstall() && !opsroot.IsPrivileged() {
		return fail("卸载", errors.New("卸载受保护安装需要 root 权限"))
	}
	prompt := opsprompt.New()
	if !yes {
		if !prompt.TTY {
			return fail("卸载", fmt.Errorf("破坏性操作，请在终端确认或使用 --yes"))
		}
		confirmed, err := prompt.Confirm(fmt.Sprintf("将彻底删除 %s 下全部配置、密钥与数据（含数据库），继续？", compose.Root.Path), false)
		if err != nil {
			return fail("卸载", err)
		}
		if !confirmed {
			fmt.Fprintln(os.Stdout, "已取消")
			return 0
		}
	}
	if _, err := os.Stat(compose.Root.Join("compose.yaml")); err == nil {
		opsprogress.Note(os.Stderr, "停止并删除容器")
		if _, err := compose.Command(ctx, "down", "-v"); err != nil {
			return fail("卸载", err)
		}
	}
	opsprogress.Note(os.Stderr, "删除配置、密钥与数据目录")
	if err := os.RemoveAll(compose.Root.Path); err != nil {
		return fail("卸载", err)
	}
	if dataPath := opsdata.Path(); fileExists(dataPath) {
		if err := os.Remove(dataPath); err != nil {
			return fail("卸载", err)
		}
	}
	removeImages := yes
	if !yes {
		var err error
		removeImages, err = prompt.Confirm("删除 registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo:* 镜像？", false)
		if err != nil {
			return fail("卸载", err)
		}
	}
	if removeImages {
		if err := removeGoalgoImages(ctx, compose.Run); err != nil {
			return fail("卸载", err)
		}
	}
	opsprogress.Done(os.Stderr, "卸载完成")
	return 0
}

func removeGoalgoImages(ctx context.Context, runner opsexec.Command) error {
	output, err := runner.CombinedOutput(ctx, "docker", "images", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return err
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo:") {
			refs = append(refs, line)
		} else if strings.HasPrefix(line, "goalgo-") {
			refs = append(refs, line)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if _, err := runner.CombinedOutput(ctx, "docker", append([]string{"rmi", "-f"}, refs...)...); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
