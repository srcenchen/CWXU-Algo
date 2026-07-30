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
	UserID      uint   `gorm:"column:user_id"`
	ObjectKey   string `gorm:"column:object_key"`
	URL         string `gorm:"column:url"`
	ContentHash string `gorm:"column:content_hash"`
	Status      string `gorm:"column:status"`
}

// ContentHashInput is one content record whose referenced image hashes need resolving.
type ContentHashInput struct {
	ID      uint
	UserID  uint
	Content string
	Cover   string
}

const contentHashAssetKeyBatchSize = 400

// ResolveContentHashesBatchChecked resolves a batch with one asset-table query.
func ResolveContentHashesBatchChecked(db *gorm.DB, inputs []ContentHashInput) (map[uint][]string, error) {
	result := make(map[uint][]string, len(inputs))
	if db == nil || len(inputs) == 0 {
		return result, nil
	}
	keysByID := make(map[uint][]string, len(inputs))
	users := make([]uint, 0, len(inputs))
	allKeys := make([]string, 0)
	seenUsers := map[uint]struct{}{}
	seenKeys := map[string]struct{}{}
	userByID := make(map[uint]uint, len(inputs))
	for _, input := range inputs {
		if input.ID == 0 || input.UserID == 0 {
			continue
		}
		userByID[input.ID] = input.UserID
		if _, ok := seenUsers[input.UserID]; !ok {
			seenUsers[input.UserID] = struct{}{}
			users = append(users, input.UserID)
		}
		refs := ExtractImageURLs(input.Content, input.Cover)
		if len(refs) == 0 {
			for _, match := range blogObjectPathRe.FindAllStringSubmatch(input.Content+"\n"+input.Cover, -1) {
				if len(match) > 1 {
					refs = append(refs, match[1])
				}
			}
		}
		localSeen := map[string]struct{}{}
		for _, ref := range refs {
			key := BlogObjectKeyFromAnyURL(ref)
			if key == "" {
				key = NormalizeObjectKey(ref)
			}
			if key == "" || !strings.HasPrefix(strings.ToLower(key), "/blog/") {
				continue
			}
			if _, ok := localSeen[key]; ok {
				continue
			}
			localSeen[key] = struct{}{}
			keysByID[input.ID] = append(keysByID[input.ID], key)
			if _, ok := seenKeys[key]; !ok {
				seenKeys[key] = struct{}{}
				allKeys = append(allKeys, key)
			}
		}
	}
	if len(users) == 0 || len(allKeys) == 0 {
		return result, nil
	}
	rows := make([]hashAssetRow, 0)
	for start := 0; start < len(allKeys); start += contentHashAssetKeyBatchSize {
		end := start + contentHashAssetKeyBatchSize
		if end > len(allKeys) {
			end = len(allKeys)
		}
		var chunk []hashAssetRow
		if err := db.Select("user_id", "object_key", "url", "content_hash", "status").
			Where("user_id IN ? AND object_key IN ? AND COALESCE(NULLIF(status, ''), 'ready') = 'ready'", users, allKeys[start:end]).Find(&chunk).Error; err != nil {
			return nil, err
		}
		rows = append(rows, chunk...)
	}
	hashByUserKey := make(map[uint]map[string]string)
	for _, row := range rows {
		key := NormalizeObjectKey(row.ObjectKey)
		hash := NormalizeHash(row.ContentHash)
		if key == "" || hash == "" {
			continue
		}
		if hashByUserKey[row.UserID] == nil {
			hashByUserKey[row.UserID] = map[string]string{}
		}
		hashByUserKey[row.UserID][key] = hash
	}
	for id, keys := range keysByID {
		for _, key := range keys {
			hash := hashByUserKey[userByID[id]][key]
			if hash == "" {
				hash = HashFromObjectKey(key)
			}
			if hash != "" {
				result[id] = append(result[id], hash)
			}
		}
		result[id] = uniqueNormalizedHashes(result[id])
	}
	return result, nil
}

func (hashAssetRow) TableName() string { return "blog_image_assets" }

// ResolveContentHashes looks up blog_image_assets for image refs in content/cover
// and returns their ContentHash values (for article/page ImageHashes column).
func ResolveContentHashes(db *gorm.DB, userID uint, content, cover string) []string {
	hashes, _ := ResolveContentHashesChecked(db, userID, content, cover)
	return hashes
}

// ResolveContentHashesChecked is the migration-safe variant that reports DB failures.
func ResolveContentHashesChecked(db *gorm.DB, userID uint, content, cover string) ([]string, error) {
	if db == nil || userID == 0 {
		return nil, nil
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
		return nil, nil
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
		return nil, nil
	}

	var rows []hashAssetRow
	if err := db.Select("object_key", "url", "content_hash", "status").
		Where("user_id = ? AND object_key IN ? AND COALESCE(NULLIF(status, ''), 'ready') = 'ready'", userID, keys).
		Find(&rows).Error; err != nil {
		return nil, err
	}

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
	return uniqueNormalizedHashes(hashes), nil
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
