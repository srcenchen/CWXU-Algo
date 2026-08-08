package service

import (
	"context"
	"strings"

	pb "cwxu-algo/api/user/v1/rbac"
	"cwxu-algo/app/common/permission"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/sqllike"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// RbacService 角色与权限管理（细粒度 RBAC）。
// 内置角色权限集代码锁定；本服务只管理自定义角色与其成员指派。
// 内置角色的任命仍走既有入口（org members/set-role、platform/set-*），并双写 user_roles 镜像。
// 实现 proto：api/user/v1/rbac/rbac.proto（RbacHTTPServer）。
type RbacService struct {
	db  *gorm.DB
	rdb *redis.Client
}

func NewRbacService(d *data.Data) *RbacService {
	return &RbacService{db: d.DB, rdb: d.RDB}
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
func verifyOrgPerm(ctx context.Context, db *gorm.DB, userID, orgID uint, code string) bool {
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

// —— proto handlers ——

// Permissions 权限目录（登录即可；供权限勾选矩阵渲染）
func (s *RbacService) Permissions(ctx context.Context, req *pb.PermissionsReq) (*pb.PermissionsRes, error) {
	if auth.GetCurrentUser(ctx) == nil {
		return &pb.PermissionsRes{Success: false, Message: "请先登录"}, nil
	}
	groups := make([]*pb.PermGroup, 0)
	for _, g := range rbac.Groups() {
		perms := make([]*pb.PermInfo, 0, len(g.Perms))
		for _, p := range g.Perms {
			perms = append(perms, &pb.PermInfo{
				Code: p.Code, Label: p.Label, Desc: p.Desc, Scope: p.Scope,
			})
		}
		groups = append(groups, &pb.PermGroup{
			Key: g.Key, Label: g.Label, Scope: g.Scope, Perms: perms,
		})
	}
	return &pb.PermissionsRes{Success: true, Message: "success", Groups: groups}, nil
}

func (s *RbacService) canViewOrgRoles(ctx context.Context, userID, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return verifyOrgPerm(ctx, s.db, userID, orgID, rbac.PermOrgRoleManage) ||
		verifyOrgPerm(ctx, s.db, userID, orgID, rbac.PermOrgMemberRole)
}

func (s *RbacService) canManageOrgRoles(ctx context.Context, userID, orgID uint) bool {
	if auth.VerifySiteAdmin(ctx) {
		return true
	}
	return verifyOrgPerm(ctx, s.db, userID, orgID, rbac.PermOrgRoleManage)
}

func (s *RbacService) roleToMap(r *model.Role, orgID uint) *pb.RoleInfo {
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
	return &pb.RoleInfo{
		RoleId:      int64(r.ID),
		Code:        r.Code,
		Name:        r.Name,
		Description: r.Description,
		Scope:       r.Scope,
		OrgId:       int64(r.OrgID),
		IsSystem:    r.IsSystem,
		// permsEditable：内置角色是否允许本组织改权限（名称/说明/成员仍锁定）
		PermsEditable: permsEditable,
		// customized：内置角色的权限已被本组织改过（可「恢复默认」）
		Customized:  customized,
		Permissions: perms,
		MemberCount: int32(members),
	}
}

// Roles 角色列表：scope=site 站点级；scope=org（默认）为「全局模板 + 该组织自定义」
func (s *RbacService) Roles(ctx context.Context, req *pb.RolesReq) (*pb.RolesRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RolesRes{Success: false, Message: "请先登录"}, nil
	}
	scope := req.Scope
	if scope == "" {
		scope = rbac.ScopeOrg
	}
	var roles []model.Role
	orgID := uint(0)
	switch scope {
	case rbac.ScopeSite:
		if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
			return &pb.RolesRes{Success: false, Message: "权限不足"}, nil
		}
		_ = s.db.WithContext(ctx).Where("scope = ? AND org_id = 0", rbac.ScopeSite).Order("is_system DESC, id ASC").Find(&roles).Error
	case rbac.ScopeOrg:
		orgID = uint(req.OrgId)
		if orgID == 0 {
			orgID = pd.OrgID
		}
		if orgID == 0 {
			return &pb.RolesRes{Success: false, Message: "缺少组织 id"}, nil
		}
		if !s.canViewOrgRoles(ctx, pd.UserID, orgID) {
			return &pb.RolesRes{Success: false, Message: "权限不足"}, nil
		}
		_ = s.db.WithContext(ctx).Where("scope = ? AND (org_id = ? OR (org_id = 0 AND is_system = ?))", rbac.ScopeOrg, orgID, true).
			Order("is_system DESC, id ASC").Find(&roles).Error
	default:
		return &pb.RolesRes{Success: false, Message: "scope 无效（site|org）"}, nil
	}
	list := make([]*pb.RoleInfo, 0, len(roles))
	for i := range roles {
		list = append(list, s.roleToMap(&roles[i], orgID))
	}
	return &pb.RolesRes{Success: true, Message: "success", List: list, OrgId: int64(orgID), Scope: scope}, nil
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

func (s *RbacService) RoleCreate(ctx context.Context, req *pb.RoleCreateReq) (*pb.RoleCreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RoleCreateRes{Success: false, Message: "请先登录"}, nil
	}
	scope := req.Scope
	if scope == "" {
		scope = rbac.ScopeOrg
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return &pb.RoleCreateRes{Success: false, Message: "请填写角色名称"}, nil
	}
	if len([]rune(name)) > 32 {
		return &pb.RoleCreateRes{Success: false, Message: "角色名称过长（最多 32 字）"}, nil
	}
	orgID := uint(0)
	switch scope {
	case rbac.ScopeSite:
		if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
			return &pb.RoleCreateRes{Success: false, Message: "权限不足"}, nil
		}
	case rbac.ScopeOrg:
		orgID = uint(req.OrgId)
		if orgID == 0 {
			orgID = pd.OrgID
		}
		if orgID == 0 {
			return &pb.RoleCreateRes{Success: false, Message: "缺少组织 id"}, nil
		}
		var o model.Org
		if s.db.WithContext(ctx).Select("id").First(&o, orgID).Error != nil {
			return &pb.RoleCreateRes{Success: false, Message: "组织不存在"}, nil
		}
		if !s.canManageOrgRoles(ctx, pd.UserID, orgID) {
			return &pb.RoleCreateRes{Success: false, Message: "权限不足"}, nil
		}
	default:
		return &pb.RoleCreateRes{Success: false, Message: "scope 无效（site|org）"}, nil
	}
	perms, errMsg := validateRolePerms(scope, req.Permissions)
	if errMsg != "" {
		return &pb.RoleCreateRes{Success: false, Message: errMsg}, nil
	}
	role := model.Role{
		Code:        "c_" + strings.ToLower(newInviteCode()),
		Name:        name,
		Description: strings.TrimSpace(req.Description),
		Scope:       scope,
		OrgID:       orgID,
		IsSystem:    false,
	}
	if err := s.db.WithContext(ctx).Create(&role).Error; err != nil {
		log.Errorf("rbac create role: %v", err)
		return &pb.RoleCreateRes{Success: false, Message: "创建失败，请稍后重试"}, nil
	}
	for _, c := range perms {
		_ = s.db.WithContext(ctx).Create(&model.RolePermission{RoleID: role.ID, PermCode: c}).Error
	}
	return &pb.RoleCreateRes{Success: true, Message: "已创建角色", Data: s.roleToMap(&role, orgID)}, nil
}

