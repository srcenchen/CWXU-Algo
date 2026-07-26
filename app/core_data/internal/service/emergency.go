package service

import (
	"context"
	"cwxu-algo/api/core/v1/emergency"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/errors"
	"gorm.io/gorm"
)

type EmergencyService struct {
	emergency.UnimplementedEmergencyServer
	dal *dal.EmergencyDal
}

func NewEmergencyService(d *dal.EmergencyDal) *EmergencyService {
	return &EmergencyService{dal: d}
}

func (s *EmergencyService) modelToProto(m *model.EmergencyNotice) *emergency.EmergencyInfo {
	return &emergency.EmergencyInfo{
		Id:         m.ID,
		Title:      m.Title,
		Content:    m.Content,
		Enabled:    m.Enabled,
		SortOrder:  m.SortOrder,
		AuthorId:   m.AuthorID,
		AuthorName: m.AuthorName,
		CreatedAt:  m.CreatedAt.Unix(),
		UpdatedAt:  m.UpdatedAt.Unix(),
	}
}

func (s *EmergencyService) Create(ctx context.Context, req *emergency.CreateEmergencyReq) (*emergency.CreateEmergencyRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteEmergency) {
		return &emergency.CreateEmergencyRes{Code: 1, Message: "权限不足，仅站点管理员可发布紧急通知"}, nil
	}
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return &emergency.CreateEmergencyRes{Code: 1, Message: "未获取到用户信息"}, nil
	}
	if req.Title == "" {
		return &emergency.CreateEmergencyRes{Code: 2, Message: "标题不能为空"}, nil
	}
	if req.Content == "" {
		return &emergency.CreateEmergencyRes{Code: 3, Message: "内容不能为空"}, nil
	}

	m := &model.EmergencyNotice{
		Title:      req.Title,
		Content:    req.Content,
		Enabled:    req.Enabled,
		SortOrder:  req.SortOrder,
		AuthorID:   int64(user.UserID),
		AuthorName: user.Name,
	}
	if err := s.dal.Create(m); err != nil {
		return nil, errors.InternalServer("创建失败", "服务暂时不可用")
	}
	return &emergency.CreateEmergencyRes{
		Code:    0,
		Message: "success",
		Data:    s.modelToProto(m),
	}, nil
}

func (s *EmergencyService) Update(ctx context.Context, req *emergency.UpdateEmergencyReq) (*emergency.UpdateEmergencyRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteEmergency) {
		return &emergency.UpdateEmergencyRes{Code: 1, Message: "权限不足"}, nil
	}
	existing, err := s.dal.GetById(req.Id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return &emergency.UpdateEmergencyRes{Code: 2, Message: "通知不存在"}, nil
		}
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	_ = existing

	updates := map[string]interface{}{
		"enabled":    req.Enabled,
		"sort_order": req.SortOrder,
	}
	if req.Title != "" {
		updates["title"] = req.Title
	}
	if req.Content != "" {
		updates["content"] = req.Content
	}

	if err := s.dal.Update(req.Id, updates); err != nil {
		return nil, errors.InternalServer("更新失败", "服务暂时不可用")
	}
	updated, err := s.dal.GetById(req.Id)
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	return &emergency.UpdateEmergencyRes{
		Code:    0,
		Message: "success",
		Data:    s.modelToProto(updated),
	}, nil
}

func (s *EmergencyService) Delete(ctx context.Context, req *emergency.DeleteEmergencyReq) (*emergency.DeleteEmergencyRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteEmergency) {
		return &emergency.DeleteEmergencyRes{Code: 1, Message: "权限不足"}, nil
	}
	if _, err := s.dal.GetById(req.Id); err != nil {
		if err == gorm.ErrRecordNotFound {
			return &emergency.DeleteEmergencyRes{Code: 2, Message: "通知不存在"}, nil
		}
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	if err := s.dal.Delete(req.Id); err != nil {
		return nil, errors.InternalServer("删除失败", "服务暂时不可用")
	}
	return &emergency.DeleteEmergencyRes{Code: 0, Message: "success"}, nil
}

func (s *EmergencyService) List(ctx context.Context, req *emergency.ListEmergencyReq) (*emergency.ListEmergencyRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteEmergency) {
		return &emergency.ListEmergencyRes{Code: 1, Message: "权限不足"}, nil
	}
	page := req.Page
	if page < 1 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize < 1 {
		pageSize = 10
	}
	if pageSize > 50 {
		pageSize = 50
	}
	list, total, err := s.dal.List(page, pageSize)
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	data := make([]*emergency.EmergencyInfo, 0, len(list))
	for i := range list {
		data = append(data, s.modelToProto(&list[i]))
	}
	return &emergency.ListEmergencyRes{
		Code:     0,
		Message:  "success",
		Data:     data,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (s *EmergencyService) Active(ctx context.Context, _ *emergency.ActiveEmergencyReq) (*emergency.ActiveEmergencyRes, error) {
	list, err := s.dal.ListActive()
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	data := make([]*emergency.EmergencyInfo, 0, len(list))
	for i := range list {
		data = append(data, s.modelToProto(&list[i]))
	}
	return &emergency.ActiveEmergencyRes{
		Code:    0,
		Message: "success",
		Data:    data,
	}, nil
}

func (s *EmergencyService) Reorder(ctx context.Context, req *emergency.ReorderEmergencyReq) (*emergency.ReorderEmergencyRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteEmergency) {
		return &emergency.ReorderEmergencyRes{Code: 1, Message: "权限不足"}, nil
	}
	if len(req.Ids) == 0 {
		return &emergency.ReorderEmergencyRes{Code: 2, Message: "顺序列表不能为空"}, nil
	}
	// 去重，保持首次出现顺序
	seen := make(map[int64]struct{}, len(req.Ids))
	ids := make([]int64, 0, len(req.Ids))
	for _, id := range req.Ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return &emergency.ReorderEmergencyRes{Code: 2, Message: "顺序列表不能为空"}, nil
	}
	if err := s.dal.Reorder(ids); err != nil {
		return nil, errors.InternalServer("排序失败", "服务暂时不可用")
	}
	return &emergency.ReorderEmergencyRes{Code: 0, Message: "success"}, nil
}
