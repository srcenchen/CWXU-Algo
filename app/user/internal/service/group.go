package service

import (
	"context"
	"cwxu-algo/api/core/v1/submit_log"
	"cwxu-algo/api/user/v1/group"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/biz"
	"cwxu-algo/app/user/internal/data/dal"
	"strconv"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	grpc2 "google.golang.org/grpc"
)

type GroupService struct {
	group.UnimplementedGroupServer
	reg          *discovery.Register
	groupUseCase *biz.GroupUseCase
	groupDal     *dal.GroupDal
}

// coreDataRPC 复用全进程共享的 core-data gRPC 长连接；调用方不得 Close。
func (g *GroupService) coreDataRPC() (*grpc2.ClientConn, error) {
	return sharedCoreDataConn(g.reg)
}

func (g *GroupService) Create(ctx context.Context, request *group.CreateRequest) (*group.CreateReply, error) {
	// 教练/队长/团队管理员/站点管理员均可建组
	if !auth.HasPerm(ctx, rbac.PermOrgGroupManage) {
		return nil, errors.Forbidden("权限不足", "需要分组管理权限")
	}
	if request.Name == "" {
		return nil, errors.BadRequest("参数错误", "组名称不能为空")
	}
	pd := auth.GetCurrentUser(ctx)
	orgID := uint(0)
	if pd != nil {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		return nil, errors.BadRequest("参数错误", "请先切换到目标组织后再创建分组")
	}
	id, err := g.groupUseCase.Create(ctx, request.Name, request.Describe, orgID)
	if err != nil {
		return nil, errors.InternalServer("创建失败", "服务暂时不可用")
	}
	return &group.CreateReply{
		Id:      id,
		Message: "创建成功",
	}, nil
}

func (g *GroupService) Delete(ctx context.Context, request *group.DeleteRequest) (*group.DeleteReply, error) {
	if !auth.HasPerm(ctx, rbac.PermOrgGroupManage) {
		return nil, errors.Forbidden("权限不足", "需要分组管理权限")
	}
	if request.Id == 0 {
		return nil, errors.BadRequest("参数错误", "组ID不能为空")
	}
	if err := g.assertGroupInCurrentOrg(ctx, request.Id); err != nil {
		return nil, err
	}
	err := g.groupUseCase.Delete(ctx, request.Id)
	if err != nil {
		return nil, errors.InternalServer("删除失败", "服务暂时不可用")
	}
	return &group.DeleteReply{Success: true}, nil
}

func (g *GroupService) Get(ctx context.Context, request *group.GetRequest) (*group.GetReply, error) {
	// 读：分组管理 或 训练报告（组长/队长需看组内结构）
	if !auth.HasPerm(ctx, rbac.PermOrgGroupManage) && !auth.HasPerm(ctx, rbac.PermOrgReportView) {
		return nil, errors.Forbidden("权限不足", "需要分组管理或训练报告权限")
	}
	if err := g.assertGroupInCurrentOrg(ctx, request.Id); err != nil {
		return nil, err
	}
	page := request.Page
	if page < 1 {
		page = 1
	}
	pageSize := request.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	groupModel, users, total, err := g.groupUseCase.GetWithUsers(ctx, request.Id, page, pageSize)
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}

	name := ""
	if groupModel.Name != nil {
		name = *groupModel.Name
	}

	reply := &group.GetReply{
		Id:       int64(groupModel.ID),
		Name:     name,
		Describe: groupModel.Describe,
		Users:    make([]*group.User, 0),
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}

	if len(users) > 0 {
		userIds := make([]int64, 0, len(users))
		uids := make([]uint, 0, len(users))
		for _, u := range users {
			userIds = append(userIds, int64(u.ID))
			uids = append(uids, u.ID)
		}

		// 组内展示优先组织内名称
		displayByUID := g.groupDal.OrgDisplayNames(ctx, groupModel.OrgID, uids)

		timeMap := map[int64]int64{}
		conn, err := g.coreDataRPC()
		if err != nil {
			log.Info(err.Error())
		} else {
			sb := submit_log.NewSubmitClient(conn)
			sp, err := sb.LastSubmitTime(ctx, &submit_log.LastSubmitTimeReq{UserIds: userIds})
			if err == nil {
				_ = utils.GobDecoder(sp.TimeMap, &timeMap)
			}
		}
		for _, u := range users {
			lastSubmit := ""
			if t, ok := timeMap[int64(u.ID)]; ok {
				lastSubmit = strconv.Itoa(int(t))
			}
			display := displayByUID[u.ID]
			if display == "" {
				display = u.Username
			}
			reply.Users = append(reply.Users, &group.User{
				UserId:     uint64(u.ID),
				Username:   u.Username,
				Name:       display,
				GroupId:    u.GroupId,
				Avatar:     u.Avatar,
				LastSubmit: lastSubmit,
			})
		}
	}

	return reply, nil
}

