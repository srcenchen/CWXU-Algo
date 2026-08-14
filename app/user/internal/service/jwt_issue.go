package service

import (
	"time"

	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

// JWTAccessTTL 默认访问令牌有效期。登录 / refresh / 切组织均签发此时长；
// 客户端在有效期内可调用 refresh 滚动续期。
const JWTAccessTTL = 7 * 24 * time.Hour

// IssueJWT 签发含组织上下文的 JWT
func IssueJWT(db *gorm.DB, u *model.User) (string, error) {
	orgID := u.CurrentOrgID
	orgRole := ""
	if orgID == 0 {
		var pub model.Org
		if err := db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error; err == nil {
			orgID = pub.ID
		}
	}
	if orgID > 0 {
		var m model.OrgMember
		if err := db.Where("org_id = ? AND user_id = ?", orgID, u.ID).First(&m).Error; err == nil {
			orgRole = m.Role
		} else {
			// 当前组织无效 → 回落公共域
			var pub model.Org
			if err := db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error; err == nil {
				orgID = pub.ID
				_ = db.Model(u).Update("current_org_id", orgID)
				var m2 model.OrgMember
				if err := db.Where("org_id = ? AND user_id = ?", orgID, u.ID).First(&m2).Error; err == nil {
					orgRole = m2.Role
				}
			}
		}
	}

	roleID := u.RoleID
	if u.IsSiteAdmin {
		roleID = 1
	} else if roleID == 1 {
		// 非站点管理员不应保留 admin roleId 语义
		roleID = 0
	}

	roleIdsJSON := []byte("[0]")
	if u.IsSiteAdmin || model.IsOrgStaffRole(orgRole) {
		roleIdsJSON = []byte("[1]")
	}

	// pm：细粒度权限位图（站点权限 ∪ 当前组织权限），见 app/common/rbac。
	// 站点管理员在校验侧旁路，但仍编码全量便于前端直接渲染。
	pm := rbac.Encode(collectUserPerms(db, u, orgID, orgRole))

	// 默认可续期访问令牌；前端活跃时通过 refresh 从 DB 重签，滚动续期并同步权限。
	now := time.Now()
	expire := now.Add(JWTAccessTTL)
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"userId":             u.ID,
		"username":           u.Username,
		"name":               u.Name,
		"roleId":             roleID,
		"roleIds":            string(roleIdsJSON),
		"isSiteAdmin":        u.IsSiteAdmin,
		"orgId":              orgID,
		"orgRole":            orgRole,
		"pm":                 pm,
		"exp":                expire.Unix(),
		"nbf":                now.Unix(),
		"iat":                now.Unix(),
		"iss":                "goalgo",
		"aud":                "goalgo-web",
	}).SignedString(_const.JWTPrivateKey())
}
