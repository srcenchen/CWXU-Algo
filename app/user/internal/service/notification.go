package service

import (
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

// NotificationService 站内信
type NotificationService struct {
	db *gorm.DB
}

func NewNotificationService(d *data.Data) *NotificationService {
	return &NotificationService{db: d.DB}
}

// RegisterNotificationRoutes 注册通知路由
func RegisterNotificationRoutes(srv *khttp.Server, s *NotificationService) {
	r := srv.Route("/")
	r.GET("/v1/user/notification/list", s.handleList)
	r.GET("/v1/user/notification/unread-count", s.handleUnreadCount)
	r.POST("/v1/user/notification/read", s.handleRead)
	r.POST("/v1/user/notification/read-all", s.handleReadAll)
	r.POST("/v1/user/notification/clear-all", s.handleClearAll)
}

// CreateNotification 进程内写入（join review 等）
func CreateNotification(db *gorm.DB, n model.Notification) error {
	if db == nil || n.UserID == 0 {
		return nil
	}
	return notify.Create(db, notify.Row{
		UserID:    n.UserID,
		Type:      n.Type,
		Title:     n.Title,
		Body:      n.Body,
		ActorID:   n.ActorID,
		RefType:   n.RefType,
		RefID:     n.RefID,
		ProblemID: n.ProblemID,
		Payload:   n.Payload,
		IsRead:    false,
	})
}

func (s *NotificationService) handleList(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	page, pageSize := pageParams(ctx, 1, 20, 50)
	q := s.db.Model(&model.Notification{}).Where("user_id = ?", pd.UserID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	var list []model.Notification
	if err := q.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	var unread int64
	_ = s.db.Model(&model.Notification{}).Where("user_id = ? AND is_read = false", pd.UserID).Count(&unread).Error

	items := make([]map[string]interface{}, 0, len(list))
	for _, n := range list {
		items = append(items, notifJSON(n))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success":     true,
		"message":     "ok",
		"list":        items,
		"total":       total,
		"page":        page,
		"pageSize":    pageSize,
		"unreadCount": unread,
	})
	return nil
}

func (s *NotificationService) handleUnreadCount(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var unread int64
	_ = s.db.Model(&model.Notification{}).Where("user_id = ? AND is_read = false", pd.UserID).Count(&unread).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "unreadCount": unread,
	})
	return nil
}

func (s *NotificationService) handleRead(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		IDs []uint `json:"ids"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || len(req.IDs) == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定通知"})
		return nil
	}
	if len(req.IDs) > 100 {
		req.IDs = req.IDs[:100]
	}
	_ = s.db.Model(&model.Notification{}).
		Where("user_id = ? AND id IN ?", pd.UserID, req.IDs).
		Update("is_read", true).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "已标记已读"})
	return nil
}

func (s *NotificationService) handleReadAll(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	_ = s.db.Model(&model.Notification{}).
		Where("user_id = ? AND is_read = false", pd.UserID).
		Update("is_read", true).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "全部已读"})
	return nil
}

// handleClearAll 硬删除当前用户全部站内信（不可恢复）
func (s *NotificationService) handleClearAll(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	res := s.db.Where("user_id = ?", pd.UserID).Delete(&model.Notification{})
	if res.Error != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "清空失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true,
		"message": "已清空",
		"deleted": res.RowsAffected,
	})
	return nil
}

func notifJSON(n model.Notification) map[string]interface{} {
	return map[string]interface{}{
		"id":        n.ID,
		"type":      n.Type,
		"title":     n.Title,
		"body":      n.Body,
		"actorId":   n.ActorID,
		"refType":   n.RefType,
		"refId":     n.RefID,
		"problemId": n.ProblemID,
		"payload":   n.Payload,
		"isRead":    n.IsRead,
		"createdAt": n.CreatedAt.Unix(),
	}
}

func pageParams(ctx khttp.Context, defPage, defSize, maxSize int) (page, pageSize int) {
	page = defPage
	pageSize = defSize
	if v := strings.TrimSpace(ctx.Query().Get("page")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := strings.TrimSpace(ctx.Query().Get("pageSize")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	if pageSize > maxSize {
		pageSize = maxSize
	}
	return
}

// Ensure created_at default for raw inserts
var _ = time.Now
