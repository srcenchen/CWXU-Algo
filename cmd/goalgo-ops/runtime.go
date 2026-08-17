package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
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

// resolveInstallRoot 仅用于尚未注册的 init/install 根选择。
// 显式 --root > GOALGO_ROOT 环境变量 > 已有 .env 中记录的 GOALGO_ROOT > 默认目录。
func resolveInstallRoot(path string) (*opsroot.Root, error) {
	return resolveInstallRootFrom(path, opsroot.JoinDefault(".env"))
}

// resolveInstallRootFrom 与 resolveInstallRoot 相同，但允许注入 .env 候选路径（便于测试）。
func resolveInstallRootFrom(path, envFile string) (*opsroot.Root, error) {
	if path != "" {
		return opsroot.Resolve(path)
	}
	if env := os.Getenv("GOALGO_ROOT"); env != "" {
		return opsroot.Resolve(env)
	}
	if configured := dotenvValueOrDefault(envFile, "GOALGO_ROOT"); configured != "" {
		return opsroot.Resolve(configured)
	}
	return opsroot.Resolve("")
}

// dotenvValueOrDefault 读取 env 文件中指定键的值，文件缺失或键不存在时返回空串。
func dotenvValueOrDefault(path, key string) string {
	value, err := dotenvValue(path, key)
	if err != nil {
		return ""
	}
	return value
}

func resolveRegisteredRoot(explicit string) (*opsroot.Root, error) {
	data, err := opsdata.Load()
	if err != nil {
		return nil, err
	}
	if data.Root == "" {
		return resolveInstallRoot(explicit)
	}
	registered, err := opsroot.Resolve(data.Root)
	if err != nil {
		return nil, err
	}
	if explicit != "" {
		selected, err := opsroot.Resolve(explicit)
		if err != nil {
			return nil, err
		}
		if selected.Path != registered.Path {
			return nil, fmt.Errorf("已安装实例位于 %s，拒绝使用其他 root %s", registered.Path, selected.Path)
		}
	}
	return registered, nil
}

func persistInstallRoot(root *opsroot.Root) error {
	data, err := opsdata.Load()
	if err != nil {
		return err
	}
	data.Root = root.Path
	return data.Save()
}

func ensureInstallAvailable(root *opsroot.Root) error {
	data, err := opsdata.Load()
	if err != nil {
		return err
	}
	if data.Root == "" {
		return nil
	}
	registered, err := opsroot.Resolve(data.Root)
	if err != nil {
		return err
	}
	if registered.Path == root.Path {
		return fmt.Errorf("%s 已安装，请使用 upgrade/restart", root.Path)
	}
	return fmt.Errorf("已有安装位于 %s，拒绝安装到 %s", registered.Path, root.Path)
}

func installUp(ctx context.Context, compose *opscompose.Compose, progress *opsprogress.Progress) error {
	if err := compose.Version(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "goalgo-ops: 安装：初始化完成；未检测到 docker compose，请安装后执行 `goalgo-ops start`")
		return nil
	}
	progress.Sub("创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return fmt.Errorf("启动服务：%w", err)
	}
	progress.Sub("等待健康")
	if err := compose.Health(ctx); err != nil {
		return fmt.Errorf("健康检查：%w", err)
	}
	return nil
}

// deployCandidate 在缺省时交互选择 release 文件；非 TTY 时返回当前 release.env。
func deployCandidate(rootPath, current string, prompt *opsprompt.Prompter) (string, error) {
	if prompt.TTY {
		return prompt.String("release 文件路径", current)
	}
	return current, nil
}

// runtimeUpgrade 解析 ACR <svc>-latest 为不可变 digest；有更新则原子升级，失败自动回滚。
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
	active, err := compose.Release()
	if err != nil {
		return fail("升级", err)
	}
	if sameDigests(data.Deploy.LastDigests, latest.Images) && releaseSame(active, latest) {
		fmt.Fprintln(os.Stdout, "已是最新版本")
		return 0
	}
	previous, err := opsrelease.ParseFile(compose.Root.Join("release.previous.env"))
	if err != nil && !os.IsNotExist(err) {
		return fail("升级", err)
	}
	if err := applyRelease(ctx, compose, latest, active, previous); err != nil {
		return fail("升级", err)
	}
	data.Deploy.LastDigests = latest.Images
	data.Deploy.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := data.Save(); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		rollbackErr := restoreRunningRelease(rollbackCtx, compose, active, previous)
		if rollbackErr != nil {
			return fail("升级", errors.Join(fmt.Errorf("保存 digest 状态失败：%w", err), fmt.Errorf("恢复旧版本失败：%w", rollbackErr)))
		}
		return fail("升级", fmt.Errorf("保存 digest 状态失败，已恢复旧版本：%w", err))
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

// runtimeUninstall 彻底删除：容器（compose down -v）→ root 目录（配置/密钥/数据）→ 系统注册文件 →
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
	envPath := compose.Root.Join(".env")
	var savedEnv []byte
	if data, err := os.ReadFile(envPath); err == nil {
		savedEnv = data
	}
	if err := os.RemoveAll(compose.Root.Path); err != nil {
		return fail("卸载", err)
	}
	if savedEnv != nil {
		opsprogress.Note(os.Stderr, "保留 .env（重新安装可复用）")
		if err := os.MkdirAll(compose.Root.Path, 0o755); err != nil {
			return fail("卸载", err)
		}
		if err := os.WriteFile(envPath, savedEnv, 0o600); err != nil {
			return fail("卸载", err)
		}
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
