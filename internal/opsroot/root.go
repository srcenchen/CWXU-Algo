package opsroot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Root struct {
	Path string
}

// DefaultPath 返回未指定 root 且无 GOALGO_ROOT 环境变量时的默认根目录。
func DefaultPath() string {
	if env := os.Getenv("GOALGO_ROOT"); env != "" {
		return env
	}
	return "/opt/goalgo"
}

// JoinDefault 返回默认根目录下拼接相对路径的结果。
func JoinDefault(parts ...string) string {
	return filepath.Join(append([]string{DefaultPath()}, parts...)...)
}

func Resolve(path string) (*Root, error) {
	if path == "" {
		path = os.Getenv("GOALGO_ROOT")
	}
	if path == "" {
		path = "/opt/goalgo"
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析根目录：%w", err)
	}
	if absolute == "/" {
		return nil, fmt.Errorf("根目录不能是文件系统根路径")
	}
	info, err := os.Lstat(absolute)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("根目录不能是符号链接：%s", absolute)
	}
	return &Root{Path: absolute}, nil
}

func (r *Root) Join(parts ...string) string {
	return filepath.Join(append([]string{r.Path}, parts...)...)
}

func (r *Root) EnsureLayout() error {
	for _, dir := range []string{"config", "secrets", "data", "state", "logs", "restore"} {
		if err := os.MkdirAll(r.Join(dir), 0o755); err != nil {
			return fmt.Errorf("创建 %s 目录：%w", dir, err)
		}
	}
	return os.Chmod(r.Join("secrets"), 0o700)
}

func (r *Root) RequireFiles() error {
	for _, relative := range []string{"compose.yaml", ".env", "release.env"} {
		path := r.Join(relative)
		if info, err := os.Stat(path); err != nil {
			return fmt.Errorf("缺少必需文件：%s", path)
		} else if info.IsDir() {
			return fmt.Errorf("必需文件是目录：%s", path)
		}
	}
	return nil
}

func IsPrivileged() bool {
	return os.Geteuid() == 0
}

func (r *Root) IsProtectedInstall() bool {
	return strings.HasPrefix(r.Path, "/opt/goalgo")
}

func (r *Root) IsInitialized() bool {
	info, err := os.Stat(r.Join("state/install.json"))
	return err == nil && !info.IsDir()
}
