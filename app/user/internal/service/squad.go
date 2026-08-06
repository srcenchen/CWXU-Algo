package service

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

// RegisterSquadRoutes 分队 + 管理范围 HTTP
func RegisterSquadRoutes(srv *khttp.Server, org *OrgService) {
	r := srv.Route("/")
	r.GET("/v1/user/org/squads", org.handleSquadList)
	r.POST("/v1/user/org/squads/create", org.handleSquadCreate)
	r.POST("/v1/user/org/squads/update", org.handleSquadUpdate)
	r.POST("/v1/user/org/squads/delete", org.handleSquadDelete)
	r.GET("/v1/user/org/squads/members", org.handleSquadMembers)
	r.POST("/v1/user/org/squads/members/set", org.handleSquadMemberSet)
	r.GET("/v1/user/org/scopes", org.handleScopeList)
	r.POST("/v1/user/org/scopes/set", org.handleScopeSet)
}

func (s *OrgService) currentOrgID(ctx khttp.Context, queryOrg string) uint {
	if id64, _ := strconv.ParseUint(strings.TrimSpace(queryOrg), 10, 64); id64 > 0 {
		return uint(id64)
	}
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		return pd.OrgID
	}
	return 0
}

// orgActorRole 当前用户在组织内的角色
func (s *OrgService) orgActorRole(ctx khttp.Context, orgID uint) string {
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
func (s *OrgService) canManageSquads(ctx khttp.Context, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgMemberRole) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgReportView)
}

// canWriteSquadStructure 建/改/删分队：组织管理员、教练（全组织）；组长仅本组
func (s *OrgService) canWriteSquadStructure(ctx khttp.Context, orgID, groupID uint) bool {
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
func (s *OrgService) canWriteSquadMembers(ctx khttp.Context, orgID uint, sq *model.Squad) bool {
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

// deprecated alias kept for call sites that mean structure write
func (s *OrgService) canWriteSquads(ctx khttp.Context, orgID uint) bool {
	return s.canWriteSquadStructure(ctx, orgID, 0) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage) ||
		auth.VerifySiteAdmin(ctx)
}

func (s *OrgService) handleSquadList(ctx khttp.Context) error {
	q := ctx.Request().URL.Query()
	orgID := s.currentOrgID(ctx, q.Get("orgId"))
	if orgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织"})
		return nil
	}
	if !s.canManageSquads(ctx, orgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	groupID, _ := strconv.ParseUint(strings.TrimSpace(q.Get("groupId")), 10, 64)
	tx := s.db.Where("org_id = ?", orgID)
	if groupID > 0 {
		tx = tx.Where("group_id = ?", groupID)
	}
	var list []model.Squad
	if err := tx.Order("id asc").Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
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
	out := make([]map[string]interface{}, 0, len(list))
	for _, sq := range list {
		out = append(out, map[string]interface{}{
			"id":          sq.ID,
			"orgId":       sq.OrgID,
			"groupId":     sq.GroupID,
			"name":        sq.Name,
			"describe":    sq.Describe,
			"memberCount": cntMap[sq.ID],
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "ok", "list": out})
	return nil
}

func (s *OrgService) handleSquadCreate(ctx khttp.Context) error {
	body, _ := io.ReadAll(ctx.Request().Body)
	var req struct {
		OrgID    uint   `json:"orgId"`
		GroupID  uint   `json:"groupId"`
		Name     string `json:"name"`
		Describe string `json:"describe"`
	}
	_ = json.Unmarshal(body, &req)
	if req.OrgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			req.OrgID = pd.OrgID
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.OrgID == 0 || req.GroupID == 0 || req.Name == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请填写分组与分队名称"})
		return nil
	}
	// 分组须属本组织
	var g model.Group
	if err := s.db.Where("id = ? AND org_id = ?", req.GroupID, req.OrgID).First(&g).Error; err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "分组不存在"})
		return nil
	}
	if !s.canWriteSquadStructure(ctx, req.OrgID, req.GroupID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	sq := model.Squad{
		OrgID:    req.OrgID,
		GroupID:  req.GroupID,
		Name:     req.Name,
		Describe: strings.TrimSpace(req.Describe),
	}
	if err := s.db.Create(&sq).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "创建失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已创建",
		"data": map[string]interface{}{"id": sq.ID, "orgId": sq.OrgID, "groupId": sq.GroupID, "name": sq.Name, "describe": sq.Describe},
	})
	return nil
}

