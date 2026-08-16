// Package cwxubak 定义 GoAlgo PostgreSQL 全量备份的字节格式：
// 魔数 CWXUBAK1 + 随机 nonce 前缀 + AES-256-GCM 分块记录 + 全文件 HMAC-SHA256。
// 明文载荷为 zstd(tar(manifest.json, globals.sql, database-NNN.dump))。
package cwxubak

import (
	"crypto/sha256"
	"encoding/json"
	"time"
)

// Magic 备份文件开头 8 字节魔数。
var Magic = []byte("CWXUBAK1")

// ChunkSize 单个 AES-GCM 记录的明文上限。
const ChunkSize = 1024 * 1024

// HMACKey 由 32 字节加密密钥派生 HMAC 密钥。
func HMACKey(key []byte) []byte {
	sum := sha256.Sum256(append(append([]byte(nil), key...), []byte("CWXU backup HMAC v1")...))
	return sum[:]
}

// Manifest 归档内的清单。
type Manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	Databases []string  `json:"databases"`
}

// Pointer 远端 latest.json 指针，与 core_data 备份 Result 一致。
type Pointer struct {
	ArchiveKey string    `json:"archiveKey"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Databases  int       `json:"databases"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ParsePointer 解析 latest.json。
func ParsePointer(data []byte) (*Pointer, error) {
	var pointer Pointer
	if err := json.Unmarshal(data, &pointer); err != nil {
		return nil, err
	}
	if pointer.ArchiveKey == "" || pointer.SHA256 == "" || pointer.Size <= 0 {
		return nil, ErrInvalidPointer
	}
	return &pointer, nil
}