// loadEditableRole 加载角色并校验编辑权限；返回 (角色, 错误消息)，错误消息非空时角色为 nil。
func (s *RbacService) loadEditableRole(ctx context.Context, pd *auth.JwtPayload, roleID uint) (*model.Role, string) {
	var role model.Role
	if s.db.WithContext(ctx).First(&role, roleID).Error != nil {
		return nil, "角色不存在"
	}
	if role.IsSystem {
		return nil, "内置角色不可改名或删除；教练 / 队长可在本组织调整权限，其余请新建自定义角色"
	}
	if role.Scope == rbac.ScopeSite {
		if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
			return nil, "权限不足"
		}
	} else if !s.canManageOrgRoles(ctx, pd.UserID, role.OrgID) {
		return nil, "权限不足"
	}
	return &role, ""
}

// updateSystemOrgRolePerms 处理内置组织角色的「本组织权限覆盖」。
// 返回 (响应, 是否已处理)；已处理（含错误响应）时调用方直接返回该响应。
// 只有教练 / 队长可覆盖；团队管理员与成员是组织基本盘，权限锁定。
func (s *RbacService) updateSystemOrgRolePerms(
	ctx context.Context, pd *auth.JwtPayload, roleID, reqOrgID uint,
	perms []string, reset bool, metaChange bool,
) (*pb.RoleUpdateRes, bool) {
	var role model.Role
	if s.db.WithContext(ctx).First(&role, roleID).Error != nil || !role.IsSystem {
		return nil, false // 非内置角色 → 交给自定义角色流程
	}
	if role.Scope != rbac.ScopeOrg || !rbac.OrgEditableSystemRole(role.Code) {
		return &pb.RoleUpdateRes{Success: false, Message: "「" + role.Name + "」是基本角色，权限固定，不能修改或删除"}, true
	}
	if metaChange {
		return &pb.RoleUpdateRes{Success: false, Message: "内置角色的名称与说明不能修改，只能调整本组织的权限"}, true
	}
	orgID := reqOrgID
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		return &pb.RoleUpdateRes{Success: false, Message: "缺少组织 id"}, true
	}
	if !s.canManageOrgRoles(ctx, pd.UserID, orgID) {
		return &pb.RoleUpdateRes{Success: false, Message: "权限不足"}, true
	}
	if reset {
		_ = s.db.WithContext(ctx).Where("org_id = ? AND role_code = ?", orgID, role.Code).Delete(&model.OrgRolePerm{}).Error
		return &pb.RoleUpdateRes{
			Success: true, Message: "已恢复默认权限；成员刷新登录态后生效", Data: s.roleToMap(&role, orgID),
		}, true
	}
	if perms == nil {
		return &pb.RoleUpdateRes{Success: false, Message: "参数错误"}, true
	}
	list, errMsg := validateRolePerms(rbac.ScopeOrg, perms)
	if errMsg != "" {
		return &pb.RoleUpdateRes{Success: false, Message: errMsg}, true
	}
	row := model.OrgRolePerm{OrgID: orgID, RoleCode: role.Code, PermCodes: strings.Join(list, ",")}
	if err := s.db.WithContext(ctx).Where("org_id = ? AND role_code = ?", orgID, role.Code).
		Assign(map[string]interface{}{"perm_codes": row.PermCodes}).
		FirstOrCreate(&row).Error; err != nil {
		log.Errorf("rbac update system org role perms: %v", err)
		return &pb.RoleUpdateRes{Success: false, Message: "保存失败，请稍后重试"}, true
	}
	return &pb.RoleUpdateRes{
		Success: true, Message: "已保存；成员权限在刷新登录态后生效", Data: s.roleToMap(&role, orgID),
	}, true
}

