package blogimg

import (
	"strings"

	"cwxu-algo/app/common/upyun"
	secretutil "cwxu-algo/app/common/utils/secret"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// ObjectDeleter is the UpYun delete surface used by GC (injectable in tests).
type ObjectDeleter interface {
	Delete(objectKey string) error
	Configured() bool
	PublicBaseURL() string
}

// imageAssetRow mirrors blog_image_assets without importing user models.
type imageAssetRow struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    uint   `gorm:"column:user_id"`
	ObjectKey string `gorm:"column:object_key"`
	URL       string `gorm:"column:url"`
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
// Used by BlogService (create/update/delete) and blogsync (题解同步 upsert/delete).
// Returns number of orphan keys processed (delete attempted).
func GCUserImages(db *gorm.DB, client ObjectDeleter, userID uint) int {
	if db == nil || userID == 0 || client == nil || !client.Configured() {
		return 0
	}
	base := client.PublicBaseURL()
	if base == "" {
		return 0
	}

	var articles []articleRefRow
	_ = db.Select("content", "cover_url").Where("user_id = ?", userID).Find(&articles).Error
	used := map[string]struct{}{}
	for _, a := range articles {
		for k := range KeysFromContent(a.Content, a.CoverURL, base) {
			used[k] = struct{}{}
		}
	}

	var assets []imageAssetRow
	_ = db.Where("user_id = ?", userID).Find(&assets).Error
	var registered []string
	keyToID := map[string]uint{}
	for _, a := range assets {
		k := "/" + strings.TrimPrefix(a.ObjectKey, "/")
		registered = append(registered, k)
		keyToID[k] = a.ID
	}

	orphans := OrphanKeys(registered, used)
	for _, key := range orphans {
		if err := client.Delete(key); err != nil {
			log.Warnf("blog image gc delete %s: %v", key, err)
		}
		if id, ok := keyToID[key]; ok {
			_ = db.Delete(&imageAssetRow{}, id).Error
		}
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
