package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/upyun"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

// loadUpyunFromDB builds an UpYun client from site_configs id=1.
func loadUpyunFromDB(db *gorm.DB) *upyun.Client {
	return blogimg.LoadUpyunClient(db)
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
// referenced by any of their articles (content + cover). Shared with blogsync.
func (s *BlogService) gcUserBlogImages(userID uint) {
	blogimg.GCUserImagesFromSite(s.db, userID)
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

// handleBlogImagesCheck POST /v1/user/blog/images/check
// body: { urls: ["https://…/blog/27/x.webp", "/blog/27/x.webp", …] }
// → { existing: [...], missing: [...] } 一次查询，无 N+1。
func (s *BlogService) handleBlogImagesCheck(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if len(body.URLs) > 200 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "一次最多检查 200 个地址"})
		return nil
	}
	existing, missing := blogimg.ExistingURLsForUser(s.db, pd.UserID, body.URLs)
	if existing == nil {
		existing = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"existing": existing,
			"missing":  missing,
		},
	})
	return nil
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
