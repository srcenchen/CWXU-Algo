package rbac

// 系统内置角色。权限集在代码中锁定；组织可对「教练 / 组长 / 队长」按组织覆盖权限（见 OrgEditableSystemRole）。
// code 与存量数据对齐：org 模板 code == org_members.role 取值；site 角色映射 users 布尔位。
// 组织内层级：org_admin > coach > group_leader > captain > member
const (
	RoleSiteAdmin   = "site_admin"
	RoleOrgAdmin    = "org_admin"
	RoleCoach       = "coach"
	RoleGroupLeader = "group_leader"
	RoleCaptain     = "captain"
	RoleMember      = "member"

	// RoleResourceReviewer 已下线的「资源审核员」内置身份；仅存量清理仍引用该 code。
	// Deprecated: 内容审核权限改由站点自定义角色授予。
	RoleResourceReviewer = "resource_reviewer"
)

// GroupContent 内容审核权限分组 key（题库审查 / 博客审核 / 社区治理 / 举报处理）
const GroupContent = "content"

// SystemRole 内置角色模板
type SystemRole struct {
	Code  string
	Name  string
	Desc  string
	Scope string
	Perms []string
}

// coachPerms 教练：全组织数据 + 分组/公告/报告 + 可任命组长/队长（任命时再按等级裁剪）
var coachPerms = []string{
	PermOrgGroupManage,
	PermOrgBulletinManage,
	PermOrgReportView,
	PermOrgMemberEmail,
	PermOrgMemberRole,
	PermOrgMemberDisplayName,
}

// groupLeaderPerms 组长：组内数据 + 报告；分队写/有限任命由 service 按 scope 判定
var groupLeaderPerms = []string{
	PermOrgReportView,
	PermOrgMemberRole, // 仅能任命本组队长/成员（handleSetRole 等级+范围校验）
	PermOrgMemberDisplayName,
}

// captainPerms 队长：仅本分队数据与报告；分队成员调整由 service 按 scope 判定
var captainPerms = []string{
	PermOrgReportView,
}

var systemRoles = []SystemRole{
	{RoleSiteAdmin, "站点管理员", "站点最高权限，旁路全部权限校验", ScopeSite, AllCodes()},
	{RoleOrgAdmin, "组织管理员", "本组织全部管理权限", ScopeOrg, CodesByScope(ScopeOrg)},
	{RoleCoach, "教练", "全组织数据与日常管理；可任命组长/队长", ScopeOrg, coachPerms},
	{RoleGroupLeader, "组长", "管理指定分组及组内分队；可任命本组队长", ScopeOrg, groupLeaderPerms},
	{RoleCaptain, "队长", "管理指定分队；查看本分队训练数据", ScopeOrg, captainPerms},
	{RoleMember, "成员", "普通成员，无管理权限", ScopeOrg, nil},
}

// orgEditableSystemRoles 组织可在本组织内改权限的内置角色。
// 组织管理员 / 成员是组织的基本盘（最高与最低档），权限固定，不可改也不可删。
var orgEditableSystemRoles = map[string]bool{
	RoleCoach:       true,
	RoleGroupLeader: true,
	RoleCaptain:     true,
}

// OrgEditableSystemRole 该内置组织角色是否允许组织覆盖权限
func OrgEditableSystemRole(code string) bool { return orgEditableSystemRoles[code] }

// ContentPerms 内容审核权限点（审核/举报通知受众判定用）
func ContentPerms() []string { return CodesByGroup(GroupContent) }

var systemRoleByCode = func() map[string]SystemRole {
	m := make(map[string]SystemRole, len(systemRoles))
	for _, r := range systemRoles {
		m[r.Code] = r
	}
	return m
}()

// SystemRoles 全部内置角色模板
func SystemRoles() []SystemRole { return systemRoles }

// SystemRoleByCode 查内置角色
func SystemRoleByCode(code string) (SystemRole, bool) {
	r, ok := systemRoleByCode[code]
	return r, ok
}

// IsSystemRoleCode 是否内置角色 code
func IsSystemRoleCode(code string) bool {
	_, ok := systemRoleByCode[code]
	return ok
}

// TemplateHas 内置角色模板是否含权限。
// ⚠️ 只看代码模板，不含组织级覆盖；组织内判定请走 service 层的 orgTemplateHas。
func TemplateHas(roleCode, perm string) bool {
	r, ok := systemRoleByCode[roleCode]
	if !ok {
		return false
	}
	for _, p := range r.Perms {
		if p == perm {
			return true
		}
	}
	return false
}

// LegacyHas 旧 token（无 pm claim）按 orgRole 推导权限：走代码模板，不含组织级覆盖。
// 站点管理员在调用方旁路，不经此处；旧 token 过期后此路径自然消失。
func LegacyHas(perm string, orgRole string) bool {
	if orgRole != "" {
		return TemplateHas(orgRole, perm)
	}
	return false
}
