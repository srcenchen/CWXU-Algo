package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cwxu-algo/internal/opsbackup"
	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opslock"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsroot"
)

const confirmToken = "RESTORE"

func cmdRestore(args []string, runner opsexec.Command) int {
	var rootPath, archive, keyFile string
	var replace, confirm bool
	var useLatest bool
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录")
	flags.StringVar(&archive, "file", "", ".cwxubak 归档路径（与 --latest 二选一）")
	flags.BoolVar(&useLatest, "latest", false, "从又拍云拉取最新归档")
	flags.StringVar(&keyFile, "key-file", "", "32 字节加密密钥文件")
	flags.BoolVar(&replace, "replace", false, "覆盖已有数据")
	flags.BoolVar(&confirm, "confirm", false, "确认破坏性恢复（须同时 --replace 与 --confirm RESTORE）")
	if err := flags.Parse(args); err != nil {
		return fail("restore", err)
	}
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
		return fail("restore", err)
	}
	prompt := opsprompt.New()
	if (archive == "" && !useLatest) || keyFile == "" || !confirm {
		if !prompt.TTY {
			if archive == "" && !useLatest {
				return fail("restore", fmt.Errorf("必须提供 --file 或 --latest"))
			}
			if keyFile == "" {
				return fail("restore", fmt.Errorf("必须提供 --key-file"))
			}
			if replace != confirm || !confirm {
				return fail("restore", fmt.Errorf("覆盖已有数据必须同时提供 --replace --confirm "+confirmToken))
			}
		} else {
			if archive == "" && !useLatest {
				chosen, err := promptArchiveSource(root, prompt)
				if err != nil {
					return fail("restore", err)
				}
				if chosen == "" {
					useLatest = true
				} else {
					archive = chosen
				}
			}
			if keyFile == "" {
				keyFile, err = prompt.String("备份加密密钥文件路径", "")
				if err != nil {
					return fail("restore", err)
				}
			}
			if !confirm {
				input, err := prompt.String(fmt.Sprintf("覆盖已有数据，请输入 %s 确认", confirmToken), "")
				if err != nil {
					return fail("restore", err)
				}
				if strings.TrimSpace(input) != confirmToken {
					return fail("restore", fmt.Errorf("确认令牌不匹配，已取消"))
				}
				confirm = true
				replace = true
			}
		}
	}
	if archive != "" && useLatest {
		return fail("restore", fmt.Errorf("--file 与 --latest 不能同时使用"))
	}
	key, err := readKeyFile(keyFile)
	if err != nil {
		return fail("restore", err)
	}
	ctx, stop := signalContext()
	defer stop()
	if root.IsProtectedInstall() && !opsroot.IsPrivileged() {
		return fail("restore", fmt.Errorf("恢复需要 root 权限"))
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("restore", err)
	}
	defer lock.Release()

	// 1. 校验归档（认证解密 + zstd + tar + manifest + pg_restore --list）。
	verifyDir := filepath.Join(root.Join("restore"), "verify-"+time.Now().Format("20060102T150405"))
	if err := os.MkdirAll(verifyDir, 0o700); err != nil {
		return fail("restore", err)
	}
	archivePath := archive
	if useLatest {
		archivePath, err = downloadLatest(ctx, root)
		if err != nil {
			return fail("restore", err)
		}
		defer os.Remove(archivePath)
	}
	result, err := opsbackup.VerifyArchive(ctx, archivePath, key, verifyDir, runner)
	if err != nil {
		return fail("restore", fmt.Errorf("归档校验失败：%w", err))
	}
	fmt.Printf("归档校验通过：%d 个数据库：%s\n", len(result.Manifest.Databases), strings.Join(result.Manifest.Databases, ", "))

	compose := &opscompose.Compose{Root: root, Run: runner}

	// 2. 停止应用服务与 postgres。
	for _, service := range []string{"frontend", "gateway", "user", "core-data", "agent", "nginx"} {
		if _, err := compose.Command(ctx, "stop", service); err != nil {
			return fail("restore", fmt.Errorf("停止 %s：%w", service, err))
		}
	}
	if _, err := compose.Command(ctx, "stop", "postgres"); err != nil {
		return fail("restore", err)
	}

	// 3. 换新数据卷（保留旧卷用于回滚）。
	dataDir := root.Join("data", "postgres")
	backupDir := dataDir + ".bak-" + time.Now().Format("20060102T150405")
	if _, err := os.Stat(dataDir); err == nil {
		if err := os.Rename(dataDir, backupDir); err != nil {
			return fail("restore", fmt.Errorf("备份旧数据卷：%w", err))
		}
		fmt.Printf("旧数据卷已保留：%s\n", backupDir)
	}
	rollback := func(previous string) error {
		_ = os.RemoveAll(dataDir)
		if previous != "" {
			return os.Rename(previous, dataDir)
		}
		return nil
	}

	// 4. 启动全新 postgres（initdb + 建库脚本自动执行）。
	if _, err := compose.Command(ctx, "up", "-d", "postgres"); err != nil {
		_ = rollback(backupDir)
		return fail("restore", fmt.Errorf("启动新 postgres：%w", err))
	}

	// 5. 恢复 globals.sql（角色已存在时 CREATE ROLE 冲突可忽略）。
	globalsContent, err := os.ReadFile(result.Globals)
	if err != nil {
		_, _ = compose.Command(ctx, "stop", "postgres")
		_ = rollback(backupDir)
		return fail("restore", err)
	}
	psqlBase := []string{"exec", "-T", "postgres", "sh", "-c",
		`PGPASSWORD="$(cat /run/secrets/postgres_password)" psql --dbname=postgres -q -v ON_ERROR_STOP=0`}
	if _, err := compose.CommandWithStdin(ctx, globalsContent, psqlBase...); err != nil {
		fmt.Fprintf(os.Stderr, "警告：globals 存在重复对象（可忽略）：%v\n", err)
	}

	// 6. 逐个恢复数据库：先 DROP 再 pg_restore --create（stdin 管道传入 dump）。
	for i, dump := range result.Dumps {
		database := result.Manifest.Databases[i]
		safe := sanitizeIdentifier(database)
		dropCmd := fmt.Sprintf("PGPASSWORD=\"$(cat /run/secrets/postgres_password)\" psql --dbname=postgres -c 'DROP DATABASE IF EXISTS %s'", safe)
		if _, err := compose.Command(ctx, "exec", "-T", "postgres", "sh", "-c", dropCmd); err != nil {
			_, _ = compose.Command(ctx, "stop", "postgres")
			_ = rollback(backupDir)
			return fail("restore", fmt.Errorf("删除数据库 %s：%w", database, err))
		}
		data, err := os.ReadFile(dump)
		if err != nil {
			_, _ = compose.Command(ctx, "stop", "postgres")
			_ = rollback(backupDir)
			return fail("restore", err)
		}
		restoreCmd := []string{"exec", "-T", "postgres", "sh", "-c",
			`PGPASSWORD="$(cat /run/secrets/postgres_password)" pg_restore --exit-on-error --create --dbname=postgres`}
		if _, err := compose.CommandWithStdin(ctx, data, restoreCmd...); err != nil {
			_, _ = compose.Command(ctx, "stop", "postgres")
			_ = rollback(backupDir)
			return fail("restore", fmt.Errorf("恢复数据库 %s：%w", database, err))
		}
		fmt.Printf("数据库 %s 恢复完成\n", database)
	}

	// 7. 启动全栈并冒烟。
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		_, _ = compose.Command(ctx, "stop", "postgres")
		_ = rollback(backupDir)
		return fail("restore", fmt.Errorf("启动服务失败，已回滚旧数据卷：%w", err))
	}
	if err := compose.Health(ctx); err != nil {
		return fail("restore", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		return fail("restore", err)
	}
	fmt.Printf("恢复成功。旧数据卷保留于 %s（确认无误后可删除）\n", backupDir)
	return 0
}

