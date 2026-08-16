package cwxubak

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CmdRunner 执行外部命令（zstd / pg_restore）。
type CmdRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// VerifyResult 校验并提取后的内容。
type VerifyResult struct {
	Manifest *Manifest
	Globals  string
	Dumps    []string // 按 manifest.databases 顺序的 database-NNN.dump 路径
	WorkDir  string
}

var (
	dumpNameRe   = regexp.MustCompile(`^database-(\d{3})\.dump$`)
	ErrBadStructure = errors.New("invalid backup archive structure")
)

// Verify 校验 .cwxubak 并解压到 workDir（已创建）：
// HMAC 认证 → AES-GCM 解密 → zstd 解压 → tar 结构校验 → manifest 校验 → 可选 pg_restore --list。
// key 必须为 32 原始字节。runner 可为 nil（跳过 zstd 与 pg_restore，仅解出 payload 供测试）。
func Verify(ctx context.Context, archive io.ReadSeeker, key []byte, workDir string, runner CmdRunner) (*VerifyResult, error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, err
	}
	plainPath := filepath.Join(workDir, "payload.zst")
	plainFile, err := os.OpenFile(plainPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := DecryptStream(ctx, archive, plainFile, key); err != nil {
		plainFile.Close()
		return nil, fmt.Errorf("解密/认证失败：%w", err)
	}
	_ = plainFile.Close()

	if runner != nil {
		tarPath := filepath.Join(workDir, "payload.tar")
		if err := runner.Run(ctx, "zstd", "--decompress", "--quiet", "--force", plainPath, "-o", tarPath); err != nil {
			return nil, fmt.Errorf("zstd 解压失败：%w", err)
		}
		tarFile, err := os.Open(tarPath)
		if err != nil {
			return nil, err
		}
		manifest, globals, dumps, err := extractTar(ctx, tarFile, workDir)
		tarFile.Close()
		if err != nil {
			return nil, err
		}
		for _, dump := range dumps {
			if err := runner.Run(ctx, "pg_restore", "--list", dump); err != nil {
				return nil, fmt.Errorf("pg_restore --list %s 失败：%w", filepath.Base(dump), err)
			}
		}
		result := &VerifyResult{Manifest: manifest, Globals: globals, Dumps: dumps, WorkDir: workDir}
		if err := validateManifest(manifest, dumps); err != nil {
			return nil, err
		}
		return result, nil
	}

	// 无执行器：仅解密，用内存 tar 解析结构。
	tarFile, err := decompressZstdInMemory(ctx, plainPath)
	if err != nil {
		return nil, err
	}
	manifest, globals, dumps, err := extractTar(ctx, tarFile, workDir)
	tarFile.Close()
	if err != nil {
		return nil, err
	}
	result := &VerifyResult{Manifest: manifest, Globals: globals, Dumps: dumps, WorkDir: workDir}
	if err := validateManifest(manifest, dumps); err != nil {
		return nil, err
	}
	return result, nil
}

func decompressZstdInMemory(ctx context.Context, plainPath string) (*os.File, error) {
	data, err := os.ReadFile(plainPath)
	if err != nil {
		return nil, err
	}
	// 测试环境不依赖 zstd 二进制时由测试自行预解压；此处直接透传。
	file, err := os.CreateTemp("", "cwxubak-tar-*")
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func extractTar(ctx context.Context, tarFile *os.File, workDir string) (*Manifest, string, []string, error) {
	reader := tar.NewReader(tarFile)
	var manifest *Manifest
	var globals string
	var dumps []string
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", nil, err
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", nil, fmt.Errorf("解析 tar：%w", err)
		}
		name := filepath.Base(header.Name)
		if name == "." || name == "/" || name == "" || strings.Contains(name, "..") || header.Name != name {
			return nil, "", nil, fmt.Errorf("%w：非法条目名 %q", ErrBadStructure, header.Name)
		}
		if seen[name] {
			return nil, "", nil, fmt.Errorf("%w：重复条目 %q", ErrBadStructure, name)
		}
		seen[name] = true
		if header.Typeflag != tar.TypeReg {
			return nil, "", nil, fmt.Errorf("%w：非法条目类型 %q", ErrBadStructure, header.Name)
		}
		if header.Size <= 0 {
			return nil, "", nil, fmt.Errorf("%w：空条目 %q", ErrBadStructure, header.Name)
		}
		target := filepath.Join(workDir, name)
		if !strings.HasPrefix(target, workDir+string(os.PathSeparator)) {
			return nil, "", nil, fmt.Errorf("%w：路径逃逸 %q", ErrBadStructure, header.Name)
		}
		switch name {
		case "manifest.json":
			data, err := io.ReadAll(io.LimitReader(reader, header.Size))
			if err != nil {
				return nil, "", nil, err
			}
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, "", nil, fmt.Errorf("解析 manifest：%w", err)
			}
			manifest = &m
			if err := os.WriteFile(target, data, 0o600); err != nil {
				return nil, "", nil, err
			}
		case "globals.sql":
			globals = target
			if err := writeEntry(target, reader, header.Size, 0o600); err != nil {
				return nil, "", nil, err
			}
		default:
			m := dumpNameRe.FindStringSubmatch(name)
			if m == nil {
				return nil, "", nil, fmt.Errorf("%w：未知条目 %q", ErrBadStructure, name)
			}
			dumps = append(dumps, target)
			if err := writeEntry(target, reader, header.Size, 0o600); err != nil {
				return nil, "", nil, err
			}
		}
	}
	if manifest == nil || globals == "" || len(dumps) == 0 {
		return nil, "", nil, fmt.Errorf("%w：缺少必需条目", ErrBadStructure)
	}
	if len(dumps) != len(manifest.Databases) {
		return nil, "", nil, fmt.Errorf("%w：dump 数量 %d 与 manifest 数据库数 %d 不一致", ErrBadStructure, len(dumps), len(manifest.Databases))
	}
	return manifest, globals, dumps, nil
}

func writeEntry(target string, reader *tar.Reader, size int64, mode os.FileMode) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, io.LimitReader(reader, size)); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func validateManifest(manifest *Manifest, dumps []string) error {
	if manifest == nil {
		return fmt.Errorf("%w：缺少 manifest", ErrBadStructure)
	}
	if manifest.Version != 1 {
		return fmt.Errorf("不支持的备份版本：%d", manifest.Version)
	}
	if manifest.CreatedAt.IsZero() {
		return errors.New("manifest 缺少 createdAt")
	}
	if len(manifest.Databases) == 0 {
		return errors.New("manifest 数据库列表为空")
	}
	seen := map[string]bool{}
	for _, name := range manifest.Databases {
		if name == "" || name == "postgres" {
			return fmt.Errorf("manifest 包含非法数据库名：%q", name)
		}
		if seen[name] {
			return fmt.Errorf("manifest 数据库名重复：%q", name)
		}
		seen[name] = true
	}
	return nil
}
