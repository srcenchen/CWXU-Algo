package opsbackup

import (
	"context"
	"fmt"
	"os"

	"cwxu-algo/app/common/cwxubak"
	"cwxu-algo/internal/opsexec"
)

// runnerAdapter 把 opsexec.Command 适配为 cwxubak.CmdRunner。
type runnerAdapter struct {
	inner opsexec.Command
}

func (r runnerAdapter) Run(ctx context.Context, name string, args ...string) error {
	output, err := r.inner.CombinedOutput(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %v：%w\n%s", name, args, err, output)
	}
	return nil
}

// VerifyArchive 完整离线校验 .cwxubak 并解压到 workDir。
func VerifyArchive(ctx context.Context, archivePath string, key []byte, workDir string, runner opsexec.Command) (*cwxubak.VerifyResult, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("密钥必须为 32 原始字节，当前 %d 字节", len(key))
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return cwxubak.Verify(ctx, file, key, workDir, runnerAdapter{inner: runner})
}
