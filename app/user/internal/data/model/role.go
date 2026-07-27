package model

import "time"

// RBAC：角色 / 角色权限 / 用户角色。
// 权限点目录在 app/common/rbac（代码即权威）；DB 只存关联。
// 系统角色（is_system）权限集由代码锁定，启动时同步；自定义角色权限集存 role_permissions。
// 系统组织角色（member|captain|group_leader|coach|org_admin）以 org_members.role 为镜像源，双写保持一致。

// Role 角色（站点级 / 组织级；内置模板 / 自定义）
type Role struct {
	ID          uint `gorm:"primaryKey"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Code        string `gorm:"size:64;uniqueIndex;not null;comment:角色标识(系统:site_admin等;自定义:c_xxx)"`
	Name        string `gorm:"size:64;not null;comment:角色名称"`
	Description string `gorm:"size:255;comment:角色说明"`
	Scope       string `gorm:"size:8;not null;default:org;comment:site|org"`
	OrgID       uint   `gorm:"index;default:0;comment:0=站点级或全局模板;>0=组织自定义角色"`
	IsSystem    bool   `gorm:"default:false;comment:内置角色(权限集代码锁定,不可编辑删除)"`
}

// RolePermission 角色 → 权限点
type RolePermission struct {
	ID       uint   `gorm:"primaryKey"`
	RoleID   uint   `gorm:"uniqueIndex:idx_role_perm;index;not null"`
	PermCode string `gorm:"size:64;uniqueIndex:idx_role_perm;not null;comment:权限点code(见 app/common/rbac)"`
}

// OrgRolePerm 组织对内置组织角色（教练 / 队长）的权限覆盖。
// 有行 = 该组织已自定义（整集覆盖，空串表示该角色在本组织无任何权限）；
// 无行 = 用代码模板。团队管理员 / 成员是基本盘，不允许覆盖。
type OrgRolePerm struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	OrgID     uint   `gorm:"uniqueIndex:idx_org_role_perm;not null;comment:组织ID"`
	RoleCode  string `gorm:"size:64;uniqueIndex:idx_org_role_perm;not null;comment:内置组织角色code(coach|group_leader|captain)"`
	PermCodes string `gorm:"type:text;comment:权限code列表,逗号分隔;空串=本组织该角色无权限"`
}

// UserRole 用户 → 角色（org_id：站点级角色恒 0；组织级角色为组织 ID）
type UserRole struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UserID    uint `gorm:"uniqueIndex:idx_user_role_org;index;not null"`
	RoleID    uint `gorm:"uniqueIndex:idx_user_role_org;index;not null"`
	OrgID     uint `gorm:"uniqueIndex:idx_user_role_org;default:0;comment:组织级角色的组织ID;站点级=0"`
}
