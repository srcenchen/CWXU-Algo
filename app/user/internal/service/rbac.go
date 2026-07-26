package service

import (
	"strconv"
	"strings"

	"cwxu-algo/app/common/permission"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// RbacService 角色与权限管理（细粒度 RBAC）。
// 内置角色权限集代码锁定；本服务只管理自定义角色与其成员指派。
// 内置角色的任命仍走既有入口（org members/set-role、platform/set-*），并双写 user_roles 镜像。
type RbacService struct {
	db  *gorm.DB
	rdb *redis.Client
}

func NewRbacService(d *data.Data) *RbacService {
	return &RbacService{db: d.DB, rdb: d.RDB}
}

// RegisterRbacRoutes HTTP 路由（与 org 同模式）
func RegisterRbacRoutes(srv *khttp.Server, s *RbacService) {
	r := srv.Route("/")
	r.GET("/v1/user/rbac/permissions", s.handlePermissions)
	r.GET("/v1/user/rbac/roles", s.handleRoles)
	r.POST("/v1/user/rbac/roles/create", s.handleRoleCreate)
	r.POST("/v1/user/rbac/roles/update", s.handleRoleUpdate)
	r.POST("/v1/user/rbac/roles/delete", s.handleRoleDelete)
	r.GET("/v1/user/rbac/roles/members", s.handleRoleMembers)
	r.POST("/v1/user/rbac/roles/assign", s.handleRoleAssign)
	r.POST("/v1/user/rbac/roles/unassign", s.handleRoleUnassign)
	r.GET("/v1/user/rbac/my-permissions", s.handleMyPermissions)
}

// —— 内置组织角色的组织级权限覆盖（教练 / 队长）——

// orgRoleOverride 读取组织对内置角色的权限覆盖；ok=false 表示该组织未自定义，用代码模板。
func orgRoleOverride(db *gorm.DB, orgID uint, roleCode string) (perms []string, ok bool) {
	if db == nil || orgID == 0 || !rbac.OrgEditableSystemRole(roleCode) {
		return nil, false
	}
	var row model.OrgRolePerm
	if db.Where("org_id = ? AND role_code = ?", orgID, roleCode).First(&row).Error != nil {
		return nil, false
	}
	return splitPermCodes(row.PermCodes), true
}

// splitPermCodes 覆盖行的 perm_codes 文本 → 有效权限 code 列表
func splitPermCodes(s string) []string {
	out := make([]string, 0, 8)
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c != "" && rbac.Valid(c) {
			out = append(out, c)
		}
	}
	return out
}

// orgRolePerms 内置组织角色在该组织的生效权限集：有覆盖用覆盖，否则用代码模板
func orgRolePerms(db *gorm.DB, orgID uint, roleCode string) []string {
	if perms, ok := orgRoleOverride(db, orgID, roleCode); ok {
		return perms
	}
	if sr, ok := rbac.SystemRoleByCode(roleCode); ok {
		return sr.Perms
	}
	return nil
}

// orgTemplateHas 内置组织角色在该组织是否含某权限（含组织级覆盖）
func orgTemplateHas(db *gorm.DB, orgID uint, roleCode, perm string) bool {
	if roleCode == "" {
		return false
	}
	for _, p := range orgRolePerms(db, orgID, roleCode) {
		if p == perm {
			return true
		}
	}
	return false
}

// siteRoleNamesByUser userId → 持有的自定义站点角色名（管理端 / 社交 badge 用）。
// 内置角色不计入：站点管理员另有专属 badge。
func siteRoleNamesByUser(db *gorm.DB, userIDs []int64) map[int64][]string {
	out := make(map[int64][]string)
	if db == nil || len(userIDs) == 0 {
		return out
	}
	type row struct {
		UserID int64  `gorm:"column:user_id"`
		Name   string `gorm:"column:name"`
	}
	var rows []row
	err := db.Table("user_roles AS ur").
		Select("ur.user_id AS user_id, r.name AS name").
		Joins("JOIN roles r ON r.id = ur.role_id AND r.scope = ? AND r.is_system = false", rbac.ScopeSite).
		Where("ur.user_id IN ? AND ur.org_id = 0", userIDs).
		Order("ur.user_id ASC, r.id ASC").
		Scan(&rows).Error
	if err != nil {
		return out
	}
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		if r.UserID == 0 || name == "" {
			continue
		}
		out[r.UserID] = append(out[r.UserID], name)
	}
	return out
}

