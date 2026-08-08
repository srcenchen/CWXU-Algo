package service

import (
	"context"

	pb "cwxu-algo/api/user/v1/notification"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/gorm"
)

// NotificationService 站内信
// 实现 proto：api/user/v1/notification/notification.proto（NotificationHTTPServer）。
type NotificationService struct {
	db *gorm.DB
}

func NewNotificationService(d *data.Data) *NotificationService {
	return &NotificationService{db: d.DB}
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

func (s *NotificationService) List(ctx context.Context, req *pb.ListReq) (*pb.ListRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.ListRes{Success: false, Message: "请先登录"}, nil
	}
	page := int(req.Page)
	pageSize := int(req.PageSize)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	q := s.db.WithContext(ctx).Model(&model.Notification{}).Where("user_id = ?", uid)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return &pb.ListRes{Success: false, Message: "加载失败"}, nil
	}
	var list []model.Notification
	if err := q.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		return &pb.ListRes{Success: false, Message: "加载失败"}, nil
	}
	var unread int64
	_ = s.db.WithContext(ctx).Model(&model.Notification{}).Where("user_id = ? AND is_read = false", uid).Count(&unread).Error

	items := make([]*pb.NotificationItem, 0, len(list))
	for i := range list {
		items = append(items, notifItem(list[i]))
	}
	return &pb.ListRes{
		Success:     true,
		Message:     "ok",
		List:        items,
		Total:       total,
		Page:        int64(page),
		PageSize:    int64(pageSize),
		UnreadCount: unread,
	}, nil
}

func (s *NotificationService) UnreadCount(ctx context.Context, req *pb.UnreadCountReq) (*pb.UnreadCountRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.UnreadCountRes{Success: false, Message: "请先登录"}, nil
	}
	var unread int64
	_ = s.db.WithContext(ctx).Model(&model.Notification{}).Where("user_id = ? AND is_read = false", uid).Count(&unread).Error
	return &pb.UnreadCountRes{Success: true, Message: "ok", UnreadCount: unread}, nil
}

func (s *NotificationService) Read(ctx context.Context, req *pb.ReadReq) (*pb.ReadRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.ReadRes{Success: false, Message: "请先登录"}, nil
	}
	if len(req.Ids) == 0 {
		return &pb.ReadRes{Success: false, Message: "请指定通知"}, nil
	}
	ids := req.Ids
	if len(ids) > 100 {
		ids = ids[:100]
	}
	idsU := make([]uint, 0, len(ids))
	for _, id := range ids {
		idsU = append(idsU, uint(id))
	}
	_ = s.db.WithContext(ctx).Model(&model.Notification{}).
		Where("user_id = ? AND id IN ?", uid, idsU).
		Update("is_read", true).Error
	return &pb.ReadRes{Success: true, Message: "已标记已读"}, nil
}

func (s *NotificationService) ReadAll(ctx context.Context, req *pb.ReadAllReq) (*pb.ReadAllRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.ReadAllRes{Success: false, Message: "请先登录"}, nil
	}
	_ = s.db.WithContext(ctx).Model(&model.Notification{}).
		Where("user_id = ? AND is_read = false", uid).
		Update("is_read", true).Error
	return &pb.ReadAllRes{Success: true, Message: "全部已读"}, nil
}

// ClearAll 硬删除当前用户全部站内信（不可恢复）
func (s *NotificationService) ClearAll(ctx context.Context, req *pb.ClearAllReq) (*pb.ClearAllRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.ClearAllRes{Success: false, Message: "请先登录"}, nil
	}
	res := s.db.WithContext(ctx).Where("user_id = ?", uid).Delete(&model.Notification{})
	if res.Error != nil {
		return &pb.ClearAllRes{Success: false, Message: "清空失败"}, nil
	}
	return &pb.ClearAllRes{Success: true, Message: "已清空", Deleted: res.RowsAffected}, nil
}

func notifItem(n model.Notification) *pb.NotificationItem {
	return &pb.NotificationItem{
		Id:        int64(n.ID),
		Type:      n.Type,
		Title:     n.Title,
		Body:      n.Body,
		ActorId:   int64(n.ActorID),
		RefType:   n.RefType,
		RefId:     int64(n.RefID),
		ProblemId: int64(n.ProblemID),
		Payload:   n.Payload,
		IsRead:    n.IsRead,
		CreatedAt: n.CreatedAt.Unix(),
	}
}
