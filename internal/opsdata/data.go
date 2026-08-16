package opsdata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const schemaVersion = 1

const defaultPath = "/var/lib/goalgo-ops/ops.data.json"

type Data struct {
	Version int    `json:"version"`
	Root    string `json:"root"`
	Deploy  Deploy `json:"deploy"`
}

type Deploy struct {
	LastDigests map[string]string `json:"last_digests"`
	UpdatedAt   string            `json:"updated_at"`
}

// Path 返回系统级注册文件位置；测试和非特权环境可显式覆盖。
func Path() string {
	if override := os.Getenv("GOALGO_OPS_DATA_FILE"); override != "" {
		return override
	}
	return defaultPath
}

// Load 读取持久化数据；文件不存在时返回空结构。
func Load() (*Data, error) {
	data := &Data{Version: schemaVersion}
	path := Path()
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return migrateLegacy(data)
		}
		return nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	if err := json.Unmarshal(content, data); err != nil {
		return nil, fmt.Errorf("解析 %s：%w", path, err)
	}
	if data.Version == 0 {
		data.Version = schemaVersion
	}
	data.Root = canonicalRoot(data.Root)
	return data, nil
}

func migrateLegacy(empty *Data) (*Data, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return empty, nil
	}
	legacyPath := filepath.Join(home, ".ops.data.json")
	content, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, nil
		}
		return nil, fmt.Errorf("读取 %s：%w", legacyPath, err)
	}
	if err := json.Unmarshal(content, empty); err != nil {
		return nil, fmt.Errorf("解析 %s：%w", legacyPath, err)
	}
	empty.Root = canonicalRoot(empty.Root)
	if err := empty.Save(); err != nil {
		return nil, fmt.Errorf("迁移 %s：%w", legacyPath, err)
	}
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("删除旧注册 %s：%w", legacyPath, err)
	}
	return empty, nil
}

// Save 原子写入持久化文件（0600）。
func (d *Data) Save() error {
	if d.Version == 0 {
		d.Version = schemaVersion
	}
	d.Root = canonicalRoot(d.Root)
	content, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 %s：%w", filepath.Dir(path), err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return fmt.Errorf("写入 %s：%w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("保存 %s：%w", path, err)
	}
	return nil
}

func canonicalRoot(root string) string {
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}
