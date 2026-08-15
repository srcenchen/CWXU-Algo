package model

import "time"

const (
	OrgRoleMember      = "member"
	OrgRoleCaptain     = "captain"
	OrgRoleGroupLeader = "group_leader" // 组长：绑定分组
	OrgRoleCoach       = "coach"
	OrgRoleOrgAdmin    = "org_admin"

	OrgJoinAuto   = "auto"
	OrgJoinReview = "review"

	OrgStatusActive    = "active"
	OrgStatusSuspended = "suspended"

	PublicOrgSlug = "public"
	PublicOrgName = "公共域"

	JoinReqPending  = "pending"
	JoinReqApproved = "approved"
	JoinReqRejected = "rejected"

	InvitePending   = "pending"
	InviteAccepted  = "accepted"
	InviteRejected  = "rejected"
	InviteCancelled = "cancelled"
)

// 组织内角色等级：组织管理员 > 教练 > 组长 > 队长 > 成员
const (
	OrgRoleRankMember      = 0
	OrgRoleRankCaptain     = 10
	OrgRoleRankGroupLeader = 20
	OrgRoleRankCoach       = 30
	OrgRoleRankOrgAdmin    = 40
)

// Org 组织/校队（含系统「公共域」）
type Org struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	Name      string     `gorm:"size:128;not null;comment:组织名称"`
	Slug      string     `gorm:"size:64;uniqueIndex;comment:URL 标识"`
	Plan      string     `gorm:"size:32;default:free;comment:套餐 free|team|pro"`
	SeatLimit int        `gorm:"default:50;comment:用户数上限(席位)；默认50；公共域仅计仅属公共域的用户"`
	ExpireAt  *time.Time `gorm:"comment:套餐到期"`
	Status    string     `gorm:"size:16;default:active;comment:active|suspended"`
	IsSystem  bool       `gorm:"default:false;comment:系统组织(公共域)"`

	BrandTitle   string `gorm:"size:128;comment:组织品牌标题"`
	BrandLogo    string `gorm:"size:512;comment:组织 logo"`
	BrandFavicon string `gorm:"size:512;comment:组织 favicon"`

	JoinMode   string `gorm:"size:16;default:auto;comment:auto|review"`
	InviteCode string `gorm:"size:32;uniqueIndex;comment:团队识别码"`

	// 策略：开关可由组织管理员改；间隔仅站点管理员可写
	// EnableAIEmail 已废弃：日报邮件不再要求组织授权，仅保留字段兼容历史数据
	EnableAIEmail       bool `gorm:"default:true;comment:AI日报邮件(组织授权,已废弃)"`
	EnableAIWeeklyEmail bool `gorm:"default:true;comment:AI周报邮件(组织授权,staff)"`
	EnableSpider        bool `gorm:"default:true;comment:爬虫定时开关"`

	SpiderIntervalMin int    `gorm:"default:60;comment:爬虫间隔分钟(站点写)"`
	AIEmailSchedule   string `gorm:"size:64;default:30 7 * * *;comment:邮件 cron(站点写)"`

	DailySyncLimit int `gorm:"default:0;comment:组织日同步上限 0=未启用"`

	// 题面流水线开关（站点管理员可写；默认 true 兼容存量组织）
	EnableFetchProblem bool `gorm:"default:true;comment:题面爬取开关(站管)"`
	EnableAiAnalyze    bool `gorm:"default:true;comment:题面AI分析开关(站管)"`

	// ForceSync 本队强制同步（集训/比赛期跳过成员休眠）；仅站点管理员可写
	ForceSync bool `gorm:"default:false;comment:强制同步跳过休眠(站管)"`
}

