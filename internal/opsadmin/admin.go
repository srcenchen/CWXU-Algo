package opsadmin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsinstall"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsroot"
)

func ValidateAdminConfigFile(path string) error {
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

// Bootstrap 创建首个站点管理员。configPath 为空时通过 prompt 交互采集；成功后标记 install.json。
func Bootstrap(ctx context.Context, root *opsroot.Root, compose *opscompose.Compose, configPath string, prompt *opsprompt.Prompter) error {
	if configPath == "" {
		var err error
		configPath, err = promptAdminConfig(root, prompt)
		if err != nil {
			return err
		}
	}
	defer os.Remove(configPath)
	if err := ValidateAdminConfigFile(configPath); err != nil {
		return err
	}
	const containerMount = "/run/admin.env"
	// 必须以可写方式挂载：容器内 admin-init 成功创建后会删除自己的配置文件
	// （os.Remove /run/admin.env）以清除凭据；只读挂载会导致删除失败并让 run 返回非零。
	if _, err := compose.RunService(ctx, "user",
		[]string{"--user", "root", "--entrypoint", "/app/admin-init", "-v", configPath + ":" + containerMount},
		"--admin-config", containerMount); err != nil {
		return fmt.Errorf("docker compose run 失败：%w", err)
	}
	return opsinstall.MarkAdminCreated(root)
}

func promptAdminConfig(root *opsroot.Root, prompt *opsprompt.Prompter) (string, error) {
	username, err := prompt.String("管理员用户名", "")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(username) == "" {
		return "", errors.New("管理员用户名不能为空")
	}
	email, err := prompt.String("管理员邮箱", "")
	if err != nil {
		return "", err
	}
	if !strings.Contains(email, "@") {
		return "", errors.New("邮箱格式不正确")
	}
	name, err := prompt.String("展示名（默认用户名）", username)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		name = username
	}
	var password string
	for {
		first, err := prompt.Password("密码")
		if err != nil {
			return "", err
		}
		second, err := prompt.Password("再次输入密码确认")
		if err != nil {
			return "", err
		}
		if first == "" || first != second {
			fmt.Fprintln(prompt.Out, "两次输入的密码不一致或为空，请重试")
			continue
		}
		password = first
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
	return path, nil
}
