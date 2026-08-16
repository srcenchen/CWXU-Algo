package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cwxu-algo/internal/opsbackup"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opsprompt"
)

func cmdBackup(args []string, runner opsexec.Command) int {
	if len(args) < 1 {
		return fail("backup", fmt.Errorf("缺少子命令：verify"))
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "verify":
		return backupVerify(rest, runner)
	default:
		return fail("backup", fmt.Errorf("未知子命令 %q（支持 verify）", sub))
	}
}

func readKeyFile(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("密钥文件 %s 必须为 32 原始字节，当前 %d 字节", path, len(key))
	}
	return key, nil
}

func backupVerify(args []string, runner opsexec.Command) int {
	var archive, keyFile, outDir string
	flags := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	flags.StringVar(&archive, "file", "", ".cwxubak 归档路径")
	flags.StringVar(&keyFile, "key-file", "", "32 字节加密密钥文件")
	flags.StringVar(&outDir, "out", "", "解压输出目录（默认临时目录）")
	if err := flags.Parse(args); err != nil {
		return fail("backup verify", err)
	}
	prompt := opsprompt.New()
	if archive == "" || keyFile == "" {
		if !prompt.TTY {
			if archive == "" {
				return fail("backup verify", fmt.Errorf("必须提供 --file"))
			}
			return fail("backup verify", fmt.Errorf("必须提供 --key-file"))
		}
		var err error
		if archive == "" {
			archive, err = prompt.String(".cwxubak 归档路径", "")
			if err != nil {
				return fail("backup verify", err)
			}
		}
		if keyFile == "" {
			keyFile, err = prompt.String("32 字节加密密钥文件路径", "")
			if err != nil {
				return fail("backup verify", err)
			}
		}
	}
	key, err := readKeyFile(keyFile)
	if err != nil {
		return fail("backup verify", err)
	}
	ctx, stop := signalContext()
	defer stop()
	if err := requireBackupTools(ctx, runner); err != nil {
		return fail("backup verify", err)
	}
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "goalgo-verify-"+time.Now().Format("20060102T150405"))
	}
	result, err := opsbackup.VerifyArchive(ctx, archive, key, outDir, runner)
	if err != nil {
		return fail("backup verify", err)
	}
	fmt.Printf("校验通过：归档创建于 %s，数据库 %d 个：%s\n", result.Manifest.CreatedAt.Format(time.RFC3339), len(result.Manifest.Databases), strings.Join(result.Manifest.Databases, ", "))
	fmt.Printf("解压目录：%s\n", result.WorkDir)
	return 0
}