func (s *OrgService) handleSquadUpdate(ctx khttp.Context) error {
	body, _ := io.ReadAll(ctx.Request().Body)
	var req struct {
		ID       uint   `json:"id"`
		Name     string `json:"name"`
		Describe string `json:"describe"`
		GroupID  uint   `json:"groupId"`
	}
	_ = json.Unmarshal(body, &req)
	var sq model.Squad
	if err := s.db.First(&sq, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "分队不存在"})
		return nil
	}
	targetGroup := sq.GroupID
	if req.GroupID > 0 {
		targetGroup = req.GroupID
	}
	if !s.canWriteSquadStructure(ctx, sq.OrgID, targetGroup) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		sq.Name = n
	}
	sq.Describe = strings.TrimSpace(req.Describe)
	if req.GroupID > 0 && req.GroupID != sq.GroupID {
		var g model.Group
		if err := s.db.Where("id = ? AND org_id = ?", req.GroupID, sq.OrgID).First(&g).Error; err != nil {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "目标分组不存在"})
			return nil
		}
		sq.GroupID = req.GroupID
	}
	if err := s.db.Save(&sq).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已保存"})
	return nil
}

func (s *OrgService) handleSquadDelete(ctx khttp.Context) error {
	body, _ := io.ReadAll(ctx.Request().Body)
	var req struct {
		ID uint `json:"id"`
	}
	_ = json.Unmarshal(body, &req)
	var sq model.Squad
	if err := s.db.First(&sq, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "分队不存在"})
		return nil
	}
	if !s.canWriteSquadStructure(ctx, sq.OrgID, sq.GroupID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
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
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

func (s *OrgService) handleSquadMembers(ctx khttp.Context) error {
	avatarBase := avatarPublicBase(s.db)
	squadID, _ := strconv.ParseUint(ctx.Request().URL.Query().Get("squadId"), 10, 64)
	if squadID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少分队 id"})
		return nil
	}
	var sq model.Squad
	if err := s.db.First(&sq, squadID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "分队不存在"})
		return nil
	}
	if !s.canManageSquads(ctx, sq.OrgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
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
	out := make([]map[string]interface{}, 0, len(members))
	for _, m := range members {
		u := uMap[m.UserID]
		display := nameMap[m.UserID]
		if display == "" {
			display = u.Name
		}
		if display == "" {
			display = u.Username
		}
		out = append(out, map[string]interface{}{
			"userId":   m.UserID,
			"username": u.Username,
			"name":     display,
			"avatar":   expandAvatarBase(avatarBase, u.Avatar),
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "ok", "list": out, "total": len(out)})
	return nil
}

func (s *OrgService) handleSquadMemberSet(ctx khttp.Context) error {
	body, _ := io.ReadAll(ctx.Request().Body)
	var req struct {
		SquadID uint `json:"squadId"`
		UserID  uint `json:"userId"`
		In      bool `json:"in"` // true=加入 false=移出
	}
	_ = json.Unmarshal(body, &req)
	var sq model.Squad
	if err := s.db.First(&sq, req.SquadID).Error; err != nil || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if !s.canWriteSquadMembers(ctx, sq.OrgID, &sq) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	// 须为本组织成员
	var cnt int64
	_ = s.db.Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", sq.OrgID, req.UserID).Count(&cnt).Error
	if cnt == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "对方不在本组织"})
		return nil
	}
	if req.In {
		// 允许多分队：不再踢出其它分队（一人可兼多队队长/队员）
		// 若当前无分组或在默认组，同步到该分队所属分组
		_ = s.db.Model(&model.OrgMember{}).
			Where("org_id = ? AND user_id = ? AND (group_id IS NULL OR group_id = 0)", sq.OrgID, req.UserID).
			Update("group_id", sq.GroupID).Error
		sm := model.SquadMember{SquadID: sq.ID, UserID: req.UserID}
		if err := s.db.Where("squad_id = ? AND user_id = ?", sq.ID, req.UserID).
			FirstOrCreate(&sm).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加入失败"})
			return nil
		}
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已加入分队"})
		return nil
	}
	_ = s.db.Where("squad_id = ? AND user_id = ?", sq.ID, req.UserID).Delete(&model.SquadMember{}).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已移出分队"})
	return nil
}