// —— 共享判定 / 同步助手（org.go / jwt_issue.go 复用）——

// hasPermInOrgDB 指定组织内是否具备权限（查库，不信 JWT；跨组织操作的兜底）。
// 顺序：站点管理员 → org_members 系统角色模板 → 自定义角色。
func hasPermInOrgDB(db *gorm.DB, userID, orgID uint, code string) bool {
	if db == nil || userID == 0 || orgID == 0 {
		return false
	}
	var u model.User
	if db.Select("is_site_admin", "role_id").First(&u, userID).Error == nil &&
		(u.IsSiteAdmin || u.RoleID == permission.RoleAdmin) {
		return true
	}
	var m model.OrgMember
	if db.Select("role").Where("org_id = ? AND user_id = ?", orgID, userID).First(&m).Error == nil &&
		orgTemplateHas(db, orgID, m.Role, code) {
		return true
	}
	var n int64
	db.Table("user_roles AS ur").
		Joins("JOIN roles r ON r.id = ur.role_id AND r.is_system = false").
		Joins("JOIN role_permissions rp ON rp.role_id = r.id").
		Where("ur.user_id = ? AND ur.org_id = ? AND rp.perm_code = ?", userID, orgID, code).
		Count(&n)
	return n > 0
}

// verifyOrgPerm JWT 快路径 + DB 兜底：当前请求对指定组织是否具备组织级权限
func verifyOrgPerm(ctx khttp.Context, db *gorm.DB, userID, orgID uint, code string) bool {
	if auth.HasOrgPerm(ctx, orgID, code) {
		return true
	}
	return hasPermInOrgDB(db, userID, orgID, code)
}

func roleIDByCode(db *gorm.DB, code string) uint {
	var r model.Role
	if db.Select("id").Where("code = ?", code).First(&r).Error != nil {
		return 0
	}
	return r.ID
}

// syncOrgMemberSystemRole 将 org_members.role 镜像进 user_roles（系统组织角色）。
// membership 不存在时清除该 (user, org) 的全部角色指派（含自定义）。
func syncOrgMemberSystemRole(db *gorm.DB, orgID, userID uint) {
	if db == nil || orgID == 0 || userID == 0 {
		return
	}
	roleCode := ""
	var m model.OrgMember
	if db.Select("role").Where("org_id = ? AND user_id = ?", orgID, userID).First(&m).Error == nil {
		roleCode = m.Role
	}
	if roleCode == "" {
		_ = db.Where("user_id = ? AND org_id = ?", userID, orgID).Delete(&model.UserRole{}).Error
		return
	}
	var sysRoles []model.Role
	_ = db.Where("is_system = ? AND scope = ?", true, rbac.ScopeOrg).Find(&sysRoles).Error
	var keepID uint
	ids := make([]uint, 0, len(sysRoles))
	for _, r := range sysRoles {
		ids = append(ids, r.ID)
		if r.Code == roleCode {
			keepID = r.ID
		}
	}
	if len(ids) > 0 {
		q := db.Where("user_id = ? AND org_id = ? AND role_id IN ?", userID, orgID, ids)
		if keepID > 0 {
			q = q.Where("role_id <> ?", keepID)
		}
		_ = q.Delete(&model.UserRole{}).Error
	}
	if keepID > 0 {
		var n int64
		_ = db.Model(&model.UserRole{}).Where("user_id = ? AND role_id = ? AND org_id = ?", userID, keepID, orgID).Count(&n).Error
		if n == 0 {
			_ = db.Create(&model.UserRole{UserID: userID, RoleID: keepID, OrgID: orgID}).Error
		}
	}
}

