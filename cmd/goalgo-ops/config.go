package main

import (
	"archive/tar"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsexec"
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
	root, err := opsroot.Resolve(rootPath)
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
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
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
	for _, relative := range []string{".env", "release.env", "config", "secrets"} {
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
	fmt.Printf("配置已导出：%s（含 .env / release.env / config / secrets，请妥善保管）\n", output)
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
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
		return fail("config import", err)
	}
	file, err := os.Open(bundle)
	if err != nil {
		return fail("config import", err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail("config import", err)
		}
		name := header.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return fail("config import", fmt.Errorf("非法条目：%s", name))
		}
		target := root.Join(name)
		if !strings.HasPrefix(target, root.Path) {
			return fail("config import", fmt.Errorf("路径逃逸：%s", name))
		}
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fail("config import", err)
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fail("config import", fmt.Errorf("非法条目类型：%s", name))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fail("config import", err)
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(name, "config/") {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return fail("config import", err)
		}
		if _, err := io.Copy(out, reader); err != nil {
			out.Close()
			return fail("config import", err)
		}
		if err := out.Close(); err != nil {
			return fail("config import", err)
		}
	}
	fmt.Printf("配置已导入：%s（密钥目录保持 0600；建议重启服务生效）\n", root.Path)
	return 0
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
