package service

import (
	"context"
	"strconv"
	"strings"

	orgpb "cwxu-algo/api/user/v1/org"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/gorm"
)

func (s *OrgService) currentOrgID(ctx context.Context, queryOrg int64) uint {
	if queryOrg > 0 {
		return uint(queryOrg)
	}
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		return pd.OrgID
	}
	return 0
}

// orgActorRole 当前用户在组织内的角色
func (s *OrgService) orgActorRole(ctx context.Context, orgID uint) string {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return ""
	}
	var role string
	_ = s.db.Model(&model.OrgMember{}).Select("role").
		Where("org_id = ? AND user_id = ?", orgID, pd.UserID).Scan(&role).Error
	return role
}

// canManageSquads 可读分队列表：组织 staff / 分组管理 / 报告
func (s *OrgService) canManageSquads(ctx context.Context, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgMemberRole) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgReportView)
}

// canWriteSquadStructure 建/改/删分队：组织管理员、教练（全组织）；组长仅本组
func (s *OrgService) canWriteSquadStructure(ctx context.Context, orgID, groupID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	if auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage) {
		return true
	}
	role := s.orgActorRole(ctx, orgID)
	if role == model.OrgRoleGroupLeader && groupID > 0 {
		pd := auth.GetCurrentUser(ctx)
		if pd != nil {
			return s.actorControlsGroup(orgID, pd.UserID, groupID)
		}
	}
	return false
}

// canWriteSquadMembers 调整分队成员：全组织写权限 / 组长（本组）/ 队长（本分队）
func (s *OrgService) canWriteSquadMembers(ctx context.Context, orgID uint, sq *model.Squad) bool {
	if sq == nil {
		return false
	}
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	if auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage) {
		return true
	}
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return false
	}
	role := s.orgActorRole(ctx, orgID)
	switch role {
	case model.OrgRoleGroupLeader:
		return s.actorControlsGroup(orgID, pd.UserID, sq.GroupID)
	case model.OrgRoleCaptain:
		var n int64
		_ = s.db.Model(&model.OrgScopeGrant{}).
			Where("org_id = ? AND user_id = ? AND scope_type = ? AND scope_id = ?",
				orgID, pd.UserID, model.ScopeTypeSquad, sq.ID).Count(&n).Error
		return n > 0
	}
	return false
}

func (s *OrgService) SquadList(ctx context.Context, req *orgpb.SquadListReq) (*orgpb.SquadListRes, error) {
	orgID := s.currentOrgID(ctx, req.OrgId)
	if orgID == 0 {
		return &orgpb.SquadListRes{Code: 1, Message: "缺少组织"}, nil
	}
	if !s.canManageSquads(ctx, orgID) {
		return &orgpb.SquadListRes{Code: 1, Message: "权限不足"}, nil
	}
	groupID := uint(req.GroupId)
	tx := s.db.Where("org_id = ?", orgID)
	if groupID > 0 {
		tx = tx.Where("group_id = ?", groupID)
	}
	var list []model.Squad
	if err := tx.Order("id asc").Find(&list).Error; err != nil {
		return &orgpb.SquadListRes{Code: 1, Message: "加载失败"}, nil
	}
	// 人数
	type row struct {
		SquadID uint
		Cnt     int64
	}
	var counts []row
	_ = s.db.Table("squad_members sm").
		Select("sm.squad_id as squad_id, count(*) as cnt").
		Joins("JOIN squads s ON s.id = sm.squad_id").
		Where("s.org_id = ?", orgID).
		Group("sm.squad_id").
		Scan(&counts)
	cntMap := map[uint]int64{}
	for _, c := range counts {
		cntMap[c.SquadID] = c.Cnt
	}
	out := make([]*orgpb.SquadInfo, 0, len(list))
	for _, sq := range list {
		out = append(out, &orgpb.SquadInfo{
			Id:          int64(sq.ID),
			OrgId:       int64(sq.OrgID),
			GroupId:     int64(sq.GroupID),
			Name:        sq.Name,
			Describe:    sq.Describe,
			MemberCount: int32(cntMap[sq.ID]),
		})
	}
	return &orgpb.SquadListRes{Code: 0, Message: "ok", List: out}, nil
}