func (s *RbacService) RoleUpdate(ctx context.Context, req *pb.RoleUpdateReq) (*pb.RoleUpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RoleUpdateRes{Success: false, Message: "请先登录"}, nil
	}
	roleID := uint(req.RoleId)
	if roleID == 0 {
		return &pb.RoleUpdateRes{Success: false, Message: "参数错误"}, nil
	}
	// 内置组织角色（教练 / 队长）：只改本组织的权限覆盖，名称与成员仍锁定
	if res, done := s.updateSystemOrgRolePerms(ctx, pd, roleID, uint(req.OrgId), req.Permissions, req.ResetPermissions, req.Name != nil || req.Description != nil); done {
		return res, nil
	}
	role, errMsg := s.loadEditableRole(ctx, pd, roleID)
	if role == nil {
		return &pb.RoleUpdateRes{Success: false, Message: errMsg}, nil
	}
	updates := map[string]interface{}{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return &pb.RoleUpdateRes{Success: false, Message: "角色名称不能为空"}, nil
		}
		if len([]rune(name)) > 32 {
			return &pb.RoleUpdateRes{Success: false, Message: "角色名称过长（最多 32 字）"}, nil
		}
		updates["name"] = name
	}
	if req.Description != nil {
		updates["description"] = strings.TrimSpace(*req.Description)
	}
	if len(updates) > 0 {
		if err := s.db.WithContext(ctx).Model(role).Updates(updates).Error; err != nil {
			log.Errorf("rbac update role: %v", err)
			return &pb.RoleUpdateRes{Success: false, Message: "保存失败，请稍后重试"}, nil
		}
	}
	if req.Permissions != nil {
		perms, errMsg := validateRolePerms(role.Scope, req.Permissions)
		if errMsg != "" {
			return &pb.RoleUpdateRes{Success: false, Message: errMsg}, nil
		}
		syncRolePermsService(s.db, role.ID, perms)
	}
	orgID := role.OrgID
	return &pb.RoleUpdateRes{Success: true, Message: "已保存；成员权限在刷新登录态后生效", Data: s.roleToMap(role, orgID)}, nil
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

