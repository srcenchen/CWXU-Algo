package main

import (
	"archive/tar"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opslock"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsrelease"
	"cwxu-algo/internal/opsroot"
)

func cmdConfig(args []string, runner opsexec.Command) int {
	if len(args) < 1 {
		return fail("config", fmt.Errorf("缺少子命令：validate|export|import"))
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "validate":
		return configValidate(rest, runner)
	case "export":
		return configExport(rest)
	case "import":
		return configImport(rest)
	default:
		return fail("config", fmt.Errorf("未知子命令 %q（支持 validate|export|import）", sub))
	}
}

func configValidate(args []string, runner opsexec.Command) int {
	var rootPath string
	flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录")
	if err := flags.Parse(args); err != nil {
		return fail("config validate", err)
	}
	root, err := resolveRegisteredRoot(rootPath)
	if err != nil {
		return fail("config validate", err)
	}
	if err := root.RequireFiles(); err != nil {
		return fail("config validate", err)
	}
	if _, err := opsrelease.ParseFile(root.Join("release.env")); err != nil {
		return fail("config validate", err)
	}
	for _, relative := range []string{"config", "secrets", "data", "state", "logs"} {
		info, err := os.Stat(root.Join(relative))
		if err != nil || !info.IsDir() {
			return fail("config validate", fmt.Errorf("目录缺失：%s", relative))
		}
	}
	for _, relative := range []string{"secrets/app.env", "secrets/jwt_private_key.pem", "secrets/jwt_public_key.pem", "secrets/postgres_password", "secrets/redis_password", "secrets/rabbitmq_password", "secrets/backup_encryption_key"} {
		info, err := os.Stat(root.Join(relative))
		if err != nil || info.IsDir() {
			return fail("config validate", fmt.Errorf("缺失或不可读：%s", relative))
		}
	}
	ctx, stop := signalContext()
	defer stop()
	compose := &opscompose.Compose{Root: root, Run: runner}
	if err := compose.Config(ctx); err != nil {
		return fail("config validate", err)
	}
	fmt.Println("配置校验通过")
	return 0
}