func promptArchiveSource(root *opsroot.Root, prompt *opsprompt.Prompter) (string, error) {
	var candidates []string
	for _, dir := range []string{root.Join("restore"), "."} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.cwxubak"))
		if err != nil {
			continue
		}
		candidates = append(candidates, matches...)
	}
	options := append(append([]string{}, candidates...), "从又拍云拉取最新归档")
	idx, err := prompt.Choice("选择归档来源", len(options)-1, options...)
	if err != nil {
		return "", err
	}
	if idx == len(candidates) {
		return "", nil
	}
	return candidates[idx], nil
}

func downloadLatest(ctx context.Context, root *opsroot.Root) (string, error) {	bucket := os.Getenv("UPYUN_BUCKET")
	operator := os.Getenv("UPYUN_OPERATOR")
	password := os.Getenv("UPYUN_PASSWORD")
	prefix := os.Getenv("UPYUN_PREFIX")
	if bucket == "" || operator == "" || password == "" {
		return "", fmt.Errorf("需要环境变量 UPYUN_BUCKET / UPYUN_OPERATOR / UPYUN_PASSWORD")
	}
	if prefix == "" {
		prefix = "backups/core"
	}
	store := opsbackup.NewUpyun(opsbackup.UpyunConfig{Bucket: bucket, Operator: operator, Password: password, Prefix: prefix})
	pointer, err := store.LatestPointer(ctx)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(root.Join("restore"), "latest-"+time.Now().Format("20060102T150405")+".cwxubak")
	if _, err := store.DownloadArchive(ctx, pointer, dest); err != nil {
		return "", err
	}
	fmt.Printf("已下载最新归档：%s（%s）\n", pointer.ArchiveKey, pointer.SHA256[:16]+"…")
	return dest, nil
}

func sanitizeIdentifier(name string) string {
	var builder strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			builder.WriteRune(r)
		}
	}
	value := builder.String()
	if value == "" {
		return "goalgo_restore"
	}
	return `"` + value + `"`
}
