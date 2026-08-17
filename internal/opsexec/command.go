package opsexec

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Command interface {
	CombinedOutput(ctx context.Context, name string, args ...string) (string, error)
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

// Real 直接在本机执行命令。Env 非空时作为环境变量覆盖层注入子进程：
// 会先从继承环境里剔除同名键，再按 map 顺序写入，保证覆盖层优先于 shell 环境。
type Real struct {
	Env map[string]string
}

func (r Real) mergedEnv() []string {
	if len(r.Env) == 0 {
		return nil
	}
	env := os.Environ()
	for key, value := range r.Env {
		prefix := key + "="
		filtered := env[:0]
		for _, item := range env {
			if !strings.HasPrefix(item, prefix) {
				filtered = append(filtered, item)
			}
		}
		env = filtered
		env = append(env, prefix+value)
	}
	return env
}

func (r Real) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env := r.mergedEnv(); env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (r Real) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if env := r.mergedEnv(); env != nil {
		cmd.Env = env
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func Sanitize(secret, output string) string {
	if secret == "" {
		return output
	}
	return strings.ReplaceAll(output, secret, "<redacted>")
}

func ShowError(command Command, err error, output string) error {
	if err == nil {
		return nil
	}
	if output != "" {
		return fmt.Errorf("命令执行失败：%v\n输出：%s", err, strings.TrimSpace(output))
	}
	return err
}

func QuietRun(ctx context.Context, runner Command, name string, args ...string) error {
	output, err := runner.CombinedOutput(ctx, name, args...)
	if err != nil {
		if strings.TrimSpace(output) == "" {
			return fmt.Errorf("%s：%w", name, err)
		}
		return fmt.Errorf("%s：%w\n%s", name, err, strings.TrimSpace(output))
	}
	return nil
}

type Outputter interface {
	CombinedOutput(ctx context.Context, name string, args ...string) (string, error)
}

func RequireCommand(ctx context.Context, runner Outputter, name string) error {
	output, err := runner.CombinedOutput(ctx, name, "version")
	if err != nil {
		if strings.TrimSpace(output) == "" {
			return fmt.Errorf("缺少必需命令：%s", name)
		}
		return fmt.Errorf("缺少必需命令：%s：%s", name, strings.TrimSpace(output))
	}
	return nil
}