func configExport(args []string) int {
	var rootPath, output string
	flags := flag.NewFlagSet("config export", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录")
	flags.StringVar(&output, "output", "./goalgo-config.tar", "输出 tar 路径")
	if err := flags.Parse(args); err != nil {
		return fail("config export", err)
	}
	root, err := resolveRegisteredRoot(rootPath)
	if err != nil {
		return fail("config export", err)
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("config export", err)
	}
	defer lock.Release()
	if err := validateConfigExport(root); err != nil {
		return fail("config export", err)
	}
	if _, err := os.Stat(output); err == nil {
		return fail("config export", fmt.Errorf("目标已存在：%s", output))
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fail("config export", err)
	}
	writer := tar.NewWriter(file)
	for _, relative := range []string{".env", "release.env", "compose.yaml", "config", "secrets"} {
		source := root.Join(relative)
		if _, err := os.Lstat(source); err != nil {
			continue
		}
		if err := addTarEntry(writer, source, relative); err != nil {
			file.Close()
			os.Remove(output)
			return fail("config export", err)
		}
	}
	if err := writer.Close(); err != nil {
		file.Close()
		return fail("config export", err)
	}
	if err := file.Close(); err != nil {
		return fail("config export", err)
	}
	fmt.Printf("配置已导出：%s（含 .env / release.env / compose.yaml / config / secrets，请妥善保管）\n", output)
	return 0
}

func configImport(args []string) int {
	var rootPath, bundle string
	flags := flag.NewFlagSet("config import", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录")
	flags.StringVar(&bundle, "file", "", "配置 tar 路径")
	if err := flags.Parse(args); err != nil {
		return fail("config import", err)
	}
	if bundle == "" {
		prompt := opsprompt.New()
		if !prompt.TTY {
			return fail("config import", fmt.Errorf("必须提供 --file"))
		}
		var err error
		bundle, err = prompt.String("配置 tar 路径", "")
		if err != nil {
			return fail("config import", err)
		}
	}
	root, err := resolveRegisteredRoot(rootPath)
	if err != nil {
		return fail("config import", err)
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("config import", err)
	}
	defer lock.Release()
	if err := importConfigBundle(root, bundle); err != nil {
		return fail("config import", err)
	}
	fmt.Printf("配置已导入：%s（密钥目录保持 0600；建议重启服务生效）\n", root.Path)
	return 0
}

var criticalConfigEntries = []string{
	".env", "release.env", "compose.yaml", "config", "secrets/app.env", "secrets/jwt_private_key.pem",
	"secrets/jwt_public_key.pem", "secrets/postgres_password", "secrets/redis_password",
	"secrets/rabbitmq_password", "secrets/backup_encryption_key",
}

func validateStagedConfig(root *opsroot.Root) error {
	if err := validateConfigExport(root); err != nil {
		return err
	}
	if _, err := opsrelease.ParseFile(root.Join("release.env")); err != nil {
		return fmt.Errorf("release.env 无效: %w", err)
	}
	for _, relative := range []string{"compose.yaml", "config/gateway.yaml", "config/user.yaml", "config/core-data.yaml", "config/agent.yaml"} {
		raw, err := os.ReadFile(root.Join(relative))
		if err != nil {
			return fmt.Errorf("读取 %s: %w", relative, err)
		}
		var value any
		if err := yaml.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s YAML 无效: %w", relative, err)
		}
		if value == nil {
			return fmt.Errorf("%s 内容为空", relative)
		}
	}
	for _, relative := range []string{"secrets/app.env", "secrets/jwt_private_key.pem", "secrets/jwt_public_key.pem", "secrets/postgres_password", "secrets/redis_password", "secrets/rabbitmq_password", "secrets/backup_encryption_key"} {
		content, err := os.ReadFile(root.Join(relative))
		if err != nil || len(content) == 0 {
			return fmt.Errorf("关键密钥内容为空或不可读：%s", relative)
		}
	}
	key, _ := os.ReadFile(root.Join("secrets/backup_encryption_key"))
	if len(key) != 32 {
		return fmt.Errorf("backup_encryption_key 必须为 32 字节")
	}
	dsn, err := dotenvValue(root.Join("secrets/app.env"), "CWXU_BACKUP_PG_DSN")
	if err != nil {
		return err
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
		return fmt.Errorf("secrets/app.env 的 CWXU_BACKUP_PG_DSN 必须是有效 PostgreSQL URI")
	}
	return nil
}

func validateConfigExport(root *opsroot.Root) error {
	for _, relative := range criticalConfigEntries {
		if _, err := os.Stat(root.Join(relative)); err != nil {
			return fmt.Errorf("缺少关键配置：%s", relative)
		}
	}
	return nil
}

func importConfigBundle(root *opsroot.Root, bundle string) error {
	file, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer file.Close()
	stage, err := os.MkdirTemp(filepath.Dir(root.Path), ".goalgo-config-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := header.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("非法条目：%s", name)
		}
		target := filepath.Join(stage, filepath.Clean(name))
		if target != stage && !strings.HasPrefix(target, stage+string(os.PathSeparator)) {
			return fmt.Errorf("路径逃逸：%s", name)
		}
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("非法条目类型：%s", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(name, "config/") {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, reader); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	stageRoot, err := opsroot.Resolve(stage)
	if err != nil {
		return err
	}
	if err := validateStagedConfig(stageRoot); err != nil {
		return err
	}
	if value, err := dotenvValue(filepath.Join(stage, ".env"), "GOALGO_ROOT"); err != nil || filepath.Clean(value) != root.Path {
		return fmt.Errorf("导入 .env 的 GOALGO_ROOT 必须为 %s", root.Path)
	}
	backup, err := os.MkdirTemp(filepath.Dir(root.Path), ".goalgo-config-backup-")
	if err != nil {
		return err
	}
	entries := []string{".env", "release.env", "compose.yaml", "config", "secrets"}
	var replaced []string
	hadOriginal := map[string]bool{}
	rollback := func() error {
		var rollbackErr error
		for i := len(replaced) - 1; i >= 0; i-- {
			name := replaced[i]
			rollbackErr = errors.Join(rollbackErr, os.RemoveAll(root.Join(name)))
			if hadOriginal[name] {
				rollbackErr = errors.Join(rollbackErr, os.Rename(filepath.Join(backup, name), root.Join(name)))
			}
		}
		return rollbackErr
	}
	for _, name := range entries {
		target := root.Join(name)
		hadTarget := false
		if _, err := os.Stat(target); err == nil {
			if err := os.Rename(target, filepath.Join(backup, name)); err != nil {
				rollbackErr := rollback()
				if rollbackErr != nil {
					return errors.Join(err, fmt.Errorf("配置回滚失败，备份保留于 %s: %w", backup, rollbackErr))
				}
				return err
			}
			hadTarget = true
			hadOriginal[name] = true
		}
		if err := os.Rename(filepath.Join(stage, name), target); err != nil {
			var rollbackErr error
			if hadTarget {
				rollbackErr = errors.Join(rollbackErr, os.Rename(filepath.Join(backup, name), target))
			}
			rollbackErr = errors.Join(rollbackErr, rollback())
			if rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("配置回滚失败，备份保留于 %s: %w", backup, rollbackErr))
			}
			return err
		}
		replaced = append(replaced, name)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("配置已导入但清理旧备份失败（%s）: %w", backup, err)
	}
	return nil
}

func addTarEntry(writer *tar.Writer, source, relative string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		var name string
		if rel == "." {
			name = relative
		} else {
			name = filepath.Join(relative, rel)
		}
		name = filepath.ToSlash(name)
		if info.IsDir() {
			return writer.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o700, Typeflag: tar.TypeDir})
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = io.Copy(writer, file)
		return err
	})
}
