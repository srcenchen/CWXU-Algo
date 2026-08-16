package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

// requireBackupTools 检查宿主机备份/恢复必需命令：zstd 存在，pg_restore 存在且版本 >=18（备份由 PG18 生成）。
func requireBackupTools(ctx context.Context, runner opsexec.Command) error {
	if _, err := runner.CombinedOutput(ctx, "zstd", "--version"); err != nil {
		return fmt.Errorf("缺少 zstd：请安装（apt install -y zstd）")
	}
	if err := opsexec.RequireCommand(ctx, runner, "pg_restore"); err != nil {
		return fmt.Errorf("缺少 pg_restore：请安装（apt install -y postgresql-client-18）")
	}
	output, err := runner.CombinedOutput(ctx, "pg_restore", "--version")
	if err != nil {
		return fmt.Errorf("pg_restore --version：%w", err)
	}
	if !pgRestoreVersion18.MatchString(output) {
		return fmt.Errorf("pg_restore 版本过低：备份由 PostgreSQL 18 生成，请安装 postgresql-client-18（当前 %s）", strings.TrimSpace(output))
	}
	return nil
}

var pgRestoreVersion18 = regexp.MustCompile(`\(PostgreSQL\) 18\.`)

func cmdRestore(args []string, runner opsexec.Command) int {
	var rootPath, archive, keyFile string
	var replace, confirm bool
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录")
	flags.StringVar(&archive, "file", "", ".cwxubak 本地路径或 http(s) URL")
	flags.StringVar(&keyFile, "key-file", "", "32 字节加密密钥文件")
	flags.BoolVar(&replace, "replace", false, "覆盖已有数据")
	flags.BoolVar(&confirm, "confirm", false, "确认破坏性恢复（须同时 --replace 与 --confirm RESTORE）")
	if err := flags.Parse(args); err != nil {
		return fail("restore", err)
	}
	root, err := resolveInstallRoot(rootPath)
	if err != nil {
		return fail("restore", err)
	}
	prompt := opsprompt.New()
	if archive == "" || keyFile == "" || !confirm {
		if !prompt.TTY {
			if archive == "" {
				return fail("restore", fmt.Errorf("必须提供 --file（本地路径或 URL）"))
			}
			if keyFile == "" {
				return fail("restore", fmt.Errorf("必须提供 --key-file"))
			}
			if replace != confirm || !confirm {
				return fail("restore", fmt.Errorf("覆盖已有数据必须同时提供 --replace --confirm "+confirmToken))
			}
		} else {
			if archive == "" {
				archive, err = prompt.String("归档路径或 URL", "")
				if err != nil {
					return fail("restore", err)
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
	compose := &opscompose.Compose{Root: root, Run: runner}
	verifyDir := filepath.Join(root.Join("restore"), "verify-"+time.Now().Format("20060102T150405"))
	if err := os.MkdirAll(verifyDir, 0o700); err != nil {
		return fail("restore", err)
	}
	verifyRunner, ok := containerVerifyRunner(ctx, compose, runner, verifyDir)
	if !ok {
		if err := requireBackupTools(ctx, runner); err != nil {
			return fail("restore", err)
		}
		verifyRunner = runner
	}
	archivePath, downloaded, err := resolveArchive(ctx, root, archive)
	if err != nil {
		return fail("restore", err)
	}
	if downloaded {
		defer os.Remove(archivePath)
	}
	result, err := opsbackup.VerifyArchive(ctx, archivePath, key, verifyDir, verifyRunner)
	if err != nil {
		return fail("restore", fmt.Errorf("归档校验失败：%w", err))
	}
	fmt.Printf("归档校验通过：%d 个数据库：%s\n", len(result.Manifest.Databases), strings.Join(result.Manifest.Databases, ", "))

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
// containerVerifyRunner 优先用 core-data 镜像容器执行 zstd/pg_restore（镜像内置 PG18 客户端与 zstd），
// 免装宿主机客户端；拿不到镜像或 docker 不可用时返回 ok=false 由调用方回退宿主机工具。
func containerVerifyRunner(ctx context.Context, compose *opscompose.Compose, runner opsexec.Command, workDir string) (opsexec.Command, bool) {
	release, err := compose.Release()
	if err != nil {
		return nil, false
	}
	image := release.Images["CORE_DATA_IMAGE"]
	if image == "" {
		return nil, false
	}
	if err := opsexec.RequireCommand(ctx, runner, "docker"); err != nil {
		return nil, false
	}
	return opsbackup.ContainerToolRunner{Inner: runner, Image: image, WorkDir: workDir}, true
}

// resolveArchive 本地文件直接使用；http(s) URL 下载到 restore 目录，返回 downloaded=true。
func resolveArchive(ctx context.Context, root *opsroot.Root, source string) (path string, downloaded bool, err error) {
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		info, err := os.Stat(source)
		if err != nil {
			return "", false, fmt.Errorf("归档文件不存在：%s", source)
		}
		if info.IsDir() {
			return "", false, fmt.Errorf("归档路径是目录：%s", source)
		}
		return source, false, nil
	}
	path, err = downloadURL(ctx, root, source)
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

func downloadURL(ctx context.Context, root *opsroot.Root, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 %s：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("下载 %s：HTTP %d", url, resp.StatusCode)
	}
	dir := root.Join("restore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "download-"+time.Now().Format("20060102T150405")+".cwxubak")
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(dest)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return "", err
	}
	fmt.Printf("已下载归档：%s\n", dest)
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
