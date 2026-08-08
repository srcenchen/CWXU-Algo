package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	pb "cwxu-algo/api/user/v1/blog"
	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/upyun"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

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
// BlogImagesCheck POST /v1/user/blog/images/check
// body: { urls?: [...], hashes?: [sha256 hex,…] }
// → { existing, missing, existingHashes, missingHashes } 一次查询，无 N+1。
// 插件/编辑器可同时按 URL 与 content hash 校验缓存，避免 GC 后误复用。
func (s *BlogService) BlogImagesCheck(ctx context.Context, req *pb.BlogImagesCheckReq) (*pb.BlogImagesCheckRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if len(req.Urls)+len(req.Hashes) > 200 {
		return nil, blogErr(http.StatusBadRequest, "一次最多检查 200 项")
	}
	existing, missing, exHash, missHash := blogimg.ExistingURLsAndHashesForUser(
		s.db, pd.UserID, req.Urls, req.Hashes,
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
	return &pb.BlogImagesCheckRes{
		Code: 0, Message: "success",
		Data: &pb.BlogImagesCheckData{
			Existing:       existing,
			Missing:        missing,
			ExistingHashes: exHash,
			MissingHashes:  missHash,
		},
	}, nil
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
// ImageUploadStatus GET /v1/user/blog/image-upload/status
func (s *BlogService) ImageUploadStatus(ctx context.Context, req *pb.ImageUploadStatusReq) (*pb.ImageUploadStatusRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	client := s.loadUpyunClient()
	configured := client.Configured() && client.PublicBaseURL() != ""
	authorized := s.userImageUploadEnabled(pd.UserID)
	pending := s.pendingImageUploadRequest(pd.UserID)
	data := &pb.ImageUploadStatusData{
		Configured:     configured,
		Authorized:     authorized,
		Enabled:        blogimg.CanUpload(configured, authorized),
		PendingRequest: pending != nil,
	}
	if pending != nil {
		data.PendingRequestId = int64(pending.ID)
	}
	return &pb.ImageUploadStatusRes{Code: 0, Message: "success", Data: data}, nil
}

// handleImageUploadApply POST /v1/user/blog/image-upload/apply
// body: { reason } — 作者申请图片上传权限，须填理由；通知站管审核。
// ImageUploadApply POST /v1/user/blog/image-upload/apply
// body: { reason } — 作者申请图片上传权限，须填理由；通知站管审核。
func (s *BlogService) ImageUploadApply(ctx context.Context, req *pb.ImageUploadApplyReq) (*pb.ImageUploadApplyRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	reason := strings.TrimSpace(req.Reason)
	if utf8.RuneCountInString(reason) < minImageUploadReasonLen {
		return nil, blogErr(http.StatusBadRequest, fmt.Sprintf("请填写至少 %d 字的申请理由", minImageUploadReasonLen))
	}
	if utf8.RuneCountInString(reason) > maxImageUploadReasonLen {
		return nil, blogErr(http.StatusBadRequest, fmt.Sprintf("申请理由最多 %d 字", maxImageUploadReasonLen))
	}
	if s.userImageUploadEnabled(pd.UserID) {
		return &pb.ImageUploadApplyRes{
			Code: 0, Message: "你已开通图片上传",
			Data: &pb.ImageUploadApplyData{PendingRequest: false, Authorized: true},
		}, nil
	}
	if existing := s.pendingImageUploadRequest(pd.UserID); existing != nil {
		return &pb.ImageUploadApplyRes{
			Code: 0, Message: "申请已提交，请等待站点管理员审批",
			Data: &pb.ImageUploadApplyData{Id: int64(existing.ID), PendingRequest: true, Status: existing.Status},
		}, nil
	}

	row, created, err := createPendingImageUploadRequest(s.db, pd.UserID, reason)
	if errors.Is(err, ErrImageUploadAlreadyEnabled) {
		return &pb.ImageUploadApplyRes{
			Code: 0, Message: "你已开通图片上传",
			Data: &pb.ImageUploadApplyData{PendingRequest: false, Authorized: true},
		}, nil
	}
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "提交失败，请稍后重试")
	}
	if !created {
		return &pb.ImageUploadApplyRes{
			Code: 0, Message: "申请已提交，请等待站点管理员审批",
			Data: &pb.ImageUploadApplyData{Id: int64(row.ID), PendingRequest: true, Status: row.Status},
		}, nil
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

	return &pb.ImageUploadApplyRes{
		Code: 0, Message: "已提交申请，请等待站点管理员审批",
		Data: &pb.ImageUploadApplyData{Id: int64(row.ID), PendingRequest: true, Status: row.Status},
	}, nil
}

// handleAdminImageUpload POST /v1/user/blog/admin/image-upload
// body: { userId, enabled }
// AdminImageUpload POST /v1/user/blog/admin/image-upload
// body: { userId, enabled }
func (s *BlogService) AdminImageUpload(ctx context.Context, req *pb.AdminImageUploadReq) (*pb.AdminImageUploadRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		return nil, blogErr(http.StatusForbidden, "需要博客管理权限")
	}
	if req.UserId == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var u model.User
	if err := s.db.Select("id").First(&u, req.UserId).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, blogErr(http.StatusNotFound, "用户不存在")
	} else if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载用户失败")
	}
	if _, err := setAdminImageUploadEnabled(s.db, uint(req.UserId), pd.UserID, req.Enabled); err != nil {
		return nil, blogErr(http.StatusInternalServerError, "保存失败")
	}
	return &pb.AdminImageUploadRes{
		Code: 0, Message: "success",
		Data: &pb.AdminImageUploadToggleData{UserId: req.UserId, ImageUploadEnabled: req.Enabled},
	}, nil
}