func (s *OrgService) SquadCreate(ctx context.Context, req *orgpb.SquadCreateReq) (*orgpb.SquadCreateRes, error) {
	orgID := uint(req.OrgId)
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			orgID = pd.OrgID
		}
	}
	name := strings.TrimSpace(req.Name)
	if orgID == 0 || req.GroupId == 0 || name == "" {
		return &orgpb.SquadCreateRes{Code: 1, Message: "请填写分组与分队名称"}, nil
	}
	// 分组须属本组织
	groupID := uint(req.GroupId)
	var g model.Group
	if err := s.db.Where("id = ? AND org_id = ?", groupID, orgID).First(&g).Error; err != nil {
		return &orgpb.SquadCreateRes{Code: 1, Message: "分组不存在"}, nil
	}
	if !s.canWriteSquadStructure(ctx, orgID, groupID) {
		return &orgpb.SquadCreateRes{Code: 1, Message: "权限不足"}, nil
	}
	sq := model.Squad{
		OrgID:    orgID,
		GroupID:  groupID,
		Name:     name,
		Describe: strings.TrimSpace(req.Describe),
	}
	if err := s.db.Create(&sq).Error; err != nil {
		return &orgpb.SquadCreateRes{Code: 1, Message: "创建失败"}, nil
	}
	return &orgpb.SquadCreateRes{
		Code: 0, Message: "已创建",
		Data: &orgpb.SquadData{Id: int64(sq.ID), OrgId: int64(sq.OrgID), GroupId: int64(sq.GroupID), Name: sq.Name, Describe: sq.Describe},
	}, nil
}

func (s *OrgService) SquadUpdate(ctx context.Context, req *orgpb.SquadUpdateReq) (*orgpb.SquadUpdateRes, error) {
	var sq model.Squad
	if err := s.db.First(&sq, req.Id).Error; err != nil {
		return &orgpb.SquadUpdateRes{Code: 1, Message: "分队不存在"}, nil
	}
	targetGroup := sq.GroupID
	if req.GroupId > 0 {
		targetGroup = uint(req.GroupId)
	}
	if !s.canWriteSquadStructure(ctx, sq.OrgID, targetGroup) {
		return &orgpb.SquadUpdateRes{Code: 1, Message: "权限不足"}, nil
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		sq.Name = n
	}
	sq.Describe = strings.TrimSpace(req.Describe)
	if req.GroupId > 0 && uint(req.GroupId) != sq.GroupID {
		var g model.Group
		if err := s.db.Where("id = ? AND org_id = ?", req.GroupId, sq.OrgID).First(&g).Error; err != nil {
			return &orgpb.SquadUpdateRes{Code: 1, Message: "目标分组不存在"}, nil
		}
		sq.GroupID = uint(req.GroupId)
	}
	if err := s.db.Save(&sq).Error; err != nil {
		return &orgpb.SquadUpdateRes{Code: 1, Message: "保存失败"}, nil
	}
	return &orgpb.SquadUpdateRes{Code: 0, Message: "已保存"}, nil
}

func (s *OrgService) SquadDelete(ctx context.Context, req *orgpb.SquadDeleteReq) (*orgpb.SquadDeleteRes, error) {
	var sq model.Squad
	if err := s.db.First(&sq, req.Id).Error; err != nil {
		return &orgpb.SquadDeleteRes{Code: 1, Message: "分队不存在"}, nil
	}
	if !s.canWriteSquadStructure(ctx, sq.OrgID, sq.GroupID) {
		return &orgpb.SquadDeleteRes{Code: 1, Message: "权限不足"}, nil
	}
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		// 卸任仅管此分队的队长
		var captainUIDs []uint
		_ = tx.Model(&model.OrgScopeGrant{}).
			Where("org_id = ? AND scope_type = ? AND scope_id = ?", sq.OrgID, model.ScopeTypeSquad, sq.ID).
			Pluck("user_id", &captainUIDs).Error
		if len(captainUIDs) > 0 {
			_ = tx.Model(&model.OrgMember{}).
				Where("org_id = ? AND user_id IN ? AND role = ?", sq.OrgID, captainUIDs, model.OrgRoleCaptain).
				Update("role", model.OrgRoleMember).Error
		}
		_ = tx.Where("squad_id = ?", sq.ID).Delete(&model.SquadMember{}).Error
		_ = tx.Where("scope_type = ? AND scope_id = ? AND org_id = ?", model.ScopeTypeSquad, sq.ID, sq.OrgID).
			Delete(&model.OrgScopeGrant{}).Error
		return tx.Delete(&sq).Error
	})
	return &orgpb.SquadDeleteRes{Code: 0, Message: "已删除"}, nil
}

