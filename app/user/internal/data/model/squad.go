package model

import "time"

// Squad 分组内的分队（组织 → 分组 → 分队 → 成员）
type Squad struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	OrgID     uint   `gorm:"index;not null;comment:所属组织"`
	GroupID   uint   `gorm:"index;not null;comment:所属分组"`
	Name      string `gorm:"size:128;not null;comment:分队名称"`
	Describe  string `gorm:"size:512;comment:分队说明"`
}

func (Squad) TableName() string { return "squads" }

// SquadMember 分队成员（同一组织内一人可只属一个分队；跨分队以 squad_id 为准）
type SquadMember struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	SquadID   uint `gorm:"uniqueIndex:idx_squad_user;not null;comment:分队ID"`
	UserID    uint `gorm:"uniqueIndex:idx_squad_user;index;not null;comment:用户ID"`
}

func (SquadMember) TableName() string { return "squad_members" }

// OrgScopeGrant 组织内管理范围
// - 组长(group_leader)：绑定 group
// - 队长(captain)：绑定 squad
// - 教练 / 组织管理员：始终全组织，不使用 grant（有也忽略）
// - 旧数据：captain 无 grant 时兼容为全组织可见，直至重新任命
type OrgScopeGrant struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	OrgID     uint   `gorm:"uniqueIndex:idx_org_scope_user;not null;comment:组织ID"`
	UserID    uint   `gorm:"uniqueIndex:idx_org_scope_user;index;not null;comment:被授权用户"`
	ScopeType string `gorm:"size:16;uniqueIndex:idx_org_scope_user;not null;comment:group|squad"`
	ScopeID   uint   `gorm:"uniqueIndex:idx_org_scope_user;not null;comment:group_id 或 squad_id"`
}

func (OrgScopeGrant) TableName() string { return "org_scope_grants" }

const (
	ScopeTypeGroup = "group"
	ScopeTypeSquad = "squad"
)

func ValidScopeType(t string) bool {
	return t == ScopeTypeGroup || t == ScopeTypeSquad
}