func (g *GroupService) List(ctx context.Context, request *group.ListRequest) (*group.ListReply, error) {
	// 读：分组管理 或 训练报告（教练/组长/队长任命与筛选需要列表）
	if !auth.HasPerm(ctx, rbac.PermOrgGroupManage) && !auth.HasPerm(ctx, rbac.PermOrgReportView) {
		return nil, errors.Forbidden("权限不足", "需要分组管理或训练报告权限")
	}
	page := request.Page
	if page < 1 {
		page = 1
	}
	size := request.Size
	if size < 1 {
		size = 10
	}
	orgID := uint(0)
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		return nil, errors.Forbidden("权限不足", "请先选择组织")
	}
	// 当前组织无分组时自动补「默认分组」
	if orgID > 0 {
		_, _ = g.groupDal.EnsureDefaultGroup(ctx, orgID)
	}
	list, total, err := g.groupUseCase.List(ctx, page, size, orgID)
	if err != nil {
		return nil, errors.InternalServer("查询失败", "服务暂时不可用")
	}
	reply := &group.ListReply{List: make([]*group.GetReply, 0, len(list)), Total: total}
	for _, gr := range list {
		name := ""
		if gr.Name != nil {
			name = *gr.Name
			if name == "未分组" {
				name = "默认分组"
			}
		}
		reply.List = append(reply.List, &group.GetReply{
			Id:       int64(gr.ID),
			Name:     name,
			Describe: gr.Describe,
		})
	}
	// 若列表空且有 org，再确保默认分组
	if len(reply.List) == 0 && orgID > 0 {
		if id, e := g.groupDal.EnsureDefaultGroup(ctx, orgID); e == nil && id > 0 {
			reply.List = append(reply.List, &group.GetReply{
				Id:       int64(id),
				Name:     "默认分组",
				Describe: "组织默认分组",
			})
			reply.Total = 1
		}
	}
	return reply, nil
}

func (g *GroupService) Update(ctx context.Context, request *group.UpdateRequest) (*group.UpdateReply, error) {
	if !auth.HasPerm(ctx, rbac.PermOrgGroupManage) {
		return nil, errors.Forbidden("权限不足", "需要分组管理权限")
	}
	if request.Id == 0 {
		return nil, errors.BadRequest("参数错误", "组ID不能为空")
	}
	if err := g.assertGroupInCurrentOrg(ctx, request.Id); err != nil {
		return nil, err
	}
	if request.Name == "" && request.Describe == "" {
		return nil, errors.BadRequest("参数错误", "至少更新一个字段")
	}
	err := g.groupUseCase.Update(ctx, request.Id, request.Name, request.Describe)
	if err != nil {
		return nil, errors.InternalServer("更新失败", "服务暂时不可用")
	}
	return &group.UpdateReply{Success: true}, nil
}

func (g *GroupService) assertGroupInCurrentOrg(ctx context.Context, groupID int64) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.OrgID == 0 {
		return errors.Forbidden("权限不足", "请先选择组织")
	}
	grp, err := g.groupUseCase.Get(ctx, groupID)
	if err != nil {
		return errors.NotFound("不存在", "分组不存在")
	}
	if grp.OrgID != pd.OrgID {
		return errors.Forbidden("权限不足", "不能操作其他组织的分组")
	}
	return nil
}

func NewGroupService(reg *discovery.Register, groupUseCase *biz.GroupUseCase, groupDal *dal.GroupDal) *GroupService {
	return &GroupService{
		reg:          reg,
		groupUseCase: groupUseCase,
		groupDal:     groupDal,
	}
}
