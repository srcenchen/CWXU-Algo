package service

import (
	"context"
	"cwxu-algo/api/core/v1/bulletin"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/errors"
	"gorm.io/gorm"
)

type BulletinService struct {
	bulletin.UnimplementedBulletinServer
	bulletinDal *dal.BulletinDal
}

func NewBulletinService(bulletinDal *dal.BulletinDal) *BulletinService {
	return &BulletinService{bulletinDal: bulletinDal}
}

// modelToProto 将 GORM 模型转换为 proto 消息
func (s *BulletinService) modelToProto(m *model.Bulletin) *bulletin.BulletinInfo {
	scope := m.Scope
	if scope == "" {
		scope = model.BulletinScopeSite
	}
	info := &bulletin.BulletinInfo{
		Id:         m.ID,
		Title:      m.Title,
		Content:    m.Content,
		AuthorId:   m.AuthorID,
		AuthorName: m.AuthorName,
		IsPinned:   m.IsPinned,
		CreatedAt:  m.CreatedAt.Unix(),
		UpdatedAt:  m.UpdatedAt.Unix(),
		Scope:      scope,
	}
	if m.OrgID != nil {
		info.OrgId = int64(*m.OrgID)
	}
	return info
}

// canManageBulletin 站点公告需全站公告权限；组织公告需该组织的组织公告权限（站管旁路）
func canManageBulletin(ctx context.Context, user *auth.JwtPayload, m *model.Bulletin) bool {
	if user == nil {
		return false
	}
	if auth.HasPerm(ctx, rbac.PermSiteBulletin) {
		return true
	}
	scope := m.Scope
	if scope == "" {
		scope = model.BulletinScopeSite
	}
	// 全站公告需全站公告权限
	if scope == model.BulletinScopeSite {
		return false
	}
	// 组织公告：对该组织具备组织公告权限（要求 JWT 当前组织匹配）
	if scope == model.BulletinScopeOrg && m.OrgID != nil {
		return auth.HasOrgPerm(ctx, *m.OrgID, rbac.PermOrgBulletinManage)
	}
	return false
}

// Create 创建公告
// - scope=site：仅站点管理员
// - scope=org 或空：组织教练及以上，写入当前 JWT 组织
func (s *BulletinService) Create(ctx context.Context, req *bulletin.CreateBulletinReq) (*bulletin.CreateBulletinRes, error) {
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return &bulletin.CreateBulletinRes{Code: 1, Message: "未获取到用户信息"}, nil
	}

	reqScope := req.Scope
	if reqScope == "" {
		// 默认：具备全站公告权限者发站点公告；其余发组织公告
		if auth.HasPerm(ctx, rbac.PermSiteBulletin) {
			reqScope = model.BulletinScopeSite
		} else {
			reqScope = model.BulletinScopeOrg
		}
	}

	var scope string
	var orgID *uint

	switch reqScope {
	case model.BulletinScopeSite:
		if !auth.HasPerm(ctx, rbac.PermSiteBulletin) {
			return &bulletin.CreateBulletinRes{
				Code:    1,
				Message: "权限不足，仅站点管理员可发布站点公告",
			}, nil
		}
		scope = model.BulletinScopeSite
		orgID = nil
	case model.BulletinScopeOrg:
		// 组织公告：需当前组织的组织公告权限；站管也可代发当前组织公告
		if !auth.HasPerm(ctx, rbac.PermOrgBulletinManage) {
			return &bulletin.CreateBulletinRes{
				Code:    1,
				Message: "权限不足，仅组织管理员或教练可发布组织公告",
			}, nil
		}
		if user.OrgID == 0 {
			return &bulletin.CreateBulletinRes{
				Code:    1,
				Message: "请先切换到要发布公告的组织",
			}, nil
		}
		scope = model.BulletinScopeOrg
		oid := user.OrgID
		orgID = &oid
	default:
		return &bulletin.CreateBulletinRes{
			Code:    2,
			Message: "无效的公告范围",
		}, nil
	}

	if req.Title == "" {
		return &bulletin.CreateBulletinRes{Code: 2, Message: "标题不能为空"}, nil
	}
	if req.Content == "" {
		return &bulletin.CreateBulletinRes{Code: 3, Message: "内容不能为空"}, nil
	}

	m := &model.Bulletin{
		Title:      req.Title,
		Content:    req.Content,
		AuthorID:   int64(user.UserID),
		AuthorName: user.Name,
		IsPinned:   req.IsPinned,
		Scope:      scope,
		OrgID:      orgID,
	}
	if err := s.bulletinDal.Create(m); err != nil {
		return nil, errors.InternalServer("创建失败", "服务暂时不可用")
	}

	return &bulletin.CreateBulletinRes{
		Code:    0,
		Message: "success",
		Data:    s.modelToProto(m),
	}, nil
}

