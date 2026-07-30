package blogimg

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cwxu-algo/app/common/upyun"
	secretutil "cwxu-algo/app/common/utils/secret"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// GCGracePeriod：刚上传、可能还在编辑中的图暂不 GC。
// 拉长到 24h，避免草稿/插件分步推送被误删。
const GCGracePeriod = 24 * time.Hour

var (
	ErrGCPreviewRequired = errors.New("blog image gc preview required")
	ErrGCStaleSnapshot   = errors.New("blog image gc preview is stale")
	ErrGCReferenceQuery  = errors.New("blog image gc reference query failed")
)

// ObjectDeleter is the UpYun delete surface used by GC (injectable in tests).
type ObjectDeleter interface {
	Delete(objectKey string) error
	Configured() bool
	PublicBaseURL() string
}

// imageAssetRow mirrors blog_image_assets without importing user models.
type imageAssetRow struct {
	ID          uint       `gorm:"primaryKey"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
	UserID      uint       `gorm:"column:user_id"`
	ObjectKey   string     `gorm:"column:object_key"`
	URL         string     `gorm:"column:url"`
	ContentHash string     `gorm:"column:content_hash"`
	Status      string     `gorm:"column:status"`
	ReservedAt  *time.Time `gorm:"column:reserved_at"`
}

func (imageAssetRow) TableName() string { return "blog_image_assets" }

// OrphanAsset is a registered image no longer referenced by any article/page.
// Protected marks a newly uploaded asset still inside the editing grace period.
type OrphanAsset struct {
	ID          uint       `json:"id"`
	CreatedAt   time.Time  `json:"createdAt"`
	ObjectKey   string     `json:"objectKey"`
	URL         string     `json:"url"`
	ContentHash string     `json:"contentHash,omitempty"`
	Status      string     `json:"status,omitempty"`
	ReservedAt  *time.Time `json:"reservedAt,omitempty"`
	Protected   bool       `json:"protected"`
}

// articleRefRow is enough content to compute referenced image keys / hashes.
type articleRefRow struct {
	ID          uint   `gorm:"primaryKey"`
	UserID      uint   `gorm:"column:user_id;index"`
	Content     string `gorm:"column:content"`
	CoverURL    string `gorm:"column:cover_url"`
	ImageHashes string `gorm:"column:image_hashes"`
}

func (articleRefRow) TableName() string { return "blog_articles" }

// pageRefRow mirrors blog_pages for GC (pages were previously ignored → 误删).
type pageRefRow struct {
	ID          uint   `gorm:"primaryKey"`
	UserID      uint   `gorm:"column:user_id;index"`
	ContentMD   string `gorm:"column:content_md"`
	ImageHashes string `gorm:"column:image_hashes"`
}

func (pageRefRow) TableName() string { return "blog_pages" }

// siteUpyunRow reads又拍云 fields from site_configs.
type siteUpyunRow struct {
	ID            uint   `gorm:"primaryKey"`
	UpyunBucket   string `gorm:"column:upyun_bucket"`
	UpyunOperator string `gorm:"column:upyun_operator"`
	UpyunPassword string `gorm:"column:upyun_password"`
	UpyunDomain   string `gorm:"column:upyun_domain"`
	UpyunScheme   string `gorm:"column:upyun_scheme"`
}

func (siteUpyunRow) TableName() string { return "site_configs" }

// LoadUpyunClient builds an UpYun client from site_configs id=1.
func LoadUpyunClient(db *gorm.DB) *upyun.Client {
	if db == nil {
		return upyun.New(upyun.Config{})
	}
	var row siteUpyunRow
	if err := db.First(&row, 1).Error; err != nil {
		return upyun.New(upyun.Config{})
	}
	pass, err := secretutil.Decrypt(row.UpyunPassword)
	if err != nil {
		pass = ""
	}
	return upyun.New(upyun.Config{
		Bucket:   strings.TrimSpace(row.UpyunBucket),
		Operator: strings.TrimSpace(row.UpyunOperator),
		Password: pass,
		Domain:   strings.TrimSpace(row.UpyunDomain),
		Scheme:   strings.TrimSpace(row.UpyunScheme),
	})
}

// gcUserImages deletes registered UpYun objects for user that are no longer
// referenced by any of their blog_articles / blog_pages.
//
// 引用判定（优先 hash，防 URL/域名/path 漂移误删）：
//  1. 文章/页面 ImageHashes 列（写入时由正文图解析并落库）
//  2. 正文/头图 object key 扫描（KeysFromContent + bare /blog/…）
//  3. AssetReferenced 子串兜底
//  4. 宽限期 GCGracePeriod 内新上传不删
//  5. 无 ContentHash 的历史资产：仅当 key/正文都无引用才删
//
// Returns number of orphan keys processed (delete attempted).
func gcUserImages(db *gorm.DB, client ObjectDeleter, userID uint) int {
	return gcUserImagesAt(db, client, userID, time.Now())
}

func gcUserImagesAt(db *gorm.DB, client ObjectDeleter, userID uint, now time.Time) int {
	n, _ := gcUserImagesChecked(db, client, userID, now, false)
	return n
}

func gcUserImagesChecked(db *gorm.DB, client ObjectDeleter, userID uint, now time.Time, force bool) (int, error) {
	if db == nil || userID == 0 || client == nil || !client.Configured() {
		return 0, fmt.Errorf("blog image gc is not configured")
	}
	base := client.PublicBaseURL()
	if strings.TrimSpace(base) == "" {
		return 0, fmt.Errorf("blog image gc public base is empty")
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, base, now)
	if err != nil {
		return 0, err
	}
	return deleteOrphanAssets(db, client, userID, orphans, force)
}

func deleteOrphanAssets(db *gorm.DB, client ObjectDeleter, userID uint, orphans []OrphanAsset, force bool) (int, error) {
	processed := 0
	for _, asset := range orphans {
		if asset.Protected && !force {
			continue
		}
		if err := client.Delete(asset.ObjectKey); err != nil {
			log.Warnf("blog image gc delete %s: %v", asset.ObjectKey, err)
			return processed, fmt.Errorf("delete remote object %s: %w", asset.ObjectKey, err)
		}
		res := db.Where("id = ? AND user_id = ?", asset.ID, userID).Delete(&imageAssetRow{})
		if res.Error != nil {
			return processed, fmt.Errorf("delete asset row %d: %w", asset.ID, res.Error)
		}
		if res.RowsAffected != 1 {
			return processed, fmt.Errorf("delete asset row %d: affected %d", asset.ID, res.RowsAffected)
		}
		processed++
	}
	return processed, nil
}

// BuildOrphanSnapshot produces a deterministic candidate list and snapshot.
func BuildOrphanSnapshot(userID uint, orphans []OrphanAsset) ([]uint, string) {
	ordered := append([]OrphanAsset(nil), orphans...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "user:%d\n", userID)
	ids := make([]uint, 0, len(ordered))
	for _, asset := range ordered {
		ids = append(ids, asset.ID)
		reservedUnix := int64(0)
		if asset.ReservedAt != nil {
			reservedUnix = asset.ReservedAt.UTC().UnixNano()
		}
		_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%t\n",
			asset.ID, asset.ObjectKey, asset.URL, asset.ContentHash, asset.Status, asset.CreatedAt.UTC().UnixNano(), reservedUnix, asset.Protected)
	}
	return ids, hex.EncodeToString(h.Sum(nil))
}

// GCUserImagesSnapshot deletes only when confirmation exactly matches a fresh full preview.
func GCUserImagesSnapshot(db *gorm.DB, client ObjectDeleter, userID uint, candidateIDs []uint, snapshot string) (int, error) {
	if len(candidateIDs) == 0 || strings.TrimSpace(snapshot) == "" {
		return 0, ErrGCPreviewRequired
	}
	if db == nil || userID == 0 || client == nil || !client.Configured() || strings.TrimSpace(client.PublicBaseURL()) == "" {
		return 0, fmt.Errorf("blog image gc is not configured")
	}
	deleted := 0
	err := WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
		orphans, err := ListUserImageOrphansCheckedAt(tx, userID, client.PublicBaseURL(), time.Now())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrGCReferenceQuery, err)
		}
		currentIDs, currentSnapshot := BuildOrphanSnapshot(userID, orphans)
		wantIDs := append([]uint(nil), candidateIDs...)
		sort.Slice(wantIDs, func(i, j int) bool { return wantIDs[i] < wantIDs[j] })
		if len(currentIDs) != len(wantIDs) {
			return ErrGCStaleSnapshot
		}
		for i := range currentIDs {
			if currentIDs[i] != wantIDs[i] {
				return ErrGCStaleSnapshot
			}
		}
		if subtle.ConstantTimeCompare([]byte(currentSnapshot), []byte(strings.TrimSpace(snapshot))) != 1 {
			return ErrGCStaleSnapshot
		}
		deleted, err = deleteOrphanAssets(tx, client, userID, orphans, true)
		return err
	})
	return deleted, err
}

// ListUserImageOrphans lists all unreferenced assets, including fresh uploads.
// Fresh uploads are marked Protected so the UI can warn before force cleanup.
func ListUserImageOrphans(db *gorm.DB, userID uint, base string) []OrphanAsset {
	return ListUserImageOrphansAt(db, userID, base, time.Now())
}

// ListUserImageOrphansAt is ListUserImageOrphans with an injectable clock.
func ListUserImageOrphansAt(db *gorm.DB, userID uint, base string, now time.Time) []OrphanAsset {
	orphans, _ := ListUserImageOrphansCheckedAt(db, userID, base, now)
	return orphans
}

// ListUserImageOrphansCheckedAt aborts on any reference/asset query failure.
func ListUserImageOrphansCheckedAt(db *gorm.DB, userID uint, base string, now time.Time) ([]OrphanAsset, error) {
	if db == nil || userID == 0 {
		return nil, fmt.Errorf("invalid blog image gc arguments")
	}

	usedHash := map[string]struct{}{}
	usedKey := map[string]struct{}{}
	var texts []string

	addHashes := func(raw string) {
		for _, h := range DecodeImageHashes(raw) {
			usedHash[h] = struct{}{}
		}
	}
	addContent := func(content, cover string) {
		texts = append(texts, content, cover)
		for k := range KeysFromContent(content, cover, base) {
			usedKey[k] = struct{}{}
			if h := HashFromObjectKey(k); h != "" {
				usedHash[h] = struct{}{}
			}
		}
	}

	var articles []articleRefRow
	if err := db.Select("content", "cover_url", "image_hashes").Where("user_id = ?", userID).Find(&articles).Error; err != nil {
		return nil, fmt.Errorf("query article references: %w", err)
	}
	for _, a := range articles {
		addHashes(a.ImageHashes)
		addContent(a.Content, a.CoverURL)
	}

	// 自定义页面此前未纳入 GC 引用 → 正文有图仍被删
	var pages []pageRefRow
	if err := db.Select("content_md", "image_hashes").Where("user_id = ?", userID).Find(&pages).Error; err != nil {
		return nil, fmt.Errorf("query page references: %w", err)
	}
	for _, p := range pages {
		addHashes(p.ImageHashes)
		addContent(p.ContentMD, "")
	}

	var assets []imageAssetRow
	if err := db.Where("user_id = ?", userID).Find(&assets).Error; err != nil {
		return nil, fmt.Errorf("query image assets: %w", err)
	}
	var orphans []OrphanAsset
	for _, a := range assets {
		key := NormalizeObjectKey(a.ObjectKey)
		if key == "" {
			continue
		}
		h := NormalizeHash(a.ContentHash)
		if h == "" {
			h = HashFromObjectKey(key)
		}
		status := strings.ToLower(strings.TrimSpace(a.Status))
		if status == "" {
			status = "ready"
		}
		if a.ReservedAt != nil && now.Sub(*a.ReservedAt) < GCGracePeriod {
			continue
		}
		if status == "pending" && a.ReservedAt == nil && !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) < GCGracePeriod {
			continue
		}
		// 主路径：content hash 仍被任一文/页声明或可从正文 key 推出
		if h != "" {
			if _, ok := usedHash[h]; ok {
				continue
			}
		}
		if _, ok := usedKey[key]; ok {
			continue
		}
		if AssetReferenced(key, a.URL, texts...) {
			continue
		}
		orphans = append(orphans, OrphanAsset{
			ID:          a.ID,
			CreatedAt:   a.CreatedAt,
			ObjectKey:   key,
			URL:         a.URL,
			ContentHash: h,
			Status:      status,
			ReservedAt:  a.ReservedAt,
			Protected:   status == "ready" && !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) < GCGracePeriod,
		})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].ID < orphans[j].ID })
	return orphans, nil
}

// ExistingURLsForUser returns which of the given URLs are still registered
// as blog_image_assets for this user (batch, no N+1).
// Also accepts bare object keys.
func ExistingURLsForUser(db *gorm.DB, userID uint, urls []string) (existing, missing []string) {
	ex, miss, _, _ := ExistingURLsAndHashesForUser(db, userID, urls, nil)
	return ex, miss
}

// ExistingURLsAndHashesForUser batch-checks URLs/object keys and content hashes.
func ExistingURLsAndHashesForUser(
	db *gorm.DB,
	userID uint,
	urls []string,
	hashes []string,
) (existingURLs, missingURLs, existingHashes, missingHashes []string) {
	if db == nil || userID == 0 {
		return nil, append([]string{}, urls...), nil, append([]string{}, hashes...)
	}

	// —— URLs / keys ——
	type item struct {
		raw string
		key string
	}
	var items []item
	seenRaw := map[string]struct{}{}
	var keys []string
	keySet := map[string]struct{}{}
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, ok := seenRaw[raw]; ok {
			continue
		}
		seenRaw[raw] = struct{}{}
		k := BlogObjectKeyFromAnyURL(raw)
		if k == "" {
			k = NormalizeObjectKey(raw)
		}
		items = append(items, item{raw: raw, key: k})
		if k != "" {
			if _, ok := keySet[k]; !ok {
				keySet[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}

	aliveKey := map[string]struct{}{}
	aliveURL := map[string]struct{}{}
	aliveHash := map[string]struct{}{}

	if len(keys) > 0 {
		var rows []imageAssetRow
		_ = db.Select("object_key", "url", "content_hash").
			Where("user_id = ? AND object_key IN ? AND COALESCE(NULLIF(status, ''), 'ready') = 'ready'", userID, keys).
			Find(&rows).Error
		for _, r := range rows {
			if k := NormalizeObjectKey(r.ObjectKey); k != "" {
				aliveKey[k] = struct{}{}
			}
			if u := strings.TrimSpace(r.URL); u != "" {
				aliveURL[u] = struct{}{}
			}
			if h := NormalizeHash(r.ContentHash); h != "" {
				aliveHash[h] = struct{}{}
			}
		}
	}
	var rawURLs []string
	for _, it := range items {
		if strings.HasPrefix(it.raw, "http://") || strings.HasPrefix(it.raw, "https://") {
			rawURLs = append(rawURLs, it.raw)
		}
	}
	if len(rawURLs) > 0 {
		var rows []imageAssetRow
		_ = db.Select("object_key", "url", "content_hash").
			Where("user_id = ? AND url IN ? AND COALESCE(NULLIF(status, ''), 'ready') = 'ready'", userID, rawURLs).
			Find(&rows).Error
		for _, r := range rows {
			if k := NormalizeObjectKey(r.ObjectKey); k != "" {
				aliveKey[k] = struct{}{}
			}
			if u := strings.TrimSpace(r.URL); u != "" {
				aliveURL[u] = struct{}{}
			}
			if h := NormalizeHash(r.ContentHash); h != "" {
				aliveHash[h] = struct{}{}
			}
		}
	}

	for _, it := range items {
		ok := false
		if it.key != "" {
			if _, hit := aliveKey[it.key]; hit {
				ok = true
			}
		}
		if !ok {
			if _, hit := aliveURL[it.raw]; hit {
				ok = true
			}
		}
		// content-addressed key still on CDN path even if URL form differs
		if !ok && it.key != "" {
			if h := HashFromObjectKey(it.key); h != "" {
				if _, hit := aliveHash[h]; hit {
					ok = true
				}
			}
		}
		if ok {
			existingURLs = append(existingURLs, it.raw)
		} else {
			missingURLs = append(missingURLs, it.raw)
		}
	}

	// —— content hashes ——
	wantHashes := uniqueNormalizedHashes(hashes)
	if len(wantHashes) > 0 {
		var rows []imageAssetRow
		_ = db.Select("content_hash").
			Where("user_id = ? AND content_hash IN ? AND COALESCE(NULLIF(status, ''), 'ready') = 'ready'", userID, wantHashes).
			Find(&rows).Error
		for _, r := range rows {
			if h := NormalizeHash(r.ContentHash); h != "" {
				aliveHash[h] = struct{}{}
			}
		}
		for _, h := range wantHashes {
			if _, ok := aliveHash[h]; ok {
				existingHashes = append(existingHashes, h)
			} else {
				missingHashes = append(missingHashes, h)
			}
		}
	}
	return existingURLs, missingURLs, existingHashes, missingHashes
}
