package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/upyun"
	"cwxu-algo/app/common/utils/auth"
	secretutil "cwxu-algo/app/common/utils/secret"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

// loadUpyunFromDB builds an UpYun client from site_configs id=1.
func loadUpyunFromDB(db *gorm.DB) *upyun.Client {
	if db == nil {
		return upyun.New(upyun.Config{})
	}
	var row model.SiteConfig
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

func (s *BlogService) loadUpyunClient() *upyun.Client {
	return loadUpyunFromDB(s.db)
}

func (s *BlogService) userImageUploadEnabled(userID uint) bool {
	if userID == 0 {
		return false
	}
	var cfg model.BlogSiteConfig
	if err := s.db.Select("image_upload_enabled").Where("user_id = ?", userID).First(&cfg).Error; err != nil {
		return false
	}
	return cfg.ImageUploadEnabled
}

// gcUserBlogImages deletes UpYun objects registered for user that are no longer
// referenced by any of their articles (content + cover).
func (s *BlogService) gcUserBlogImages(userID uint) {
	if userID == 0 {
		return
	}
	client := s.loadUpyunClient()
	if !client.Configured() {
		return
	}
	base := client.PublicBaseURL()
	if base == "" {
		return
	}

	var articles []model.BlogArticle
	_ = s.db.Select("content", "cover_url").Where("user_id = ?", userID).Find(&articles).Error
	used := map[string]struct{}{}
	for _, a := range articles {
		for k := range blogimg.KeysFromContent(a.Content, a.CoverURL, base) {
			used[k] = struct{}{}
		}
	}

	var assets []model.BlogImageAsset
	_ = s.db.Where("user_id = ?", userID).Find(&assets).Error
	var registered []string
	keyToID := map[string]uint{}
	for _, a := range assets {
		k := "/" + strings.TrimPrefix(a.ObjectKey, "/")
		registered = append(registered, k)
		keyToID[k] = a.ID
	}
	for _, key := range blogimg.OrphanKeys(registered, used) {
		if err := client.Delete(key); err != nil {
			log.Warnf("blog image gc delete %s: %v", key, err)
		}
		if id, ok := keyToID[key]; ok {
			_ = s.db.Delete(&model.BlogImageAsset{}, id).Error
		}
	}
}

// registerBlogImageAsset records an uploaded object for later GC.
func registerBlogImageAsset(db *gorm.DB, userID uint, objectKey, publicURL, purpose string, articleID *uint) error {
	if db == nil || userID == 0 || objectKey == "" {
		return fmt.Errorf("invalid asset")
	}
	key := "/" + strings.TrimPrefix(objectKey, "/")
	var existing model.BlogImageAsset
	err := db.Where("object_key = ?", key).First(&existing).Error
	if err == nil {
		updates := map[string]interface{}{
			"user_id": userID,
			"url":     publicURL,
			"purpose": purpose,
		}
		if articleID != nil {
			updates["article_id"] = *articleID
		}
		return db.Model(&existing).Updates(updates).Error
	}
	row := model.BlogImageAsset{
		UserID:    userID,
		ObjectKey: key,
		URL:       publicURL,
		Purpose:   purpose,
		ArticleID: articleID,
	}
	return db.Create(&row).Error
}

// handleImageUploadStatus GET /v1/user/blog/image-upload/status
func (s *BlogService) handleImageUploadStatus(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	client := s.loadUpyunClient()
	configured := client.Configured() && client.PublicBaseURL() != ""
	authorized := s.userImageUploadEnabled(pd.UserID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"configured": configured,
			"authorized": authorized,
			"enabled":    blogimg.CanUpload(configured, authorized),
		},
	})
	return nil
}

// handleAdminImageUpload POST /v1/user/blog/admin/image-upload
// body: { userId, enabled }
func (s *BlogService) handleAdminImageUpload(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客管理权限"})
		return nil
	}
	var body struct {
		UserID  uint `json:"userId"`
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var u model.User
	if err := s.db.Select("id").First(&u, body.UserID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不存在"})
		return nil
	}
	var cfg model.BlogSiteConfig
	err := s.db.Where("user_id = ?", body.UserID).First(&cfg).Error
	if err == gorm.ErrRecordNotFound {
		cfg = model.BlogSiteConfig{
			UserID:             body.UserID,
			ThemeID:            "mizuki",
			ImageUploadEnabled: body.Enabled,
		}
		if e := s.db.Create(&cfg).Error; e != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
			return nil
		}
	} else if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	} else {
		if e := s.db.Model(&cfg).Update("image_upload_enabled", body.Enabled).Error; e != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
			return nil
		}
		cfg.ImageUploadEnabled = body.Enabled
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"userId":             body.UserID,
			"imageUploadEnabled": cfg.ImageUploadEnabled,
		},
	})
	return nil
}