func (s *OrgService) handleScopeList(ctx khttp.Context) error {
	q := ctx.Request().URL.Query()
	orgID := s.currentOrgID(ctx, q.Get("orgId"))
	userID64, _ := strconv.ParseUint(q.Get("userId"), 10, 64)
	if orgID == 0 || userID64 == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织或用户"})
		return nil
	}
	// 本人或可任命角色者可看
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	if !auth.VerifySiteAdmin(ctx) && pd.UserID != uint(userID64) &&
		!auth.HasOrgPerm(ctx, orgID, rbac.PermOrgMemberRole) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	var grants []model.OrgScopeGrant
	_ = s.db.Where("org_id = ? AND user_id = ?", orgID, userID64).Find(&grants).Error
	out := make([]map[string]interface{}, 0, len(grants))
	for _, g := range grants {
		out = append(out, map[string]interface{}{
			"scopeType": g.ScopeType,
			"scopeId":   g.ScopeID,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "ok", "list": out})
	return nil
}

func (s *OrgService) handleScopeSet(ctx khttp.Context) error {
	body, _ := io.ReadAll(ctx.Request().Body)
	var req struct {
		OrgID  uint `json:"orgId"`
		UserID uint `json:"userId"`
		Grants []struct {
			ScopeType string `json:"scopeType"`
			ScopeID   uint   `json:"scopeId"`
		} `json:"grants"`
	}
	_ = json.Unmarshal(body, &req)
	if req.OrgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			req.OrgID = pd.OrgID
		}
	}
	if req.OrgID == 0 || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织或用户"})
		return nil
	}
	// 仅组织管理员 / 站管可手动改范围；组长/队长范围由任命决定
	if !auth.VerifySiteAdmin(ctx) {
		var actorRole string
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			_ = s.db.Model(&model.OrgMember{}).Select("role").
				Where("org_id = ? AND user_id = ?", req.OrgID, pd.UserID).Scan(&actorRole).Error
		}
		if actorRole != model.OrgRoleOrgAdmin {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "仅组织管理员可手动调整管理范围；组长/队长请通过任命指定"})
			return nil
		}
	}
	// 目标须在组织内
	var target model.OrgMember
	if s.db.Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).First(&target).Error != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "对方不在本组织"})
		return nil
	}
	// 教练始终全组织，禁止写入限制范围
	if model.IsOrgFullScopeRole(target.Role) {
		if len(req.Grants) > 0 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "教练与组织管理员始终可看全组织数据，无需设置管理范围"})
			return nil
		}
		// 清空遗留 grants
		_ = s.replaceUserScopeGrants(req.OrgID, req.UserID, nil)
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已更新管理范围", "count": 0})
		return nil
	}
	rows := make([]model.OrgScopeGrant, 0, len(req.Grants))
	seen := map[string]struct{}{}
	for _, g := range req.Grants {
		if !model.ValidScopeType(g.ScopeType) || g.ScopeID == 0 {
			continue
		}
		key := g.ScopeType + ":" + strconv.FormatUint(uint64(g.ScopeID), 10)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		// 校验 scope 属于本组织
		if g.ScopeType == model.ScopeTypeGroup {
			var n int64
			_ = s.db.Model(&model.Group{}).Where("id = ? AND org_id = ?", g.ScopeID, req.OrgID).Count(&n).Error
			if n == 0 {
				continue
			}
		} else {
			var n int64
			_ = s.db.Model(&model.Squad{}).Where("id = ? AND org_id = ?", g.ScopeID, req.OrgID).Count(&n).Error
			if n == 0 {
				continue
			}
		}
		rows = append(rows, model.OrgScopeGrant{
			OrgID:     req.OrgID,
			UserID:    req.UserID,
			ScopeType: g.ScopeType,
			ScopeID:   g.ScopeID,
		})
	}
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).Delete(&model.OrgScopeGrant{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已更新管理范围",
		"count": len(rows),
	})
	return nil
}