// OrgMember 用户与组织关系
type OrgMember struct {
	ID             uint `gorm:"primaryKey"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	OrgID          uint      `gorm:"uniqueIndex:idx_org_user;not null;comment:组织ID"`
	UserID         uint      `gorm:"uniqueIndex:idx_org_user;index;not null;comment:用户ID"`
	Role           string    `gorm:"size:16;default:member;comment:member|captain|group_leader|coach|org_admin"`
	GroupID        *uint     `gorm:"index;comment:组织内分组"`
	OrgDisplayName string    `gorm:"size:64;comment:组织内名称(仅本组织展示)"`
	JoinedAt       time.Time `gorm:"comment:加入时间"`
}

// ValidOrgRole 组织内角色是否合法
func ValidOrgRole(role string) bool {
	switch role {
	case OrgRoleMember, OrgRoleCaptain, OrgRoleGroupLeader, OrgRoleCoach, OrgRoleOrgAdmin:
		return true
	default:
		return false
	}
}

// OrgRoleRank 角色等级（越高权限越大）
func OrgRoleRank(role string) int {
	switch role {
	case OrgRoleOrgAdmin:
		return OrgRoleRankOrgAdmin
	case OrgRoleCoach:
		return OrgRoleRankCoach
	case OrgRoleGroupLeader:
		return OrgRoleRankGroupLeader
	case OrgRoleCaptain:
		return OrgRoleRankCaptain
	default:
		return OrgRoleRankMember
	}
}

// IsOrgStaffRole 组织内可进管理端的角色（教练/组长/队长/组织管理员）
func IsOrgStaffRole(role string) bool {
	return role == OrgRoleCoach || role == OrgRoleGroupLeader ||
		role == OrgRoleCaptain || role == OrgRoleOrgAdmin
}

// IsOrgFullScopeRole 全组织数据可见、不受分组/分队 scope 限制
func IsOrgFullScopeRole(role string) bool {
	return role == OrgRoleOrgAdmin || role == OrgRoleCoach
}

// RoleNeedsScope 任命该角色时必须绑定管理范围
// captain → squad；group_leader → group
func RoleNeedsScope(role string) (scopeType string, ok bool) {
	switch role {
	case OrgRoleCaptain:
		return ScopeTypeSquad, true
	case OrgRoleGroupLeader:
		return ScopeTypeGroup, true
	default:
		return "", false
	}
}

// CanAppointOrgRole 操作者能否把目标设为 newRole。
// actorRole / targetCurrentRole / newRole 均为 org_members.role。
// 收紧规则（统一生效，组织管理员同样受限，站管在 service 层另开通道）：
//   - 队长及以下无任命权（仅组长及以上可任命）
//   - 可任命到自己同级职务（允许把下级提高到与自己同级）
//   - 不可任命高于自己的职务
//   - 不可动比自己高的人
//   - 同级别目标：不可降级（只能保持同级）
func CanAppointOrgRole(actorRole, targetCurrentRole, newRole string) bool {
	ar := OrgRoleRank(actorRole)
	// 仅组长及以上可任命
	if ar < OrgRoleRankGroupLeader {
		return false
	}
	// 不能任命高于自己的职务
	if OrgRoleRank(newRole) > ar {
		return false
	}
	tr := OrgRoleRank(targetCurrentRole)
	// 不能动比自己高的人
	if tr > ar {
		return false
	}
	// 同级别：不能降级（只能保持同级）
	if tr == ar && OrgRoleRank(newRole) < ar {
		return false
	}
	return true
}

// EffectiveRoleFromGrants 根据管理范围与当前角色推算展示/JWT 用角色。
// 组织管理员、教练保持不变；否则有分组 grant→组长，有分队 grant→队长，否则成员。
// 一人可同时有多组/多队 grant，角色取最高领导档。
func EffectiveRoleFromGrants(currentRole string, hasGroupGrant, hasSquadGrant bool) string {
	if currentRole == OrgRoleOrgAdmin || currentRole == OrgRoleCoach {
		return currentRole
	}
	if hasGroupGrant {
		return OrgRoleGroupLeader
	}
	if hasSquadGrant {
		return OrgRoleCaptain
	}
	if currentRole == OrgRoleGroupLeader || currentRole == OrgRoleCaptain {
		return OrgRoleMember
	}
	if currentRole == "" {
		return OrgRoleMember
	}
	return currentRole
}

// OrgJoinRequest 团队识别码加入申请（join_mode=review）
type OrgJoinRequest struct {
	ID             uint `gorm:"primaryKey"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	OrgID          uint   `gorm:"uniqueIndex:idx_org_join_user;not null"`
	UserID         uint   `gorm:"uniqueIndex:idx_org_join_user;not null"`
	Status         string `gorm:"size:16;default:pending;comment:pending|approved|rejected"`
	CodeUsed       string `gorm:"size:32"`
	OrgDisplayName string `gorm:"size:64;comment:申请时填写的组织内名称"`
	ReviewedBy     *uint  `gorm:"comment:审批人"`
}

// OrgInvite 组织管理员主动邀请（org.member.add 权限），需被邀请人同意后才成为成员。
// 与 OrgJoinRequest 方向相反：JoinRequest 是用户主动申请加入；Invite 是组织拉人。
type OrgInvite struct {
	ID             uint `gorm:"primaryKey"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	OrgID          uint   `gorm:"index:idx_org_invite_user;not null;comment:组织"`
	UserID         uint   `gorm:"index:idx_org_invite_user;not null;comment:被邀请人"`
	InviterID      uint   `gorm:"comment:邀请人"`
	Status         string `gorm:"size:16;default:pending;comment:pending|accepted|rejected|cancelled"`
	Role           string `gorm:"size:32;default:member;comment:入组角色"`
	OrgDisplayName string `gorm:"size:64;comment:入组后组织内名称"`
}

func (OrgInvite) TableName() string { return "org_invites" }

// PlanQuota 套餐配额模板
// 表名须显式指定：GORM 默认 inflection 会把 PlanQuota 收成 plan_quota（非 plan_quotas）。
type PlanQuota struct {
	ID                uint `gorm:"primaryKey"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Plan              string `gorm:"size:32;uniqueIndex;not null"`
	SeatLimit         int    `gorm:"default:20"`
	DailySyncPerUser  int    `gorm:"default:24"`
	AISummaryPerMonth int    `gorm:"default:0"`
}

func (PlanQuota) TableName() string { return "plan_quota" }