func (s *OrgService) SquadMembers(ctx context.Context, req *orgpb.SquadMembersReq) (*orgpb.SquadMembersRes, error) {
	avatarBase := avatarPublicBase(s.db)
	squadID := uint(req.SquadId)
	if squadID == 0 {
		return &orgpb.SquadMembersRes{Code: 1, Message: "缺少分队 id"}, nil
	}
	var sq model.Squad
	if err := s.db.First(&sq, squadID).Error; err != nil {
		return &orgpb.SquadMembersRes{Code: 1, Message: "分队不存在"}, nil
	}
	if !s.canManageSquads(ctx, sq.OrgID) {
		return &orgpb.SquadMembersRes{Code: 1, Message: "权限不足"}, nil
	}
	var members []model.SquadMember
	_ = s.db.Where("squad_id = ?", squadID).Find(&members).Error
	uids := make([]uint, 0, len(members))
	for _, m := range members {
		uids = append(uids, m.UserID)
	}
	type urow struct {
		ID       uint
		Username string
		Name     string
		Avatar   string
	}
	var users []urow
	if len(uids) > 0 {
		_ = s.db.Table("users").Select("id, username, name, avatar").Where("id IN ?", uids).Scan(&users).Error
	}
	// 组织内名称
	nameMap := map[uint]string{}
	if len(uids) > 0 {
		var ms []model.OrgMember
		_ = s.db.Where("org_id = ? AND user_id IN ?", sq.OrgID, uids).Find(&ms).Error
		for _, m := range ms {
			if strings.TrimSpace(m.OrgDisplayName) != "" {
				nameMap[m.UserID] = m.OrgDisplayName
			}
		}
	}
	uMap := map[uint]urow{}
	for _, u := range users {
		uMap[u.ID] = u
	}
	out := make([]*orgpb.SquadMemberInfo, 0, len(members))
	for _, m := range members {
		u := uMap[m.UserID]
		display := nameMap[m.UserID]
		if display == "" {
			display = u.Name
		}
		if display == "" {
			display = u.Username
		}
		out = append(out, &orgpb.SquadMemberInfo{
			UserId:   int64(m.UserID),
			Username: u.Username,
			Name:     display,
			Avatar:   expandAvatarBase(avatarBase, u.Avatar),
		})
	}
	return &orgpb.SquadMembersRes{Code: 0, Message: "ok", List: out, Total: int32(len(out))}, nil
}

func (s *OrgService) SquadMemberSet(ctx context.Context, req *orgpb.SquadMemberSetReq) (*orgpb.SquadMemberSetRes, error) {
	var sq model.Squad
	if err := s.db.First(&sq, req.SquadId).Error; err != nil || req.UserId == 0 {
		return &orgpb.SquadMemberSetRes{Code: 1, Message: "参数错误"}, nil
	}
	if !s.canWriteSquadMembers(ctx, sq.OrgID, &sq) {
		return &orgpb.SquadMemberSetRes{Code: 1, Message: "权限不足"}, nil
	}
	// 须为本组织成员
	userID := uint(req.UserId)
	var cnt int64
	_ = s.db.Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", sq.OrgID, userID).Count(&cnt).Error
	if cnt == 0 {
		return &orgpb.SquadMemberSetRes{Code: 1, Message: "对方不在本组织"}, nil
	}
	if req.In {
		// 允许多分队：不再踢出其它分队（一人可兼多队队长/队员）
		// 若当前无分组或在默认组，同步到该分队所属分组
		_ = s.db.Model(&model.OrgMember{}).
			Where("org_id = ? AND user_id = ? AND (group_id IS NULL OR group_id = 0)", sq.OrgID, userID).
			Update("group_id", sq.GroupID).Error
		sm := model.SquadMember{SquadID: sq.ID, UserID: userID}
		if err := s.db.Where("squad_id = ? AND user_id = ?", sq.ID, userID).
			FirstOrCreate(&sm).Error; err != nil {
			return &orgpb.SquadMemberSetRes{Code: 1, Message: "加入失败"}, nil
		}
		return &orgpb.SquadMemberSetRes{Code: 0, Message: "已加入分队"}, nil
	}
	_ = s.db.Where("squad_id = ? AND user_id = ?", sq.ID, userID).Delete(&model.SquadMember{}).Error
	return &orgpb.SquadMemberSetRes{Code: 0, Message: "已移出分队"}, nil
}

