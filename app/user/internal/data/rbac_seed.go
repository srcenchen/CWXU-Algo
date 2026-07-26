package data

import (
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// patchRbacBootstrap 存量角色 → user_roles 的一次性迁移标记。
// ⚠️ 仅系统角色的「权限集」允许每次启动重同步（代码锁定，非用户数据）；
// 用户 ↔ 角色 的指派迁移必须一次性（schema_patches），否则会覆盖管理员后续调整。
const patchRbacBootstrap = "rbac_bootstrap_v1"

// seedRbac 内置角色种子 + 权限集同步 + 存量指派一次性迁移 + 孤儿清理。幂等。
func seedRbac(db *gorm.DB) {
	if db == nil {
		return
	}
	// 1. 内置角色行：缺则建；权限集与代码模板强制对齐（系统角色锁定，可随版本升级）
	for _, sr := range rbac.SystemRoles() {
		var r model.Role
		err := db.Where("code = ?", sr.Code).First(&r).Error
		if err == gorm.ErrRecordNotFound {
			r = model.Role{Code: sr.Code, Name: sr.Name, Description: sr.Desc, Scope: sr.Scope, OrgID: 0, IsSystem: true}
			if e := db.Create(&r).Error; e != nil {
				log.Errorf("rbac seed role %s: %v", sr.Code, e)
				continue
			}
		} else if err != nil {
			log.Errorf("rbac query role %s: %v", sr.Code, err)
			continue
		} else {
			// 已存在：确保元数据与系统标记正确（防历史手工数据漂移）
			_ = db.Model(&r).Updates(map[string]interface{}{
				"name": sr.Name, "description": sr.Desc, "scope": sr.Scope, "org_id": 0, "is_system": true,
			}).Error
		}
		syncRolePerms(db, r.ID, sr.Perms)
	}

	// 2. 存量指派一次性迁移（claimSchemaPatch：唯一插入认领，多实例并发安全）
	if claimSchemaPatch(db, patchRbacBootstrap) {
		bootstrapUserRoles(db)
		log.Info("rbac bootstrap: 存量角色已迁移至 user_roles")
	}

	// 3. 孤儿清理（用户/组织/角色已删）：与 org_members 孤儿清理同思路，可重复执行
	_ = db.Exec(`DELETE FROM user_roles ur WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = ur.user_id)`).Error
	_ = db.Exec(`DELETE FROM user_roles ur WHERE NOT EXISTS (SELECT 1 FROM roles r WHERE r.id = ur.role_id)`).Error
	_ = db.Exec(`DELETE FROM user_roles ur WHERE ur.org_id > 0 AND NOT EXISTS (SELECT 1 FROM orgs o WHERE o.id = ur.org_id)`).Error
	_ = db.Exec(`DELETE FROM role_permissions rp WHERE NOT EXISTS (SELECT 1 FROM roles r WHERE r.id = rp.role_id)`).Error
	// 自定义组织角色的宿主组织已删
	_ = db.Exec(`DELETE FROM roles r WHERE r.org_id > 0 AND NOT EXISTS (SELECT 1 FROM orgs o WHERE o.id = r.org_id)`).Error
}

// syncRolePerms 将角色权限集对齐到 want（增缺删多）。仅用于系统角色。
func syncRolePerms(db *gorm.DB, roleID uint, want []string) {
	wantSet := make(map[string]bool, len(want))
	for _, c := range want {
		if rbac.Valid(c) {
			wantSet[c] = true
		}
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

// bootstrapUserRoles 存量身份 → user_roles：
// users.is_site_admin / is_resource_reviewer → 站点角色；org_members.role → 组织模板角色。
func bootstrapUserRoles(db *gorm.DB) {
	if err := db.Exec(`
		INSERT INTO user_roles (created_at, user_id, role_id, org_id)
		SELECT NOW(), u.id, r.id, 0
		FROM users u JOIN roles r ON r.code = ?
		WHERE u.is_site_admin = true
		ON CONFLICT DO NOTHING
	`, rbac.RoleSiteAdmin).Error; err != nil {
		log.Errorf("rbac bootstrap site_admin: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO user_roles (created_at, user_id, role_id, org_id)
		SELECT NOW(), u.id, r.id, 0
		FROM users u JOIN roles r ON r.code = ?
		WHERE u.is_resource_reviewer = true
		ON CONFLICT DO NOTHING
	`, rbac.RoleResourceReviewer).Error; err != nil {
		log.Errorf("rbac bootstrap resource_reviewer: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO user_roles (created_at, user_id, role_id, org_id)
		SELECT NOW(), m.user_id, r.id, m.org_id
		FROM org_members m
		JOIN roles r ON r.code = m.role AND r.scope = 'org' AND r.is_system = true
		JOIN users u ON u.id = m.user_id
		ON CONFLICT DO NOTHING
	`).Error; err != nil {
		log.Errorf("rbac bootstrap org members: %v", err)
	}
}
