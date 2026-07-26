// Package rbac 细粒度权限的唯一权威目录：权限点、位序、分组、系统角色模板。
// DB 只存 role→perm_code 关联；新增权限点只在本包注册（bit 追加，永不复用/改号）。
package rbac

// 权限作用域
const (
	ScopeSite = "site" // 站点级（全站生效）
	ScopeOrg  = "org"  // 组织级（对某组织生效；JWT 中为当前组织）
)

// 权限点 code 常量。命名：域.资源.动作
const (
	// —— 站点 · 配置运维 ——
	PermSiteConfigRead  = "site.config.read"
	PermSiteConfigWrite = "site.config.write"
	PermSiteStatsRead   = "site.stats.read"
	PermSiteBackup      = "site.backup.manage"
	PermSiteSpiderOps   = "site.spider.ops"
	PermSiteProblemOps  = "site.problem.ops"
	PermSiteBlogBoard   = "site.blog.dashboard"

	// —— 站点 · 组织治理 ——
	PermSiteOrgCreate = "site.org.create"
	PermSiteOrgDelete = "site.org.delete"
	PermSiteOrgPolicy = "site.org.policy"
	PermSiteOrgList   = "site.org.list"

	// —— 站点 · 用户运维 ——
	PermSiteUserList    = "site.user.list"
	PermSiteUserDisable = "site.user.disable"
	PermSiteUserDelete  = "site.user.delete"
	PermSiteUserSync    = "site.user.sync"

	// —— 站点 · 任命与角色 ——
	// ⚠️ 曾有 site.appoint.reviewer（bit 16）：资源审核员内置身份已下线，该 bit 永久退休，不得复用。
	PermSiteAppointAdmin = "site.appoint.admin"
	PermSiteRoleManage   = "site.role.manage"

	// —— 站点 · 公告 ——
	PermSiteBulletin  = "site.bulletin.manage"
	PermSiteEmergency = "site.emergency.manage"

	// —— 内容审核（站点级） ——
	PermContentProblemReview    = "content.problem.review"
	PermContentBlogModerate     = "content.blog.moderate"
	PermContentCommunityMod     = "content.community.moderate"
	PermContentReportHandle     = "content.report.handle"

	// —— 组织 · 设置 ——
	PermOrgInfoWrite    = "org.info.write"
	PermOrgPolicyToggle = "org.policy.toggle"
	PermOrgRoleManage   = "org.role.manage"

	// —— 组织 · 成员 ——
	PermOrgMemberAdd         = "org.member.add"
	PermOrgMemberRemove      = "org.member.remove"
	PermOrgMemberRole        = "org.member.role"
	PermOrgMemberDisplayName = "org.member.display-name"
	PermOrgInviteView        = "org.invite.view"
	PermOrgInviteRotate      = "org.invite.rotate"
	PermOrgJoinReview        = "org.join.review"

	// —— 组织 · 日常管理 ——
	PermOrgGroupManage    = "org.group.manage"
	PermOrgBulletinManage = "org.bulletin.manage"
	PermOrgReportView     = "org.report.view"
	PermOrgMemberEmail    = "org.member.email"
)

// Perm 权限点元数据
type Perm struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
	Group string `json:"group"` // 分组 key，见 Groups()
	Scope string `json:"scope"` // site | org
	Bit   int    `json:"-"`     // bitmask 位序：稳定、只追加、永不复用
}

// Group 权限分组（供前端勾选矩阵渲染）
type Group struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Scope string `json:"scope"`
	Perms []Perm `json:"perms"`
}

