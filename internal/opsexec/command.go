package opsexec

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type Command interface {
	CombinedOutput(ctx context.Context, name string, args ...string) (string, error)
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

type Real struct{}

func (Real) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (Real) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
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