func (s *RbacService) RoleDelete(ctx context.Context, req *pb.RoleDeleteReq) (*pb.RoleDeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RoleDeleteRes{Success: false, Message: "请先登录"}, nil
	}
	roleID := uint(req.RoleId)
	if roleID == 0 {
		return &pb.RoleDeleteRes{Success: false, Message: "参数错误"}, nil
	}
	role, errMsg := s.loadEditableRole(ctx, pd, roleID)
	if role == nil {
		return &pb.RoleDeleteRes{Success: false, Message: errMsg}, nil
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", role.ID).Delete(&model.UserRole{}).Error; err != nil {
			return err
		}
		if err := tx.Where("role_id = ?", role.ID).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		return tx.Delete(role).Error
	}); err != nil {
		log.Errorf("rbac delete role: %v", err)
		return &pb.RoleDeleteRes{Success: false, Message: "删除失败，请稍后重试"}, nil
	}
	return &pb.RoleDeleteRes{Success: true, Message: "已删除角色"}, nil
}

// RoleMembers 角色成员（分页 + 模糊搜索）
func (s *RbacService) RoleMembers(ctx context.Context, req *pb.RoleMembersReq) (*pb.RoleMembersRes, error) {
	avatarBase := avatarPublicBase(s.db)
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RoleMembersRes{Success: false, Message: "请先登录"}, nil
	}
	roleID := uint(req.RoleId)
	if roleID == 0 {
		return &pb.RoleMembersRes{Success: false, Message: "缺少 roleId"}, nil
	}
	var role model.Role
	if s.db.WithContext(ctx).First(&role, roleID).Error != nil {
		return &pb.RoleMembersRes{Success: false, Message: "角色不存在"}, nil
	}
	orgID := uint(0)
	if role.Scope == rbac.ScopeOrg {
		if role.OrgID > 0 {
			orgID = role.OrgID
		} else {
			orgID = uint(req.OrgId)
			if orgID == 0 {
				orgID = pd.OrgID
			}
		}
		if orgID == 0 {
			return &pb.RoleMembersRes{Success: false, Message: "缺少组织 id"}, nil
		}
		if !s.canViewOrgRoles(ctx, pd.UserID, orgID) {
			return &pb.RoleMembersRes{Success: false, Message: "权限不足"}, nil
		}
	} else if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
		return &pb.RoleMembersRes{Success: false, Message: "权限不足"}, nil
	}
	page := int(req.Page)
	if page < 1 {
		page = 1
	}
	pageSize := int(req.PageSize)
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	keyword := strings.TrimSpace(req.Keyword)

	base := s.db.WithContext(ctx).Table("user_roles AS ur").
		Joins("JOIN users u ON u.id = ur.user_id").
		Where("ur.role_id = ? AND ur.org_id = ?", role.ID, orgID)
	if keyword != "" {
		if like := sqllike.Pattern(keyword); like != "" {
			base = base.Where("u.username ILIKE ? OR u.name ILIKE ?", like, like)
		}
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
	list := make([]*pb.RoleMemberInfo, 0, len(rows))
	for _, r := range rows {
		list = append(list, &pb.RoleMemberInfo{
			UserId: int64(r.UserID), Username: r.Username, Name: r.Name, Avatar: expandAvatarBase(avatarBase, r.Avatar),
		})
	}
	return &pb.RoleMembersRes{
		Success: true, Message: "success", List: list, Total: int32(total),
		Page: int32(page), PageSize: int32(pageSize),
		RoleId: int64(role.ID), OrgId: int64(orgID),
	}, nil
}

