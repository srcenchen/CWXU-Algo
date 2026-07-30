package blogimg

import (
	"strings"
	"time"

	"cwxu-algo/app/common/upyun"
	secretutil "cwxu-algo/app/common/utils/secret"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// GCGracePeriod：刚上传、可能还在编辑中的图暂不 GC，避免「另存一篇」误删草稿图。
const GCGracePeriod = 2 * time.Hour

// ObjectDeleter is the UpYun delete surface used by GC (injectable in tests).
type ObjectDeleter interface {
	Delete(objectKey string) error
	Configured() bool
	PublicBaseURL() string
}

// imageAssetRow mirrors blog_image_assets without importing user models.
type imageAssetRow struct {
	ID        uint      `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UserID    uint      `gorm:"column:user_id"`
	ObjectKey string    `gorm:"column:object_key"`
	URL       string    `gorm:"column:url"`
}

func (imageAssetRow) TableName() string { return "blog_image_assets" }

// articleRefRow is enough content to compute referenced image keys.
type articleRefRow struct {
	ID       uint   `gorm:"primaryKey"`
	UserID   uint   `gorm:"column:user_id;index"`
	Content  string `gorm:"column:content"`
	CoverURL string `gorm:"column:cover_url"`
}

func (articleRefRow) TableName() string { return "blog_articles" }

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

// GCUserImages deletes registered UpYun objects for user that are no longer
// referenced by any of their blog_articles (content + cover).
//
// 引用判定（多重，防误删）：
//  1. KeysFromContent（含 /blog/n/… 路径，与 host 无关）
//  2. AssetReferenced：object key / 完整 URL 出现在任一文正文或头图
//  3. 宽限期 GCGracePeriod 内新上传的资产不删
//
// Returns number of orphan keys processed (delete attempted).
func GCUserImages(db *gorm.DB, client ObjectDeleter, userID uint) int {
	return GCUserImagesAt(db, client, userID, time.Now())
}

// GCUserImagesAt is GCUserImages with injectable clock (tests).
func GCUserImagesAt(db *gorm.DB, client ObjectDeleter, userID uint, now time.Time) int {
	if db == nil || userID == 0 || client == nil || !client.Configured() {
		return 0
	}
	base := client.PublicBaseURL()
	// base 可空：仍可用路径/子串匹配做引用判定；但无 base 时不调又拍云删除
	canDeleteRemote := base != ""

	var articles []articleRefRow
	_ = db.Select("content", "cover_url").Where("user_id = ?", userID).Find(&articles).Error
	used := map[string]struct{}{}
	var texts []string
	for _, a := range articles {
		texts = append(texts, a.Content, a.CoverURL)
		for k := range KeysFromContent(a.Content, a.CoverURL, base) {
			used[k] = struct{}{}
		}
	}

	var assets []imageAssetRow
	_ = db.Where("user_id = ?", userID).Find(&assets).Error
	var orphans []imageAssetRow
	for _, a := range assets {
		key := NormalizeObjectKey(a.ObjectKey)
		if key == "" {
			continue
		}
		// 宽限期：编辑中未写入正文的新图
		if !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) < GCGracePeriod {
			continue
		}
		if _, ok := used[key]; ok {
			continue
		}
		if AssetReferenced(key, a.URL, texts...) {
			continue
		}
		orphans = append(orphans, a)
	}

	for _, a := range orphans {
		key := NormalizeObjectKey(a.ObjectKey)
		if canDeleteRemote {
			if err := client.Delete(key); err != nil {
				log.Warnf("blog image gc delete %s: %v", key, err)
			}
		}
		_ = db.Delete(&imageAssetRow{}, a.ID).Error
	}
	return len(orphans)
}

// GCUserImagesFromSite loads UpYun from site_configs then runs GCUserImages.
func GCUserImagesFromSite(db *gorm.DB, userID uint) int {
	return GCUserImages(db, LoadUpyunClient(db), userID)
}

// ScheduleGCUserImages runs GC asynchronously (fire-and-forget).
func ScheduleGCUserImages(db *gorm.DB, userID uint) {
	if db == nil || userID == 0 {
		return
	}
	go func() {
		_ = GCUserImagesFromSite(db, userID)
	}()
}

// ExistingURLsForUser returns which of the given URLs are still registered
// as blog_image_assets for this user (batch, no N+1).
// Also accepts bare object keys.
func ExistingURLsForUser(db *gorm.DB, userID uint, urls []string) (existing, missing []string) {
	if db == nil || userID == 0 || len(urls) == 0 {
		return nil, append([]string{}, urls...)
	}
	// 规范化输入，去重保序
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
	if len(items) == 0 {
		return nil, nil
	}

	aliveKey := map[string]struct{}{}
	aliveURL := map[string]struct{}{}
	if len(keys) > 0 {
		var rows []imageAssetRow
		_ = db.Select("object_key", "url").
			Where("user_id = ? AND object_key IN ?", userID, keys).
			Find(&rows).Error
		for _, r := range rows {
			if k := NormalizeObjectKey(r.ObjectKey); k != "" {
				aliveKey[k] = struct{}{}
			}
			if u := strings.TrimSpace(r.URL); u != "" {
				aliveURL[u] = struct{}{}
			}
		}
	}
	// 也按完整 URL 查一轮（历史 object_key 与 URL 不一致时）
	var rawURLs []string
	for _, it := range items {
		if strings.HasPrefix(it.raw, "http://") || strings.HasPrefix(it.raw, "https://") {
			rawURLs = append(rawURLs, it.raw)
		}
	}
	if len(rawURLs) > 0 {
		var rows []imageAssetRow
		_ = db.Select("object_key", "url").
			Where("user_id = ? AND url IN ?", userID, rawURLs).
			Find(&rows).Error
		for _, r := range rows {
			if k := NormalizeObjectKey(r.ObjectKey); k != "" {
				aliveKey[k] = struct{}{}
			}
			if u := strings.TrimSpace(r.URL); u != "" {
				aliveURL[u] = struct{}{}
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
		if ok {
			existing = append(existing, it.raw)
		} else {
			missing = append(missing, it.raw)
		}
	}
	return existing, missing
}
