package opsbackup

import (
	"context"
	"fmt"
	"io"

	"cwxu-algo/internal/opsexec"
)

// ContainerToolRunner 把 zstd / pg_restore 命令改为经 core-data 镜像容器执行
// （镜像内置 PostgreSQL 18 客户端与 zstd），避免宿主机额外安装客户端。
// WorkDir 以同路径挂载，容器内以 root 运行以读取 0700 的校验目录。
type ContainerToolRunner struct {
	Inner   opsexec.Command
	Image   string
	WorkDir string
}

func (r ContainerToolRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	switch name {
	case "zstd", "pg_restore":
		full := []string{"run", "--rm", "--user", "root",
			"-v", r.WorkDir + ":" + r.WorkDir,
			"--entrypoint", name, r.Image}
		full = append(full, args...)
		return r.Inner.CombinedOutput(ctx, "docker", full...)
	default:
		return r.Inner.CombinedOutput(ctx, name, args...)
	}
}

func (r ContainerToolRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	output, err := r.CombinedOutput(ctx, name, args...)
	if err != nil {
		if output != "" {
			return fmt.Errorf("%s %v：%w\n%s", name, args, err, output)
		}
		return err
	}
	return nil
}