// loadAssignableRole 指派/移除的目标角色：仅自定义角色；内置角色走既有任命入口。
// 返回 (角色, orgID, 错误消息)，错误消息非空时角色为 nil。
func (s *RbacService) loadAssignableRole(ctx context.Context, pd *auth.JwtPayload, roleID uint) (*model.Role, uint, string) {
	var role model.Role
	if s.db.WithContext(ctx).First(&role, roleID).Error != nil {
		return nil, 0, "角色不存在"
	}
	if role.IsSystem {
		return nil, 0, "内置角色请在成员管理或全站用户中任命"
	}
	orgID := uint(0)
	if role.Scope == rbac.ScopeOrg {
		orgID = role.OrgID
		if !s.canManageOrgRoles(ctx, pd.UserID, orgID) {
			return nil, 0, "权限不足"
		}
	} else if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) {
		return nil, 0, "权限不足"
	}
	return &role, orgID, ""
}

func (s *RbacService) RoleAssign(ctx context.Context, req *pb.RoleAssignReq) (*pb.RoleAssignRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RoleAssignRes{Success: false, Message: "请先登录"}, nil
	}
	roleID := uint(req.RoleId)
	if roleID == 0 || len(req.UserIds) == 0 {
		return &pb.RoleAssignRes{Success: false, Message: "参数错误"}, nil
	}
	role, orgID, errMsg := s.loadAssignableRole(ctx, pd, roleID)
	if role == nil {
		return &pb.RoleAssignRes{Success: false, Message: errMsg}, nil
	}
	added := 0
	skipped := make([]int32, 0)
	for _, uid64 := range req.UserIds {
		uid := uint(uid64)
		if uid == 0 {
			continue
		}
		var u model.User
		if s.db.WithContext(ctx).Select("id").First(&u, uid).Error != nil {
			skipped = append(skipped, int32(uid))
			continue
		}
		if role.Scope == rbac.ScopeOrg {
			var n int64
			s.db.WithContext(ctx).Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", orgID, uid).Count(&n)
			if n == 0 {
				skipped = append(skipped, int32(uid))
				continue
			}
		}
		var exists int64
		_ = s.db.WithContext(ctx).Model(&model.UserRole{}).Where("user_id = ? AND role_id = ? AND org_id = ?", uid, role.ID, orgID).Count(&exists).Error
		if exists == 0 {
			if err := s.db.WithContext(ctx).Create(&model.UserRole{UserID: uid, RoleID: role.ID, OrgID: orgID}).Error; err != nil {
				skipped = append(skipped, int32(uid))
				continue
			}
		}
		added++
	}
	msg := "已加入角色；对方刷新登录态后权限生效"
	if len(skipped) > 0 {
		msg = "部分用户未能加入（不存在或不在该组织中）"
	}
	return &pb.RoleAssignRes{Success: true, Message: msg, Added: int32(added), Skipped: skipped}, nil
}