// syncSiteSystemRole 将 users 布尔位镜像进 user_roles（站点系统角色）
func syncSiteSystemRole(db *gorm.DB, userID uint, roleCode string, has bool) {
	if db == nil || userID == 0 {
		return
	}
	rid := roleIDByCode(db, roleCode)
	if rid == 0 {
		return
	}
	if has {
		var n int64
		_ = db.Model(&model.UserRole{}).Where("user_id = ? AND role_id = ? AND org_id = 0", userID, rid).Count(&n).Error
		if n == 0 {
			_ = db.Create(&model.UserRole{UserID: userID, RoleID: rid, OrgID: 0}).Error
		}
		return
	}
	_ = db.Where("user_id = ? AND role_id = ? AND org_id = 0", userID, rid).Delete(&model.UserRole{}).Error
}

// collectUserPerms 计算用户在指定组织上下文的有效权限（签发 JWT / my-permissions 用）。
// 站点管理员 → 全部；否则 org_members 模板（含本组织覆盖）∪ 自定义角色（站点级 + 该组织）。
func collectUserPerms(db *gorm.DB, u *model.User, orgID uint, orgRole string) []string {
	if u == nil {
		return nil
	}
	if u.IsSiteAdmin || u.RoleID == permission.RoleAdmin {
		return rbac.AllCodes()
	}
	set := make(map[string]bool)
	if orgRole != "" {
		// 内置组织角色按「本组织覆盖优先」取权限
		for _, c := range orgRolePerms(db, orgID, orgRole) {
			set[c] = true
		}
	}
	if db != nil {
		var codes []string
		_ = db.Table("user_roles AS ur").
			Joins("JOIN roles r ON r.id = ur.role_id AND r.is_system = false").
			Joins("JOIN role_permissions rp ON rp.role_id = r.id").
			Where("ur.user_id = ? AND ((ur.org_id = 0 AND r.scope = ?) OR ur.org_id = ?)", u.ID, rbac.ScopeSite, orgID).
			Pluck("rp.perm_code", &codes).Error
		for _, c := range codes {
			set[c] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		if rbac.Valid(c) {
			out = append(out, c)
		}
	}
	return out
}

// —— handlers ——

// handlePermissions 权限目录（登录即可；供权限勾选矩阵渲染）
func (s *RbacService) handlePermissions(ctx khttp.Context) error {
	if auth.GetCurrentUser(ctx) == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	groups := make([]map[string]interface{}, 0)
	for _, g := range rbac.Groups() {
		perms := make([]map[string]interface{}, 0, len(g.Perms))
		for _, p := range g.Perms {
			perms = append(perms, map[string]interface{}{
				"code": p.Code, "label": p.Label, "desc": p.Desc, "scope": p.Scope,
			})
		}
		groups = append(groups, map[string]interface{}{
			"key": g.Key, "label": g.Label, "scope": g.Scope, "perms": perms,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "groups": groups})
	return nil
}

func (s *RbacService) canViewOrgRoles(ctx khttp.Context, userID, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return verifyOrgPerm(ctx, s.db, userID, orgID, rbac.PermOrgRoleManage) ||
		verifyOrgPerm(ctx, s.db, userID, orgID, rbac.PermOrgMemberRole)
}

func (s *RbacService) canManageOrgRoles(ctx khttp.Context, userID, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return verifyOrgPerm(ctx, s.db, userID, orgID, rbac.PermOrgRoleManage)
}

func (s *RbacService) roleToMap(r *model.Role, orgID uint) map[string]interface{} {
	var perms []string
	_ = s.db.Model(&model.RolePermission{}).Where("role_id = ?", r.ID).Pluck("perm_code", &perms).Error
	if perms == nil {
		perms = []string{}
	}
	countOrg := uint(0)
	if r.Scope == rbac.ScopeOrg {
		countOrg = orgID
	}
	// 内置组织角色：权限展示按「本组织覆盖优先」，且教练/队长允许本组织改权限
	permsEditable := !r.IsSystem
	customized := false
	if r.IsSystem && r.Scope == rbac.ScopeOrg && orgID > 0 && rbac.OrgEditableSystemRole(r.Code) {
		permsEditable = true
		if ov, ok := orgRoleOverride(s.db, orgID, r.Code); ok {
			perms = ov
			customized = true
		} else if sr, ok := rbac.SystemRoleByCode(r.Code); ok {
			perms = append([]string{}, sr.Perms...)
		}
	} else if r.IsSystem {
		if sr, ok := rbac.SystemRoleByCode(r.Code); ok {
			perms = append([]string{}, sr.Perms...)
		}
	}
	var members int64
	_ = s.db.Model(&model.UserRole{}).Where("role_id = ? AND org_id = ?", r.ID, countOrg).Count(&members).Error
	return map[string]interface{}{
		"roleId":      r.ID,
		"code":        r.Code,
		"name":        r.Name,
		"description": r.Description,
		"scope":       r.Scope,
		"orgId":       r.OrgID,
		"isSystem":    r.IsSystem,
		// permsEditable：内置角色是否允许本组织改权限（名称/说明/成员仍锁定）
		"permsEditable": permsEditable,
		// customized：内置角色的权限已被本组织改过（可「恢复默认」）
		"customized":  customized,
		"permissions": perms,
		"memberCount": members,
	}
}

// handleRoles 角色列表：scope=site 站点级；scope=org（默认）为「全局模板 + 该组织自定义」
func (s *RbacService) handleRoles(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	q := ctx.Request().URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		scope = rbac.ScopeOrg
	}
	var roles []model.Role
	orgID := uint(0)
	switch scope {
	case rbac.ScopeSite:
		if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil
		}
		_ = s.db.Where("scope = ? AND org_id = 0", rbac.ScopeSite).Order("is_system DESC, id ASC").Find(&roles).Error
	case rbac.ScopeOrg:
		id64, _ := strconv.ParseUint(q.Get("orgId"), 10, 64)
		orgID = uint(id64)
		if orgID == 0 {
			orgID = pd.OrgID
		}
		if orgID == 0 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织 id"})
			return nil
		}
		if !s.canViewOrgRoles(ctx, pd.UserID, orgID) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil
		}
		_ = s.db.Where("scope = ? AND (org_id = ? OR (org_id = 0 AND is_system = ?))", rbac.ScopeOrg, orgID, true).
			Order("is_system DESC, id ASC").Find(&roles).Error
	default:
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "scope 无效（site|org）"})
		return nil
	}
	list := make([]map[string]interface{}, 0, len(roles))
	for i := range roles {
		list = append(list, s.roleToMap(&roles[i], orgID))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "list": list, "orgId": orgID, "scope": scope})
	return nil
}

