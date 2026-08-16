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
)

func cmdBackup(args []string, runner opsexec.Command) int {
	if len(args) < 1 {
		return fail("backup", fmt.Errorf("缺少子命令：verify|download"))
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "verify":
		return backupVerify(rest, runner)
	case "download":
		return backupDownload(rest)
	default:
		return fail("backup", fmt.Errorf("未知子命令 %q（支持 verify|download）", sub))
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
	if archive == "" {
		return fail("backup verify", fmt.Errorf("必须提供 --file"))
	}
	if keyFile == "" {
		return fail("backup verify", fmt.Errorf("必须提供 --key-file"))
	}
	key, err := readKeyFile(keyFile)
	if err != nil {
		return fail("backup verify", err)
	}
	ctx, stop := signalContext()
	defer stop()
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

func backupDownload(args []string) int {
	var output, prefix string
	flags := flag.NewFlagSet("backup download", flag.ContinueOnError)
	flags.StringVar(&output, "output", "", "下载到本地的 .cwxubak 路径（默认 ./latest.cwxubak）")
	flags.StringVar(&prefix, "prefix", "", "又拍云归档前缀（默认 backups/core）")
	if err := flags.Parse(args); err != nil {
		return fail("backup download", err)
	}
	bucket := os.Getenv("UPYUN_BUCKET")
	operator := os.Getenv("UPYUN_OPERATOR")
	password := os.Getenv("UPYUN_PASSWORD")
	if bucket == "" || operator == "" || password == "" {
		return fail("backup download", fmt.Errorf("需要环境变量 UPYUN_BUCKET / UPYUN_OPERATOR / UPYUN_PASSWORD"))
	}
	if prefix == "" {
		prefix = "backups/core"
	}
	if output == "" {
		output = "./latest.cwxubak"
	}
	if _, err := os.Stat(output); err == nil {
		return fail("backup download", fmt.Errorf("目标已存在：%s", output))
	}
	store := opsbackup.NewUpyun(opsbackup.UpyunConfig{Bucket: bucket, Operator: operator, Password: password, Prefix: prefix})
	ctx, stop := signalContext()
	defer stop()
	pointer, err := store.LatestPointer(ctx)
	if err != nil {
		return fail("backup download", err)
	}
	hash, err := store.DownloadArchive(ctx, pointer, output)
	if err != nil {
		return fail("backup download", err)
	}
	fmt.Printf("下载完成：%s\n", output)
	fmt.Printf("归档：%s\nSHA-256：%s\n大小：%d 字节\n数据库：%d 个\n创建于：%s\n",
		pointer.ArchiveKey, hash, pointer.Size, pointer.Databases, pointer.CreatedAt.Format(time.RFC3339))
	return 0
}