// registry 按 bit 升序注册。⚠️ 只允许在末尾追加；修改/删除既有 bit 会破坏已签发 JWT 与存量数据。
var registry = []Perm{
	{PermSiteConfigRead, "查看站点配置", "查看站点全局配置（含敏感字段脱敏视图）", "site_ops", ScopeSite, 0},
	{PermSiteConfigWrite, "修改站点配置", "修改站点全局配置、发送测试邮件", "site_ops", ScopeSite, 1},
	{PermSiteStatsRead, "站点访问统计", "查看全站访问统计数据", "site_ops", ScopeSite, 2},
	{PermSiteBackup, "站点备份", "导出 / 导入站点数据备份", "site_ops", ScopeSite, 3},
	{PermSiteSpiderOps, "爬虫运维", "全站爬虫任务触发、清理与重爬", "site_ops", ScopeSite, 4},
	{PermSiteProblemOps, "题库流水线运维", "题库识别/分析队列的运维操作", "site_ops", ScopeSite, 5},
	{PermSiteBlogBoard, "博客数据看板", "查看全站博客后台数据看板", "site_ops", ScopeSite, 6},

	{PermSiteOrgCreate, "创建组织", "创建新组织", "site_org", ScopeSite, 7},
	{PermSiteOrgDelete, "删除组织", "删除组织（不可恢复）", "site_org", ScopeSite, 8},
	{PermSiteOrgPolicy, "组织策略管理", "组织停用、席位上限、同步/总结间隔、强制同步等站点级策略字段", "site_org", ScopeSite, 9},
	{PermSiteOrgList, "查看全部组织", "查看全站所有组织列表", "site_org", ScopeSite, 10},

	{PermSiteUserList, "全站用户列表", "查看全站用户（跨组织）", "site_user", ScopeSite, 11},
	{PermSiteUserDisable, "禁用账号", "禁用 / 恢复用户账号登录", "site_user", ScopeSite, 12},
	{PermSiteUserDelete, "删除账号", "永久删除用户账号", "site_user", ScopeSite, 13},
	{PermSiteUserSync, "用户同步运维", "冻结/解冻同步、豁免休眠、个人同步间隔与题面开关覆盖", "site_user", ScopeSite, 14},

	{PermSiteAppointAdmin, "任命站点管理员", "设置 / 撤销站点管理员", "site_appoint", ScopeSite, 15},
	// bit 16 = 已下线的「任命资源审核员」，永久退休不复用
	{PermSiteRoleManage, "站点角色管理", "创建/编辑站点级自定义角色并分配成员", "site_appoint", ScopeSite, 17},

	{PermSiteBulletin, "全站公告", "发布 / 管理全站公告", "site_notice", ScopeSite, 18},
	{PermSiteEmergency, "紧急通知", "发布 / 管理全站紧急弹窗通知", "site_notice", ScopeSite, 19},

	{PermContentProblemReview, "题库审查", "审核题面/标签修改申请；本人修改自动通过", "content", ScopeSite, 20},
	{PermContentBlogModerate, "博客审核", "博客内容审核与精选", "content", ScopeSite, 21},
	{PermContentCommunityMod, "社区内容治理", "删除违规题解与评论", "content", ScopeSite, 22},

	{PermOrgInfoWrite, "组织信息设置", "组织品牌、名称、加入方式", "org_settings", ScopeOrg, 23},
	{PermOrgPolicyToggle, "组织功能开关", "本组织 AI 总结 / 邮件 / 爬虫开关", "org_settings", ScopeOrg, 24},
	{PermOrgRoleManage, "组织角色管理", "创建/编辑本组织自定义角色并分配成员", "org_settings", ScopeOrg, 25},

	{PermOrgMemberAdd, "拉人入组", "将用户加入本组织", "org_member", ScopeOrg, 26},
	{PermOrgMemberRemove, "移除成员", "将成员移出本组织", "org_member", ScopeOrg, 27},
	{PermOrgMemberRole, "设置成员角色", "任命本组织成员角色", "org_member", ScopeOrg, 28},
	{PermOrgMemberDisplayName, "改成员称呼", "修改成员的组织内名称", "org_member", ScopeOrg, 29},
	{PermOrgInviteView, "查看识别码", "查看团队识别码与邀请链接", "org_member", ScopeOrg, 30},
	{PermOrgInviteRotate, "更换识别码", "轮换团队识别码", "org_member", ScopeOrg, 31},
	{PermOrgJoinReview, "加入审批", "审批通过识别码提交的加入申请", "org_member", ScopeOrg, 32},

	{PermOrgGroupManage, "分组管理", "本组织分组的创建、调整与删除", "org_daily", ScopeOrg, 33},
	{PermOrgBulletinManage, "组织公告", "发布 / 管理本组织公告", "org_daily", ScopeOrg, 34},
	{PermOrgReportView, "训练报告与统计", "查看本组织训练报告与管理端统计", "org_daily", ScopeOrg, 35},
	{PermOrgMemberEmail, "代管日报开关", "代成员管理日报邮件开关", "org_daily", ScopeOrg, 36},

	// bit 只追加：content 组新增位排在末尾，分组展示顺序由 Groups() 决定，与 bit 无关
	{PermContentReportHandle, "举报处理", "查看与处理用户举报（博客/题解/评论）", "content", ScopeSite, 37},
}

var groupMeta = []struct{ Key, Label, Scope string }{
	{"site_ops", "站点 · 配置运维", ScopeSite},
	{"site_org", "站点 · 组织治理", ScopeSite},
	{"site_user", "站点 · 用户运维", ScopeSite},
	{"site_appoint", "站点 · 任命与角色", ScopeSite},
	{"site_notice", "站点 · 公告通知", ScopeSite},
	{"content", "内容审核", ScopeSite},
	{"org_settings", "组织 · 设置", ScopeOrg},
	{"org_member", "组织 · 成员", ScopeOrg},
	{"org_daily", "组织 · 日常管理", ScopeOrg},
}

var byCode = func() map[string]Perm {
	m := make(map[string]Perm, len(registry))
	for _, p := range registry {
		m[p.Code] = p
	}
	return m
}()

// All 全部权限点（bit 升序）
func All() []Perm { return registry }

// ByCode 查权限点；不存在则 ok=false
func ByCode(code string) (Perm, bool) {
	p, ok := byCode[code]
	return p, ok
}

// Valid code 是否已注册
func Valid(code string) bool {
	_, ok := byCode[code]
	return ok
}

// ScopeOf 权限点作用域；未注册返回空串
func ScopeOf(code string) string {
	if p, ok := byCode[code]; ok {
		return p.Scope
	}
	return ""
}

// AllCodes 全部权限 code
func AllCodes() []string {
	out := make([]string, 0, len(registry))
	for _, p := range registry {
		out = append(out, p.Code)
	}
	return out
}

// CodesByGroup 按分组筛选权限 code（如 content：内容审核相关）
func CodesByGroup(group string) []string {
	out := make([]string, 0, len(registry))
	for _, p := range registry {
		if p.Group == group {
			out = append(out, p.Code)
		}
	}
	return out
}

// CodesByScope 按作用域筛选权限 code
func CodesByScope(scope string) []string {
	out := make([]string, 0, len(registry))
	for _, p := range registry {
		if p.Scope == scope {
			out = append(out, p.Code)
		}
	}
	return out
}

// Groups 分组视图（供权限勾选矩阵）
func Groups() []Group {
	out := make([]Group, 0, len(groupMeta))
	for _, g := range groupMeta {
		grp := Group{Key: g.Key, Label: g.Label, Scope: g.Scope}
		for _, p := range registry {
			if p.Group == g.Key {
				grp.Perms = append(grp.Perms, p)
			}
		}
		out = append(out, grp)
	}
	return out
}