// validateRolePerms 校验权限点合法且作用域与角色一致；返回去重后的集合
func validateRolePerms(scope string, perms []string) ([]string, string) {
	out := make([]string, 0, len(perms))
	seen := make(map[string]bool, len(perms))
	for _, c := range perms {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		p, ok := rbac.ByCode(c)
		if !ok {
			return nil, "存在未知权限点：" + c
		}
		if p.Scope != scope {
			return nil, "权限点与角色作用域不符：" + c
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, ""
}

func (s *RbacService) handleRoleCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		Scope       string   `json:"scope"`
		OrgID       uint     `json:"orgId"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if req.Scope == "" {
		req.Scope = rbac.ScopeOrg
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请填写角色名称"})
		return nil
	}
	if len([]rune(name)) > 32 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "角色名称过长（最多 32 字）"})
		return nil
	}
	orgID := uint(0)
	switch req.Scope {
	case rbac.ScopeSite:
		if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil
		}
	case rbac.ScopeOrg:
		orgID = req.OrgID
		if orgID == 0 {
			orgID = pd.OrgID
		}
		if orgID == 0 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织 id"})
			return nil
		}
		var o model.Org
		if s.db.Select("id").First(&o, orgID).Error != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
			return nil
		}
		if !s.canManageOrgRoles(ctx, pd.UserID, orgID) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil
		}
	default:
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "scope 无效（site|org）"})
		return nil
	}
	perms, errMsg := validateRolePerms(req.Scope, req.Permissions)
	if errMsg != "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": errMsg})
		return nil
	}
	role := model.Role{
		Code:        "c_" + strings.ToLower(newInviteCode()),
		Name:        name,
		Description: strings.TrimSpace(req.Description),
		Scope:       req.Scope,
		OrgID:       orgID,
		IsSystem:    false,
	}
	if err := s.db.Create(&role).Error; err != nil {
		log.Errorf("rbac create role: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "创建失败，请稍后重试"})
		return nil
	}
	for _, c := range perms {
		_ = s.db.Create(&model.RolePermission{RoleID: role.ID, PermCode: c}).Error
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已创建角色", "data": s.roleToMap(&role, orgID)})
	return nil
}

// loadEditableRole 加载角色并校验编辑权限；写错误响应时返回 nil
func (s *RbacService) loadEditableRole(ctx khttp.Context, pd *auth.JwtPayload, roleID uint) *model.Role {
	var role model.Role
	if s.db.First(&role, roleID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "角色不存在"})
		return nil
	}
	if role.IsSystem {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "内置角色不可改名或删除；教练 / 队长可在本组织调整权限，其余请新建自定义角色"})
		return nil
	}
	if role.Scope == rbac.ScopeSite {
		if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil
		}
	} else if !s.canManageOrgRoles(ctx, pd.UserID, role.OrgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	return &role
}

// updateSystemOrgRolePerms 处理内置组织角色的「本组织权限覆盖」。
// 返回 true 表示本次请求已由这里处理（含错误响应），调用方直接返回。
// 只有教练 / 队长可覆盖；团队管理员与成员是组织基本盘，权限锁定。
func (s *RbacService) updateSystemOrgRolePerms(
	ctx khttp.Context, pd *auth.JwtPayload, roleID, reqOrgID uint,
	perms *[]string, reset bool, metaChange bool,
) bool {
	var role model.Role
	if s.db.First(&role, roleID).Error != nil || !role.IsSystem {
		return false // 非内置角色 → 交给自定义角色流程
	}
	if role.Scope != rbac.ScopeOrg || !rbac.OrgEditableSystemRole(role.Code) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "「" + role.Name + "」是基本角色，权限固定，不能修改或删除"})
		return true
	}
	if metaChange {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "内置角色的名称与说明不能修改，只能调整本组织的权限"})
		return true
	}
	orgID := reqOrgID
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织 id"})
		return true
	}
	if !s.canManageOrgRoles(ctx, pd.UserID, orgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return true
	}
	if reset {
		_ = s.db.Where("org_id = ? AND role_code = ?", orgID, role.Code).Delete(&model.OrgRolePerm{}).Error
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "已恢复默认权限；成员刷新登录态后生效", "data": s.roleToMap(&role, orgID),
		})
		return true
	}
	if perms == nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return true
	}
	list, errMsg := validateRolePerms(rbac.ScopeOrg, *perms)
	if errMsg != "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": errMsg})
		return true
	}
	row := model.OrgRolePerm{OrgID: orgID, RoleCode: role.Code, PermCodes: strings.Join(list, ",")}
	if err := s.db.Where("org_id = ? AND role_code = ?", orgID, role.Code).
		Assign(map[string]interface{}{"perm_codes": row.PermCodes}).
		FirstOrCreate(&row).Error; err != nil {
		log.Errorf("rbac update system org role perms: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败，请稍后重试"})
		return true
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已保存；成员权限在刷新登录态后生效", "data": s.roleToMap(&role, orgID),
	})
	return true
}

func (s *RbacService) handleRoleUpdate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		RoleID      uint      `json:"roleId"`
		OrgID       uint      `json:"orgId"`
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		Permissions *[]string `json:"permissions"`
		// ResetPermissions 内置角色专用：清除本组织的权限覆盖，恢复默认
		ResetPermissions bool `json:"resetPermissions"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.RoleID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	// 内置组织角色（教练 / 队长）：只改本组织的权限覆盖，名称与成员仍锁定
	if done := s.updateSystemOrgRolePerms(ctx, pd, req.RoleID, req.OrgID, req.Permissions, req.ResetPermissions, req.Name != nil || req.Description != nil); done {
		return nil
	}
	role := s.loadEditableRole(ctx, pd, req.RoleID)
	if role == nil {
		return nil
	}
	updates := map[string]interface{}{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "角色名称不能为空"})
			return nil
		}
		if len([]rune(name)) > 32 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "角色名称过长（最多 32 字）"})
			return nil
		}
		updates["name"] = name
	}
	if req.Description != nil {
		updates["description"] = strings.TrimSpace(*req.Description)
	}
	if len(updates) > 0 {
		if err := s.db.Model(role).Updates(updates).Error; err != nil {
			log.Errorf("rbac update role: %v", err)
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败，请稍后重试"})
			return nil
		}
	}
	if req.Permissions != nil {
		perms, errMsg := validateRolePerms(role.Scope, *req.Permissions)
		if errMsg != "" {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": errMsg})
			return nil
		}
		syncRolePermsService(s.db, role.ID, perms)
	}
	orgID := role.OrgID
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已保存；成员权限在刷新登录态后生效", "data": s.roleToMap(role, orgID)})
	return nil
}