func (s *RbacService) RoleUnassign(ctx context.Context, req *pb.RoleUnassignReq) (*pb.RoleUnassignRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.RoleUnassignRes{Success: false, Message: "请先登录"}, nil
	}
	roleID := uint(req.RoleId)
	if roleID == 0 || len(req.UserIds) == 0 {
		return &pb.RoleUnassignRes{Success: false, Message: "参数错误"}, nil
	}
	role, orgID, errMsg := s.loadAssignableRole(ctx, pd, roleID)
	if role == nil {
		return &pb.RoleUnassignRes{Success: false, Message: errMsg}, nil
	}
	userIDs := make([]uint, 0, len(req.UserIds))
	for _, uid64 := range req.UserIds {
		userIDs = append(userIDs, uint(uid64))
	}
	if err := s.db.WithContext(ctx).Where("role_id = ? AND org_id = ? AND user_id IN ?", role.ID, orgID, userIDs).
		Delete(&model.UserRole{}).Error; err != nil {
		log.Errorf("rbac unassign: %v", err)
		return &pb.RoleUnassignRes{Success: false, Message: "移除失败，请稍后重试"}, nil
	}
	return &pb.RoleUnassignRes{Success: true, Message: "已移出角色；对方刷新登录态后权限失效"}, nil
}

// UserRoles 某用户持有的站点级自定义角色 id 列表（org_id=0），消 N+1。
// 权限：site.role.manage 或 site.user.list（与站点角色/用户管理一致）。
func (s *RbacService) UserRoles(ctx context.Context, req *pb.UserRolesReq) (*pb.UserRolesRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.UserRolesRes{Success: false, Message: "请先登录"}, nil
	}
	if !auth.HasPerm(ctx, rbac.PermSiteRoleManage) && !auth.HasPerm(ctx, rbac.PermSiteUserList) {
		return &pb.UserRolesRes{Success: false, Message: "权限不足"}, nil
	}
	userID := uint(req.UserId)
	if userID == 0 {
		return &pb.UserRolesRes{Success: false, Message: "缺少 userId"}, nil
	}
	var roleIDs []uint
	_ = s.db.WithContext(ctx).Model(&model.UserRole{}).
		Where("user_id = ? AND org_id = 0", userID).
		Order("role_id ASC").
		Pluck("role_id", &roleIDs).Error
	if roleIDs == nil {
		roleIDs = []uint{}
	}
	out := make([]int32, 0, len(roleIDs))
	for _, id := range roleIDs {
		out = append(out, int32(id))
	}
	return &pb.UserRolesRes{Success: true, RoleIds: out}, nil
}

// MyPermissions 当前用户在当前组织的有效权限（查库实时值，供前端兜底/调试）
func (s *RbacService) MyPermissions(ctx context.Context, req *pb.MyPermissionsReq) (*pb.MyPermissionsRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &pb.MyPermissionsRes{Success: false, Message: "请先登录"}, nil
	}
	var u model.User
	if s.db.WithContext(ctx).First(&u, pd.UserID).Error != nil {
		return &pb.MyPermissionsRes{Success: false, Message: "用户不存在"}, nil
	}
	orgID := pd.OrgID
	orgRole := ""
	var m model.OrgMember
	if orgID > 0 && s.db.WithContext(ctx).Select("role").Where("org_id = ? AND user_id = ?", orgID, pd.UserID).First(&m).Error == nil {
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
	_ = s.db.WithContext(ctx).Table("user_roles AS ur").
		Joins("JOIN roles r ON r.id = ur.role_id").
		Where("ur.user_id = ? AND (ur.org_id = 0 OR ur.org_id = ?)", pd.UserID, orgID).
		Select("r.name AS name, r.scope AS scope, r.code AS code").
		Scan(&roleRows).Error
	roles := make([]*pb.MyRoleBrief, 0, len(roleRows))
	for _, r := range roleRows {
		roles = append(roles, &pb.MyRoleBrief{Name: r.Name, Scope: r.Scope, Code: r.Code})
	}
	return &pb.MyPermissionsRes{
		Success: true, Message: "success",
		Perms: perms, Roles: roles,
		IsSiteAdmin: u.IsSiteAdmin, OrgId: int64(orgID), OrgRole: orgRole,
	}, nil
}
