package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/upyun"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

const (
	minImageUploadReasonLen = 5
	maxImageUploadReasonLen = 500
)

var (
	ErrImageUploadAlreadyReviewed = errors.New("image upload request already reviewed")
	ErrImageUploadRequestNotFound = errors.New("image upload request not found")
	ErrImageUploadAlreadyEnabled  = errors.New("image upload already enabled")
)

type blogImageObjectWriter interface {
	Put(objectKey string, data []byte, contentType string) error
}

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

// registerBlogImageAsset records an uploaded object for later GC (keyed by object path + content hash).
func registerBlogImageAsset(db *gorm.DB, userID uint, objectKey, publicURL, contentHash, purpose string, articleID *uint) error {
	if db == nil || userID == 0 || objectKey == "" {
		return fmt.Errorf("invalid asset")
	}
	asset, err := reserveBlogImageAsset(db, userID, objectKey, publicURL, contentHash, purpose, articleID)
	if err != nil {
		return err
	}
	return blogimg.WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
		return finalizeBlogImageAssetTx(tx, asset.ID, userID)
	})
}

func reserveBlogImageAsset(db *gorm.DB, userID uint, objectKey, publicURL, contentHash, purpose string, articleID *uint) (model.BlogImageAsset, error) {
	var asset model.BlogImageAsset
	err := blogimg.WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
		var err error
		asset, err = reserveBlogImageAssetTx(tx, userID, objectKey, publicURL, contentHash, purpose, articleID)
		return err
	})
	return asset, err
}

func reserveBlogImageAssetTx(db *gorm.DB, userID uint, objectKey, publicURL, contentHash, purpose string, articleID *uint) (model.BlogImageAsset, error) {
	key := blogimg.NormalizeObjectKey(objectKey)
	if key == "" {
		key = "/" + strings.TrimPrefix(objectKey, "/")
	}
	hash := blogimg.NormalizeHash(contentHash)
	now := time.Now()
	var existing model.BlogImageAsset
	err := db.Where("object_key = ?", key).First(&existing).Error
	if err == nil {
		updates := map[string]interface{}{
			"user_id":     userID,
			"url":         publicURL,
			"purpose":     purpose,
			"reserved_at": &now,
		}
		if hash != "" {
			updates["content_hash"] = hash
		}
		if articleID != nil {
			updates["article_id"] = *articleID
		}
		if err := db.Model(&existing).Updates(updates).Error; err != nil {
			return model.BlogImageAsset{}, err
		}
		return existing, db.First(&existing, existing.ID).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.BlogImageAsset{}, err
	}
	row := model.BlogImageAsset{
		UserID:      userID,
		ObjectKey:   key,
		URL:         publicURL,
		ContentHash: hash,
		Purpose:     purpose,
		ArticleID:   articleID,
		Status:      model.BlogImageAssetPending,
		ReservedAt:  &now,
	}
	return row, db.Create(&row).Error
}

func finalizeBlogImageAssetTx(db *gorm.DB, assetID, userID uint) error {
	var asset model.BlogImageAsset
	if err := db.Select("id", "status").Where("id = ? AND user_id = ?", assetID, userID).First(&asset).Error; err != nil {
		return err
	}
	if asset.Status == model.BlogImageAssetReady || asset.Status == "" {
		return db.Model(&model.BlogImageAsset{}).Where("id = ? AND user_id = ?", assetID, userID).
			Update("reserved_at", nil).Error
	}
	res := db.Model(&model.BlogImageAsset{}).
		Where("id = ? AND user_id = ? AND status = ?", assetID, userID, model.BlogImageAssetPending).
		Updates(map[string]interface{}{"status": model.BlogImageAssetReady, "reserved_at": nil})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("finalize blog image asset %d affected %d", assetID, res.RowsAffected)
	}
	return nil
}

func putAndRegisterBlogImage(
	db *gorm.DB,
	writer blogImageObjectWriter,
	userID uint,
	objectKey string,
	data []byte,
	contentType, storedURL, contentHash, purpose string,
) error {
	if db == nil || writer == nil || userID == 0 || objectKey == "" {
		return fmt.Errorf("invalid blog image upload")
	}
	asset, err := reserveBlogImageAsset(db, userID, objectKey, storedURL, contentHash, purpose, nil)
	if err != nil {
		return err
	}
	if err := writer.Put(objectKey, data, contentType); err != nil {
		return err
	}
	return blogimg.WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
		return finalizeBlogImageAssetTx(tx, asset.ID, userID)
	})
}