// syncRolePermsService 对齐角色权限集（增缺删多）
func syncRolePermsService(db *gorm.DB, roleID uint, want []string) {
	wantSet := make(map[string]bool, len(want))
	for _, c := range want {
		wantSet[c] = true
	}
	var have []model.RolePermission
	_ = db.Where("role_id = ?", roleID).Find(&have).Error
	haveSet := make(map[string]bool, len(have))
	for _, h := range have {
		haveSet[h.PermCode] = true
		if !wantSet[h.PermCode] {
			_ = db.Where("role_id = ? AND perm_code = ?", roleID, h.PermCode).Delete(&model.RolePermission{}).Error
		}
	}
	for c := range wantSet {
		if !haveSet[c] {
			_ = db.Create(&model.RolePermission{RoleID: roleID, PermCode: c}).Error
		}
	}
}

func (s *RbacService) handleRoleDelete(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		RoleID uint `json:"roleId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.RoleID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	role := s.loadEditableRole(ctx, pd, req.RoleID)
	if role == nil {
		return nil
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", role.ID).Delete(&model.UserRole{}).Error; err != nil {
			return err
		}
		if err := tx.Where("role_id = ?", role.ID).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		return tx.Delete(role).Error
	}); err != nil {
		log.Errorf("rbac delete role: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "删除失败，请稍后重试"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除角色"})
	return nil
}

// handleRoleMembers 角色成员（分页 + 模糊搜索）
func (s *RbacService) handleRoleMembers(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	q := ctx.Request().URL.Query()
	roleID64, _ := strconv.ParseUint(q.Get("roleId"), 10, 64)
	if roleID64 == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少 roleId"})
		return nil
	}
	var role model.Role
	if s.db.First(&role, uint(roleID64)).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "角色不存在"})
		return nil
	}
	orgID := uint(0)
	if role.Scope == rbac.ScopeOrg {
		if role.OrgID > 0 {
			orgID = role.OrgID
		} else {
			id64, _ := strconv.ParseUint(q.Get("orgId"), 10, 64)
			orgID = uint(id64)
			if orgID == 0 {
				orgID = pd.OrgID
			}
		}
		if orgID == 0 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织 id"})
			return nil
		}
		if !s.canViewOrgRoles(ctx, pd.UserID, orgID) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil
		}
	} else if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	keyword := strings.TrimSpace(q.Get("keyword"))

	base := s.db.Table("user_roles AS ur").
		Joins("JOIN users u ON u.id = ur.user_id").
		Where("ur.role_id = ? AND ur.org_id = ?", role.ID, orgID)
	if keyword != "" {
		like := "%" + keyword + "%"
		base = base.Where("u.username ILIKE ? OR u.name ILIKE ?", like, like)
	}
	var total int64
	_ = base.Session(&gorm.Session{}).Count(&total).Error

	type row struct {
		UserID    uint
		Username  string
		Name      string
		Avatar    string
		CreatedAt string
	}
	var rows []row
	_ = base.Session(&gorm.Session{}).
		Select("ur.user_id AS user_id, u.username AS username, u.name AS name, u.avatar AS avatar, ur.created_at AS created_at").
		Order("ur.id ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&rows).Error
	list := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		list = append(list, map[string]interface{}{
			"userId": r.UserID, "username": r.Username, "name": r.Name, "avatar": r.Avatar,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success", "list": list, "total": total, "page": page, "pageSize": pageSize,
		"roleId": role.ID, "orgId": orgID,
	})
	return nil
}

// loadAssignableRole 指派/移除的目标角色：仅自定义角色；内置角色走既有任命入口
func (s *RbacService) loadAssignableRole(ctx khttp.Context, pd *auth.JwtPayload, roleID uint) (*model.Role, uint) {
	var role model.Role
	if s.db.First(&role, roleID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "角色不存在"})
		return nil, 0
	}
	if role.IsSystem {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "内置角色请在成员管理或全站用户中任命"})
		return nil, 0
	}
	orgID := uint(0)
	if role.Scope == rbac.ScopeOrg {
		orgID = role.OrgID
		if !s.canManageOrgRoles(ctx, pd.UserID, orgID) {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
			return nil, 0
		}
	} else if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil, 0
	}
	return &role, orgID
}

func (s *RbacService) handleRoleAssign(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		RoleID  uint   `json:"roleId"`
		UserIDs []uint `json:"userIds"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.RoleID == 0 || len(req.UserIDs) == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	role, orgID := s.loadAssignableRole(ctx, pd, req.RoleID)
	if role == nil {
		return nil
	}
	added := 0
	skipped := make([]uint, 0)
	for _, uid := range req.UserIDs {
		if uid == 0 {
			continue
		}
		var u model.User
		if s.db.Select("id").First(&u, uid).Error != nil {
			skipped = append(skipped, uid)
			continue
		}
		if role.Scope == rbac.ScopeOrg {
			var n int64
			s.db.Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", orgID, uid).Count(&n)
			if n == 0 {
				skipped = append(skipped, uid)
				continue
			}
		}
		var exists int64
		_ = s.db.Model(&model.UserRole{}).Where("user_id = ? AND role_id = ? AND org_id = ?", uid, role.ID, orgID).Count(&exists).Error
		if exists == 0 {
			if err := s.db.Create(&model.UserRole{UserID: uid, RoleID: role.ID, OrgID: orgID}).Error; err != nil {
				skipped = append(skipped, uid)
				continue
			}
		}
		added++
	}
	msg := "已加入角色；对方刷新登录态后权限生效"
	if len(skipped) > 0 {
		msg = "部分用户未能加入（不存在或不在该组织中）"
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": msg, "added": added, "skipped": skipped,
	})
	return nil
}

