package blogimg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

// ContentHash returns lowercase SHA-256 hex of raw bytes (uploaded/stored image).
func ContentHash(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NormalizeHash trims and lowercases a hex hash; empty if not 16–64 hex chars.
func NormalizeHash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) < 16 || len(h) > 64 {
		return ""
	}
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return h
}

// EncodeImageHashes serializes a unique ordered list of content hashes to JSON text.
func EncodeImageHashes(hashes []string) string {
	seen := map[string]struct{}{}
	var out []string
	for _, h := range hashes {
		h = NormalizeHash(h)
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	if len(out) == 0 {
		return "[]"
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// DecodeImageHashes parses JSON array or comma-separated hashes.
func DecodeImageHashes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		return uniqueNormalizedHashes(arr)
	}
	// fallback: comma / whitespace separated
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';'
	})
	return uniqueNormalizedHashes(parts)
}

func uniqueNormalizedHashes(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, h := range in {
		h = NormalizeHash(h)
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// hashAssetRow is a minimal projection for resolving keys → content hashes.
type hashAssetRow struct {
	ObjectKey   string `gorm:"column:object_key"`
	URL         string `gorm:"column:url"`
	ContentHash string `gorm:"column:content_hash"`
}

func (hashAssetRow) TableName() string { return "blog_image_assets" }

// ResolveContentHashes looks up blog_image_assets for image refs in content/cover
// and returns their ContentHash values (for article/page ImageHashes column).
func ResolveContentHashes(db *gorm.DB, userID uint, content, cover string) []string {
	if db == nil || userID == 0 {
		return nil
	}
	refs := ExtractImageURLs(content, cover)
	if len(refs) == 0 {
		// still scan bare /blog/ paths
		blob := content + "\n" + cover
		for _, m := range blogObjectPathRe.FindAllStringSubmatch(blob, -1) {
			if len(m) > 1 {
				refs = append(refs, m[1])
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}

	keys := make([]string, 0, len(refs))
	keySet := map[string]struct{}{}
	for _, r := range refs {
		k := BlogObjectKeyFromAnyURL(r)
		if k == "" {
			k = NormalizeObjectKey(r)
		}
		if k == "" || !strings.HasPrefix(strings.ToLower(k), "/blog/") {
			continue
		}
		if _, ok := keySet[k]; ok {
			continue
		}
		keySet[k] = struct{}{}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}

	var rows []hashAssetRow
	_ = db.Select("object_key", "url", "content_hash").
		Where("user_id = ? AND object_key IN ?", userID, keys).
		Find(&rows).Error

	var hashes []string
	foundKey := map[string]struct{}{}
	for _, r := range rows {
		if h := NormalizeHash(r.ContentHash); h != "" {
			hashes = append(hashes, h)
		}
		if k := NormalizeObjectKey(r.ObjectKey); k != "" {
			foundKey[k] = struct{}{}
		}
	}
	// keys not in assets: try filename stem as hash (content-addressed object keys)
	for _, k := range keys {
		if _, ok := foundKey[k]; ok {
			continue
		}
		if h := HashFromObjectKey(k); h != "" {
			hashes = append(hashes, h)
		}
	}
	return uniqueNormalizedHashes(hashes)
}

// HashFromObjectKey extracts a content hash embedded in object key
// forms: /blog/{uid}/{64hex}.ext or /blog/{uid}/{32+hex}.ext
func HashFromObjectKey(objectKey string) string {
	k := NormalizeObjectKey(objectKey)
	if k == "" {
		return ""
	}
	base := k
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// strip extension
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	// date_random legacy: 20260730_16hex — not a content hash
	if i := strings.IndexByte(base, '_'); i > 0 {
		// prefer full segment after date_ if long enough hex
		tail := base[i+1:]
		if h := NormalizeHash(tail); len(h) >= 32 {
			return h
		}
		return ""
	}
	if h := NormalizeHash(base); len(h) >= 32 {
		return h
	}
	return ""
}

// ObjectKeyForHash builds a content-addressed UpYun key for a user image.
// Format: /blog/{userID}/{sha256hex}{ext}
func ObjectKeyForHash(userID uint, contentHash, ext string) string {
	h := NormalizeHash(contentHash)
	if userID == 0 || h == "" {
		return ""
	}
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if ext == "" {
		ext = ".bin"
	}
	return NormalizeObjectKey("/blog/" + itoaUint(userID) + "/" + h + ext)
}

func itoaUint(n uint) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