func (s *OrgService) ScopeList(ctx context.Context, req *orgpb.ScopeListReq) (*orgpb.ScopeListRes, error) {
	orgID := s.currentOrgID(ctx, req.OrgId)
	userID := uint(req.UserId)
	if orgID == 0 || userID == 0 {
		return &orgpb.ScopeListRes{Code: 1, Message: "缺少组织或用户"}, nil
	}
	// 本人或可任命角色者可看
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.ScopeListRes{Code: 1, Message: "请先登录"}, nil
	}
	if !auth.VerifySiteAdmin(ctx) && pd.UserID != userID &&
		!auth.HasOrgPerm(ctx, orgID, rbac.PermOrgMemberRole) {
		return &orgpb.ScopeListRes{Code: 1, Message: "权限不足"}, nil
	}
	var grants []model.OrgScopeGrant
	_ = s.db.Where("org_id = ? AND user_id = ?", orgID, userID).Find(&grants).Error
	out := make([]*orgpb.ScopeRef, 0, len(grants))
	for _, g := range grants {
		out = append(out, &orgpb.ScopeRef{
			ScopeType: g.ScopeType,
			ScopeId:   int64(g.ScopeID),
		})
	}
	return &orgpb.ScopeListRes{Code: 0, Message: "ok", List: out}, nil
}

func (s *OrgService) ScopeSet(ctx context.Context, req *orgpb.ScopeSetReq) (*orgpb.ScopeSetRes, error) {
	orgID := uint(req.OrgId)
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			orgID = pd.OrgID
		}
	}
	userID := uint(req.UserId)
	if orgID == 0 || userID == 0 {
		return &orgpb.ScopeSetRes{Code: 1, Message: "缺少组织或用户"}, nil
	}
	// 仅组织管理员 / 站管可手动改范围；组长/队长范围由任命决定
	if !auth.VerifySiteAdmin(ctx) {
		var actorRole string
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			_ = s.db.Model(&model.OrgMember{}).Select("role").
				Where("org_id = ? AND user_id = ?", orgID, pd.UserID).Scan(&actorRole).Error
		}
		if actorRole != model.OrgRoleOrgAdmin {
			return &orgpb.ScopeSetRes{Code: 1, Message: "仅组织管理员可手动调整管理范围；组长/队长请通过任命指定"}, nil
		}
	}
	// 目标须在组织内
	var target model.OrgMember
	if s.db.Where("org_id = ? AND user_id = ?", orgID, userID).First(&target).Error != nil {
		return &orgpb.ScopeSetRes{Code: 1, Message: "对方不在本组织"}, nil
	}
	// 教练始终全组织，禁止写入限制范围
	if model.IsOrgFullScopeRole(target.Role) {
		if len(req.Grants) > 0 {
			return &orgpb.ScopeSetRes{Code: 1, Message: "教练与组织管理员始终可看全组织数据，无需设置管理范围"}, nil
		}
		// 清空遗留 grants
		_ = s.replaceUserScopeGrants(orgID, userID, nil)
		return &orgpb.ScopeSetRes{Code: 0, Message: "已更新管理范围", Count: 0}, nil
	}
	rows := make([]model.OrgScopeGrant, 0, len(req.Grants))
	seen := map[string]struct{}{}
	for _, g := range req.Grants {
		if !model.ValidScopeType(g.ScopeType) || g.ScopeId == 0 {
			continue
		}
		key := g.ScopeType + ":" + strconv.FormatUint(uint64(g.ScopeId), 10)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		// 校验 scope 属于本组织
		if g.ScopeType == model.ScopeTypeGroup {
			var n int64
			_ = s.db.Model(&model.Group{}).Where("id = ? AND org_id = ?", g.ScopeId, orgID).Count(&n).Error
			if n == 0 {
				continue
			}
		} else {
			var n int64
			_ = s.db.Model(&model.Squad{}).Where("id = ? AND org_id = ?", g.ScopeId, orgID).Count(&n).Error
			if n == 0 {
				continue
			}
		}
		rows = append(rows, model.OrgScopeGrant{
			OrgID:     orgID,
			UserID:    userID,
			ScopeType: g.ScopeType,
			ScopeID:   uint(g.ScopeId),
		})
	}
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ? AND user_id = ?", orgID, userID).Delete(&model.OrgScopeGrant{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
	return &orgpb.ScopeSetRes{
		Code: 0, Message: "已更新管理范围",
		Count: int32(len(rows)),
	}, nil
}
