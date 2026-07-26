package rbac

// 系统内置角色。权限集在代码中锁定（不可经 API 编辑）；差异化需求用自定义角色叠加。
// code 与存量数据对齐：org 模板 code == org_members.role 取值；site 角色映射 users 布尔位。
const (
	RoleSiteAdmin        = "site_admin"
	RoleResourceReviewer = "resource_reviewer"
	RoleOrgAdmin         = "org_admin"
	RoleCoach            = "coach"
	RoleCaptain          = "captain"
	RoleMember           = "member"
)

// SystemRole 内置角色模板
type SystemRole struct {
	Code  string
	Name  string
	Desc  string
	Scope string
	Perms []string
}

// orgStaffPerms 教练/队长共有权限（现状「队长暂同教练」，差异化走自定义角色）
var orgStaffPerms = []string{
	PermOrgGroupManage,
	PermOrgBulletinManage,
	PermOrgReportView,
	PermOrgMemberEmail,
}

var reviewerPerms = []string{
	PermContentProblemReview,
	PermContentBlogModerate,
	PermContentCommunityMod,
	PermContentReportHandle,
}

var systemRoles = []SystemRole{
	{RoleSiteAdmin, "站点管理员", "站点最高权限，旁路全部权限校验", ScopeSite, AllCodes()},
	{RoleResourceReviewer, "资源审核员", "题库审查、博客审核、社区内容治理与举报处理", ScopeSite, reviewerPerms},
	{RoleOrgAdmin, "团队管理员", "本组织全部管理权限", ScopeOrg, CodesByScope(ScopeOrg)},
	{RoleCoach, "教练", "本组织日常管理：分组、公告、训练报告", ScopeOrg, orgStaffPerms},
	{RoleCaptain, "队长", "本组织日常管理：分组、公告、训练报告", ScopeOrg, orgStaffPerms},
	{RoleMember, "成员", "普通成员，无管理权限", ScopeOrg, nil},
}

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

// TemplateHas 内置角色模板是否含权限（用于 org_members.role / 旧 token 推导）
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

// LegacyHas 旧 token（无 pm claim）按旧 claims 推导权限：
// 资源审核员 → 内容审核模板（reviewerPerms）；orgRole → 对应模板。站点管理员在调用方旁路，不经此处。
func LegacyHas(perm string, isResourceReviewer bool, orgRole string) bool {
	if isResourceReviewer {
		for _, p := range reviewerPerms {
			if p == perm {
				return true
			}
		}
	}
	if orgRole != "" {
		return TemplateHas(orgRole, perm)
	}
	return false
}