func (s *RbacService) handleRoleUnassign(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		RoleID  uint   `json:"roleId"`
		UserIDs []uint `json:"userIds"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.RoleID == 0 || len(req.UserIDs) == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	role, orgID := s.loadAssignableRole(ctx, pd, req.RoleID)
	if role == nil {
		return nil
	}
	if err := s.db.Where("role_id = ? AND org_id = ? AND user_id IN ?", role.ID, orgID, req.UserIDs).
		Delete(&model.UserRole{}).Error; err != nil {
		log.Errorf("rbac unassign: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "移除失败，请稍后重试"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已移出角色；对方刷新登录态后权限失效"})
	return nil
}

// handleMyPermissions 当前用户在当前组织的有效权限（查库实时值，供前端兜底/调试）
func (s *RbacService) handleMyPermissions(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var u model.User
	if s.db.First(&u, pd.UserID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不存在"})
		return nil
	}
	orgID := pd.OrgID
	orgRole := ""
	var m model.OrgMember
	if orgID > 0 && s.db.Select("role").Where("org_id = ? AND user_id = ?", orgID, pd.UserID).First(&m).Error == nil {
		orgRole = m.Role
	}
	perms := collectUserPerms(s.db, &u, orgID, orgRole)
	if perms == nil {
		perms = []string{}
	}
	type roleRow struct {
		Name  string
		Scope string
		Code  string
	}
	var roleRows []roleRow
	_ = s.db.Table("user_roles AS ur").
		Joins("JOIN roles r ON r.id = ur.role_id").
		Where("ur.user_id = ? AND (ur.org_id = 0 OR ur.org_id = ?)", pd.UserID, orgID).
		Select("r.name AS name, r.scope AS scope, r.code AS code").
		Scan(&roleRows).Error
	roles := make([]map[string]interface{}, 0, len(roleRows))
	for _, r := range roleRows {
		roles = append(roles, map[string]interface{}{"name": r.Name, "scope": r.Scope, "code": r.Code})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"perms": perms, "roles": roles,
		"isSiteAdmin": u.IsSiteAdmin, "orgId": orgID, "orgRole": orgRole,
	})
	return nil
}
