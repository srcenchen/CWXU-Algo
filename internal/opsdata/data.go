package opsdata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const schemaVersion = 1

type Data struct {
	Version int     `json:"version"`
	Deploy  Deploy  `json:"deploy"`
	Webhook Webhook `json:"webhook"`
}

type Deploy struct {
	LastDigests map[string]string `json:"last_digests"`
	UpdatedAt   string            `json:"updated_at"`
}

type Webhook struct {
	Key            string   `json:"key"`
	Port           int      `json:"port"`
	Bind           string   `json:"bind"`
	EnabledActions []string `json:"enabled_actions"`
	Calls          []Call   `json:"calls"`
}

type Call struct {
	Time   string `json:"time"`
	Action string `json:"action"`
	IP     string `json:"ip"`
	Status string `json:"status"`
}

// Path 返回持久化文件位置（$HOME/.ops.data.json）。
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ops.data.json"
	}
	return filepath.Join(home, ".ops.data.json")
}

// Load 读取持久化数据；文件不存在时返回空结构。
func Load() (*Data, error) {
	data := &Data{Version: schemaVersion}
	path := Path()
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return data, nil
		}
		return nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	if err := json.Unmarshal(content, data); err != nil {
		return nil, fmt.Errorf("解析 %s：%w", path, err)
	}
	if data.Version == 0 {
		data.Version = schemaVersion
	}
	return data, nil
}

// Save 原子写入持久化文件（0600）。
func (d *Data) Save() error {
	if d.Version == 0 {
		d.Version = schemaVersion
	}
	content, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	path := Path()
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
