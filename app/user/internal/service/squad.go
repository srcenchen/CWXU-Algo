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

func (s *OrgService) canManageSquads(ctx khttp.Context, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgMemberRole) ||
		auth.HasOrgPerm(ctx, orgID, rbac.PermOrgReportView)
}

func (s *OrgService) canWriteSquads(ctx khttp.Context, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return auth.HasOrgPerm(ctx, orgID, rbac.PermOrgGroupManage)
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
	if !s.canWriteSquads(ctx, req.OrgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	// 分组须属本组织
	var g model.Group
	if err := s.db.Where("id = ? AND org_id = ?", req.GroupID, req.OrgID).First(&g).Error; err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "分组不存在"})
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
	if !s.canWriteSquads(ctx, sq.OrgID) {
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
	if !s.canWriteSquads(ctx, sq.OrgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		_ = tx.Where("squad_id = ?", sq.ID).Delete(&model.SquadMember{}).Error
		_ = tx.Where("scope_type = ? AND scope_id = ? AND org_id = ?", model.ScopeTypeSquad, sq.ID, sq.OrgID).
			Delete(&model.OrgScopeGrant{}).Error
		return tx.Delete(&sq).Error
	})
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

func (s *OrgService) handleSquadMembers(ctx khttp.Context) error {
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
			"avatar":   u.Avatar,
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
	if !s.canWriteSquads(ctx, sq.OrgID) {
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
		// 同一组织内先退出其它分队，再加入
		_ = s.db.Exec(`
			DELETE FROM squad_members WHERE user_id = ? AND squad_id IN (
				SELECT id FROM squads WHERE org_id = ?
			)`, req.UserID, sq.OrgID).Error
		// 同步分组成员到该分队所属分组
		_ = s.db.Model(&model.OrgMember{}).
			Where("org_id = ? AND user_id = ?", sq.OrgID, req.UserID).
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
	if !auth.VerifySiteAdmin(ctx) && !auth.HasOrgPerm(ctx, req.OrgID, rbac.PermOrgMemberRole) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	// 目标须在组织内
	var cnt int64
	_ = s.db.Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).Count(&cnt).Error
	if cnt == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "对方不在本组织"})
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