// handleBlogImagesCheck POST /v1/user/blog/images/check
// body: { urls?: [...], hashes?: [sha256 hex,…] }
// → { existing, missing, existingHashes, missingHashes } 一次查询，无 N+1。
// 插件/编辑器可同时按 URL 与 content hash 校验缓存，避免 GC 后误复用。
func (s *BlogService) handleBlogImagesCheck(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		URLs   []string `json:"urls"`
		Hashes []string `json:"hashes"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if len(body.URLs)+len(body.Hashes) > 200 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "一次最多检查 200 项"})
		return nil
	}
	existing, missing, exHash, missHash := blogimg.ExistingURLsAndHashesForUser(
		s.db, pd.UserID, body.URLs, body.Hashes,
	)
	if existing == nil {
		existing = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	if exHash == nil {
		exHash = []string{}
	}
	if missHash == nil {
		missHash = []string{}
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"existing":       existing,
			"missing":        missing,
			"existingHashes": exHash,
			"missingHashes":  missHash,
		},
	})
	return nil
}

// pendingImageUploadRequest returns the latest pending request for user, if any.
func (s *BlogService) pendingImageUploadRequest(userID uint) *model.BlogImageUploadRequest {
	if userID == 0 {
		return nil
	}
	var row model.BlogImageUploadRequest
	err := s.db.Where("user_id = ? AND status = ?", userID, model.BlogImageUploadPending).
		Order("id DESC").First(&row).Error
	if err != nil {
		return nil
	}
	return &row
}

func setUserImageUploadEnabledDB(db *gorm.DB, userID uint, enabled bool) error {
	if userID == 0 {
		return fmt.Errorf("invalid user")
	}
	var cfg model.BlogSiteConfig
	err := db.Where("user_id = ?", userID).First(&cfg).Error
	if err == gorm.ErrRecordNotFound {
		cfg = model.BlogSiteConfig{
			UserID:             userID,
			ThemeID:            "mizuki",
			ImageUploadEnabled: enabled,
		}
		return db.Create(&cfg).Error
	}
	if err != nil {
		return err
	}
	return db.Model(&cfg).Update("image_upload_enabled", enabled).Error
}

func setAdminImageUploadEnabled(db *gorm.DB, userID, reviewerID uint, enabled bool) (int64, error) {
	if db == nil || userID == 0 || reviewerID == 0 {
		return 0, fmt.Errorf("invalid image upload permission change")
	}
	var reviewed int64
	err := blogimg.WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
		if err := setUserImageUploadEnabledDB(tx, userID, enabled); err != nil {
			return err
		}
		if !enabled {
			return nil
		}
		now := time.Now()
		res := tx.Model(&model.BlogImageUploadRequest{}).
			Where("user_id = ? AND status = ?", userID, model.BlogImageUploadPending).
			Updates(map[string]interface{}{
				"status":      model.BlogImageUploadApproved,
				"reviewer_id": reviewerID,
				"reviewed_at": now,
				"review_note": "站管直接开通",
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 1 {
			return fmt.Errorf("unexpected pending image upload rows: %d", res.RowsAffected)
		}
		reviewed = res.RowsAffected
		return nil
	})
	return reviewed, err
}

func createPendingImageUploadRequest(db *gorm.DB, userID uint, reason string) (model.BlogImageUploadRequest, bool, error) {
	var row model.BlogImageUploadRequest
	created := false
	err := blogimg.WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
		var cfg model.BlogSiteConfig
		if err := tx.Select("image_upload_enabled").Where("user_id = ?", userID).First(&cfg).Error; err == nil {
			if cfg.ImageUploadEnabled {
				return ErrImageUploadAlreadyEnabled
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Where("user_id = ? AND status = ?", userID, model.BlogImageUploadPending).
			Order("id DESC").First(&row).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row = model.BlogImageUploadRequest{UserID: userID, Reason: reason, Status: model.BlogImageUploadPending}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if errors.Is(err, ErrImageUploadAlreadyEnabled) {
		return row, false, err
	}
	if err == nil {
		return row, created, nil
	}
	// A concurrent transaction may have won the partial unique index race.
	if qerr := db.Where("user_id = ? AND status = ?", userID, model.BlogImageUploadPending).
		Order("id DESC").First(&row).Error; qerr == nil {
		return row, false, nil
	}
	return model.BlogImageUploadRequest{}, false, err
}

func reviewImageUploadRequest(db *gorm.DB, requestID, reviewerID uint, action, note string) (model.BlogImageUploadRequest, string, error) {
	var row model.BlogImageUploadRequest
	newStatus := model.BlogImageUploadApproved
	if action == "reject" {
		newStatus = model.BlogImageUploadRejected
	}
	if err := db.Select("id", "user_id").First(&row, requestID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, newStatus, ErrImageUploadRequestNotFound
		}
		return row, newStatus, err
	}
	err := blogimg.WithUserImageReferenceTx(db, row.UserID, func(tx *gorm.DB) error {
		if err := tx.First(&row, requestID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrImageUploadRequestNotFound
			}
			return err
		}
		if action == "approve" {
			if err := setUserImageUploadEnabledDB(tx, row.UserID, true); err != nil {
				return err
			}
		}
		now := time.Now()
		res := tx.Model(&model.BlogImageUploadRequest{}).
			Where("id = ? AND status = ?", requestID, model.BlogImageUploadPending).
			Updates(map[string]interface{}{
				"status": newStatus, "review_note": note, "reviewer_id": reviewerID, "reviewed_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrImageUploadAlreadyReviewed
		}
		return nil
	})
	return row, newStatus, err
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
	pending := s.pendingImageUploadRequest(pd.UserID)
	data := map[string]interface{}{
		"configured":     configured,
		"authorized":     authorized,
		"enabled":        blogimg.CanUpload(configured, authorized),
		"pendingRequest": pending != nil,
	}
	if pending != nil {
		data["pendingRequestId"] = pending.ID
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": data,
	})
	return nil
}

// handleImageUploadApply POST /v1/user/blog/image-upload/apply
// body: { reason } — 作者申请图片上传权限，须填理由；通知站管审核。
func (s *BlogService) handleImageUploadApply(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	reason := strings.TrimSpace(body.Reason)
	if utf8.RuneCountInString(reason) < minImageUploadReasonLen {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code": 1, "message": fmt.Sprintf("请填写至少 %d 字的申请理由", minImageUploadReasonLen),
		})
		return nil
	}
	if utf8.RuneCountInString(reason) > maxImageUploadReasonLen {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code": 1, "message": fmt.Sprintf("申请理由最多 %d 字", maxImageUploadReasonLen),
		})
		return nil
	}
	if s.userImageUploadEnabled(pd.UserID) {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "你已开通图片上传",
			"data": map[string]interface{}{
				"pendingRequest": false,
				"authorized":     true,
			},
		})
		return nil
	}
	if existing := s.pendingImageUploadRequest(pd.UserID); existing != nil {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "申请已提交，请等待站点管理员审批",
			"data": map[string]interface{}{
				"id":             existing.ID,
				"pendingRequest": true,
				"status":         existing.Status,
			},
		})
		return nil
	}

	row, created, err := createPendingImageUploadRequest(s.db, pd.UserID, reason)
	if errors.Is(err, ErrImageUploadAlreadyEnabled) {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "你已开通图片上传",
			"data": map[string]interface{}{"pendingRequest": false, "authorized": true},
		})
		return nil
	}
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "提交失败，请稍后重试"})
		return nil
	}
	if !created {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "申请已提交，请等待站点管理员审批",
			"data": map[string]interface{}{"id": row.ID, "pendingRequest": true, "status": row.Status},
		})
		return nil
	}

	var u model.User
	_ = s.db.Select("id", "username", "name").First(&u, pd.UserID).Error
	display := strings.TrimSpace(u.Name)
	if display == "" {
		display = u.Username
	}
	if display == "" {
		display = fmt.Sprintf("用户#%d", pd.UserID)
	}
	bodyText := fmt.Sprintf("%s（@%s）申请图片上传权限：%s", display, u.Username, reason)
	if utf8.RuneCountInString(bodyText) > 280 {
		runes := []rune(bodyText)
		bodyText = string(runes[:277]) + "…"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"requestId": row.ID,
		"userId":    pd.UserID,
		"username":  u.Username,
		"reason":    reason,
		"path":      "/admin/blog",
	})
	notify.NotifySiteAdmins(s.db, notify.AdminNotif{
		Type:       notify.TypeReviewPending,
		Title:      "图片上传权限申请",
		Body:       bodyText,
		ActorID:    pd.UserID,
		RefType:    "blog_image_upload",
		RefID:      row.ID,
		Payload:    string(payload),
		SkipUserID: pd.UserID,
	})

	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已提交申请，请等待站点管理员审批",
		"data": map[string]interface{}{
			"id":             row.ID,
			"pendingRequest": true,
			"status":         row.Status,
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
	if err := s.db.Select("id").First(&u, body.UserID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不存在"})
		return nil
	} else if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载用户失败"})
		return nil
	}
	if _, err := setAdminImageUploadEnabled(s.db, body.UserID, pd.UserID, body.Enabled); err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"userId":             body.UserID,
			"imageUploadEnabled": body.Enabled,
		},
	})
	return nil
}

// handleAdminImageUploadRequests GET /v1/user/blog/admin/image-upload/requests
func (s *BlogService) handleAdminImageUploadRequests(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客管理权限"})
		return nil
	}
	q := ctx.Request().URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	status := strings.TrimSpace(q.Get("status"))
	if status == "" {
		status = model.BlogImageUploadPending
	}
	if status == "all" {
		status = ""
	}

	countQ := s.db.Table("blog_image_upload_requests")
	if status != "" {
		countQ = countQ.Where("status = ?", status)
	}
	var total int64
	_ = countQ.Count(&total).Error

	type row struct {
		ID         uint
		UserID     uint
		Reason     string
		Status     string
		ReviewNote string
		ReviewerID uint
		CreatedAt  time.Time
		ReviewedAt *time.Time
		Username   string
		Name       string
		Avatar     string
	}
	var rows []row
	listQ := s.db.Table("blog_image_upload_requests AS r").
		Select(`r.id, r.user_id, r.reason, r.status, r.review_note, r.reviewer_id,
			r.created_at, r.reviewed_at,
			COALESCE(u.username,'') AS username,
			COALESCE(u.name,'') AS name,
			COALESCE(u.avatar,'') AS avatar`).
		Joins("LEFT JOIN users u ON u.id = r.user_id")
	if status != "" {
		listQ = listQ.Where("r.status = ?", status)
	}
	err := listQ.Order("r.id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&rows).Error
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	list := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		item := map[string]interface{}{
			"id":        r.ID,
			"userId":    r.UserID,
			"username":  r.Username,
			"name":      r.Name,
			"avatar":    r.Avatar,
			"reason":    r.Reason,
			"status":    r.Status,
			"createdAt": r.CreatedAt.Unix(),
		}
		if r.ReviewNote != "" {
			item["reviewNote"] = r.ReviewNote
		}
		if r.ReviewerID > 0 {
			item["reviewerId"] = r.ReviewerID
		}
		if r.ReviewedAt != nil {
			item["reviewedAt"] = r.ReviewedAt.Unix()
		}
		list = append(list, item)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"list":     list,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
	return nil
}

// handleAdminImageUploadReview POST /v1/user/blog/admin/image-upload/review
// body: { id, action: "approve"|"reject", note? }
func (s *BlogService) handleAdminImageUploadReview(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客管理权限"})
		return nil
	}
	var body struct {
		ID     uint   `json:"id"`
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	action := strings.TrimSpace(strings.ToLower(body.Action))
	if action != "approve" && action != "reject" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "action 须为 approve 或 reject"})
		return nil
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > 500 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "备注最多 500 字"})
		return nil
	}

	row, newStatus, err := reviewImageUploadRequest(s.db, body.ID, pd.UserID, action, note)
	if errors.Is(err, ErrImageUploadRequestNotFound) {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "申请不存在"})
		return nil
	}
	if errors.Is(err, ErrImageUploadAlreadyReviewed) {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "该申请已处理"})
		return nil
	}
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}

	if action == "approve" {
		_ = CreateNotification(s.db, model.Notification{
			UserID:  row.UserID,
			Type:    model.NotifTypeImageUploadApproved,
			Title:   "图片上传权限已开通",
			Body:    "站点管理员已通过你的图片上传申请，现在可以在博客与题解中直接上传图片。",
			ActorID: pd.UserID,
			RefType: "blog_image_upload",
			RefID:   row.ID,
		})
	} else {
		bodyMsg := "站点管理员未通过你的图片上传申请。"
		if note != "" {
			bodyMsg += " 备注：" + note
		}
		_ = CreateNotification(s.db, model.Notification{
			UserID:  row.UserID,
			Type:    model.NotifTypeImageUploadRejected,
			Title:   "图片上传申请未通过",
			Body:    bodyMsg,
			ActorID: pd.UserID,
			RefType: "blog_image_upload",
			RefID:   row.ID,
		})
	}

	msg := "已通过"
	if action == "reject" {
		msg = "已驳回"
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": msg,
		"data": map[string]interface{}{
			"id":     row.ID,
			"status": newStatus,
			"userId": row.UserID,
		},
	})
	return nil
}