// Update 更新公告
func (s *BulletinService) Update(ctx context.Context, req *bulletin.UpdateBulletinReq) (*bulletin.UpdateBulletinRes, error) {
	user := auth.GetCurrentUser(ctx)
	existing, err := s.bulletinDal.GetById(req.Id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return &bulletin.UpdateBulletinRes{Code: 2, Message: "公告不存在"}, nil
		}
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	if !canManageBulletin(ctx, user, existing) {
		return &bulletin.UpdateBulletinRes{
			Code:    1,
			Message: "权限不足",
		}, nil
	}

	updates := make(map[string]interface{})
	if req.Title != "" {
		updates["title"] = req.Title
	}
	if req.Content != "" {
		updates["content"] = req.Content
	}
	updates["is_pinned"] = req.IsPinned

	if len(updates) == 0 {
		return &bulletin.UpdateBulletinRes{
			Code:    3,
			Message: "无需更新的字段",
		}, nil
	}

	if err := s.bulletinDal.Update(req.Id, updates); err != nil {
		return nil, errors.InternalServer("更新失败", "服务暂时不可用")
	}

	updated, err := s.bulletinDal.GetById(req.Id)
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}

	return &bulletin.UpdateBulletinRes{
		Code:    0,
		Message: "success",
		Data:    s.modelToProto(updated),
	}, nil
}

// Delete 删除公告
func (s *BulletinService) Delete(ctx context.Context, req *bulletin.DeleteBulletinReq) (*bulletin.DeleteBulletinRes, error) {
	user := auth.GetCurrentUser(ctx)
	existing, err := s.bulletinDal.GetById(req.Id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return &bulletin.DeleteBulletinRes{Code: 2, Message: "公告不存在"}, nil
		}
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	if !canManageBulletin(ctx, user, existing) {
		return &bulletin.DeleteBulletinRes{Code: 1, Message: "权限不足"}, nil
	}

	if err := s.bulletinDal.Delete(req.Id); err != nil {
		return nil, errors.InternalServer("删除失败", "服务暂时不可用")
	}

	return &bulletin.DeleteBulletinRes{
		Code:    0,
		Message: "success",
	}, nil
}

// Get 获取公告详情
// 站点公告公开；组织公告仅站点管理员或当前 JWT 组织匹配时可看（与 List 隔离一致）
func (s *BulletinService) Get(ctx context.Context, req *bulletin.GetBulletinReq) (*bulletin.GetBulletinRes, error) {
	m, err := s.bulletinDal.GetById(req.Id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return &bulletin.GetBulletinRes{
				Code:    2,
				Message: "公告不存在",
			}, nil
		}
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}

	scope := m.Scope
	if scope == "" {
		scope = model.BulletinScopeSite
	}
	if scope == model.BulletinScopeOrg {
		if !auth.HasPerm(ctx, rbac.PermSiteBulletin) {
			u := auth.GetCurrentUser(ctx)
			if u == nil || m.OrgID == nil || u.OrgID != *m.OrgID {
				return &bulletin.GetBulletinRes{
					Code:    2,
					Message: "公告不存在",
				}, nil
			}
		}
	}

	return &bulletin.GetBulletinRes{
		Code:    0,
		Message: "success",
		Data:    s.modelToProto(m),
	}, nil
}

// List 分页获取公告列表
// scope 空：全站 ∪ 当前组织；scope=site / org 仅对应范围
func (s *BulletinService) List(ctx context.Context, req *bulletin.ListBulletinReq) (*bulletin.ListBulletinRes, error) {
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

	orgID := uint(0)
	if u := auth.GetCurrentUser(ctx); u != nil {
		orgID = u.OrgID
	}

	scope := req.Scope
	// 管理端请求 org 范围但无组织上下文时，不泄露其他组织公告
	if scope == model.BulletinScopeOrg && orgID == 0 {
		return &bulletin.ListBulletinRes{
			Code:     0,
			Message:  "success",
			Data:     []*bulletin.BulletinInfo{},
			Total:    0,
			Page:     page,
			PageSize: pageSize,
		}, nil
	}

	bulletins, total, err := s.bulletinDal.List(page, pageSize, orgID, scope)
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}

	data := make([]*bulletin.BulletinInfo, 0, len(bulletins))
	for i := range bulletins {
		data = append(data, s.modelToProto(&bulletins[i]))
	}

	return &bulletin.ListBulletinRes{
		Code:     0,
		Message:  "success",
		Data:     data,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}
