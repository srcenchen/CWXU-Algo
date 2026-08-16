// Package cwxubak 定义 GoAlgo PostgreSQL 全量备份的字节格式：
// 魔数 CWXUBAK1 + 随机 nonce 前缀 + AES-256-GCM 分块记录 + 全文件 HMAC-SHA256。
// 明文载荷为 zstd(tar(manifest.json, globals.sql, database-NNN.dump))。
package cwxubak

import (
	"crypto/sha256"
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