// handleAdminImageUploadRequests GET /v1/user/blog/admin/image-upload/requests
// AdminImageUploadRequests GET /v1/user/blog/admin/image-upload/requests
func (s *BlogService) AdminImageUploadRequests(ctx context.Context, req *pb.AdminImageUploadRequestsReq) (*pb.AdminImageUploadRequestsRes, error) {
	imgBase := s.publicImageBase()
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		return nil, blogErr(http.StatusForbidden, "需要博客管理权限")
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	status := strings.TrimSpace(req.Status)
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
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	list := make([]*pb.ImageUploadRequestItem, 0, len(rows))
	for _, r := range rows {
		item := &pb.ImageUploadRequestItem{
			Id:        int64(r.ID),
			UserId:    int64(r.UserID),
			Username:  r.Username,
			Name:      r.Name,
			Avatar:    expandAvatarBase(imgBase, r.Avatar),
			Reason:    r.Reason,
			Status:    r.Status,
			CreatedAt: r.CreatedAt.Unix(),
		}
		if r.ReviewNote != "" {
			item.ReviewNote = r.ReviewNote
		}
		if r.ReviewerID > 0 {
			item.ReviewerId = int64(r.ReviewerID)
		}
		if r.ReviewedAt != nil {
			item.ReviewedAt = r.ReviewedAt.Unix()
		}
		list = append(list, item)
	}
	return &pb.AdminImageUploadRequestsRes{
		Code: 0, Message: "success",
		Data: &pb.ImageUploadRequestListData{List: list, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// handleAdminImageUploadReview POST /v1/user/blog/admin/image-upload/review
// body: { id, action: "approve"|"reject", note? }
// AdminImageUploadReview POST /v1/user/blog/admin/image-upload/review
// body: { id, action: "approve"|"reject", note? }
func (s *BlogService) AdminImageUploadReview(ctx context.Context, req *pb.AdminImageUploadReviewReq) (*pb.AdminImageUploadReviewRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		return nil, blogErr(http.StatusForbidden, "需要博客管理权限")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	action := strings.TrimSpace(strings.ToLower(req.Action))
	if action != "approve" && action != "reject" {
		return nil, blogErr(http.StatusBadRequest, "action 须为 approve 或 reject")
	}
	note := strings.TrimSpace(req.Note)
	if utf8.RuneCountInString(note) > 500 {
		return nil, blogErr(http.StatusBadRequest, "备注最多 500 字")
	}

	row, newStatus, err := reviewImageUploadRequest(s.db, uint(req.Id), pd.UserID, action, note)
	if errors.Is(err, ErrImageUploadRequestNotFound) {
		return nil, blogErr(http.StatusNotFound, "申请不存在")
	}
	if errors.Is(err, ErrImageUploadAlreadyReviewed) {
		return nil, blogErr(http.StatusBadRequest, "该申请已处理")
	}
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "保存失败")
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
	return &pb.AdminImageUploadReviewRes{
		Code: 0, Message: msg,
		Data: &pb.AdminImageUploadReviewData{Id: int64(row.ID), Status: newStatus, UserId: int64(row.UserID)},
	}, nil
}

