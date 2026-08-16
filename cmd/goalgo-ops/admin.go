package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opslock"
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
	root, err := opsroot.Resolve(rootPath)
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

	// 生成受保护的配置文件：优先使用 --admin-config，否则交互式采集。
	configPath := adminConfig
	if configPath == "" {
		configPath, err = promptAdminConfig(root)
		if err != nil {
			return fail("管理员", err)
		}
		defer os.Remove(configPath)
	} else {
		defer os.Remove(configPath)
	}
	if err := validateAdminConfigFile(configPath); err != nil {
		return fail("管理员", err)
	}

	compose := &opscompose.Compose{Root: root, Run: runner}
	containerMount := "/run/admin.env"
	runOptions := []string{
		"--user", "root",
		"--entrypoint", "/app/admin-init",
		"-v", configPath + ":" + containerMount + ":ro",
	}
	output, err := compose.RunService(ctx, "user", runOptions, "--admin-config", containerMount)
	if err != nil {
		return fail("管理员", fmt.Errorf("docker compose run 失败：%w\n%s", err, output))
	}
	return 0
}

func validateAdminConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("配置文件不能是符号链接")
	}
	if !info.Mode().IsRegular() {
		return errors.New("配置文件必须是普通文件")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("配置文件权限必须为 0600，当前为 %o", info.Mode().Perm())
	}
	return nil
}

func promptAdminConfig(root *opsroot.Root) (string, error) {
	var username, email, name, password, confirm string
	var err error
	fmt.Print("管理员用户名：")
	if _, err = fmt.Scanln(&username); err != nil {
		return "", fmt.Errorf("读取用户名：%w", err)
	}
	fmt.Print("管理员邮箱：")
	if _, err = fmt.Scanln(&email); err != nil {
		return "", fmt.Errorf("读取邮箱：%w", err)
	}
	fmt.Print("展示名（回车默认用户名）：")
	_, _ = fmt.Scanln(&name)
	if strings.TrimSpace(name) == "" {
		name = username
	}
	fd := int(os.Stdin.Fd())
	for {
		if term.IsTerminal(fd) {
			fmt.Print("密码（输入不可见）：")
			passwordBytes, err := term.ReadPassword(fd)
			fmt.Println()
			if err != nil {
				return "", fmt.Errorf("读取密码：%w", err)
			}
			password = string(passwordBytes)
			fmt.Print("再次输入密码确认：")
			confirmBytes, err := term.ReadPassword(fd)
			fmt.Println()
			if err != nil {
				return "", fmt.Errorf("读取密码确认：%w", err)
			}
			confirm = string(confirmBytes)
		} else {
			fmt.Print("密码：")
			if _, err = fmt.Scanln(&password); err != nil {
				return "", fmt.Errorf("读取密码：%w", err)
			}
			fmt.Print("再次输入密码确认：")
			if _, err = fmt.Scanln(&confirm); err != nil {
				return "", fmt.Errorf("读取密码确认：%w", err)
			}
		}
		if password != confirm {
			fmt.Fprintln(os.Stderr, "两次输入的密码不一致，请重试")
			continue
		}
		break
	}
	dir := root.Join("restore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "admin-bootstrap.env")
	content := fmt.Sprintf("ADMIN_USERNAME=%s\nADMIN_EMAIL=%s\nADMIN_NAME=%s\nADMIN_PASSWORD=%s\n",
		username, email, name, password)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
