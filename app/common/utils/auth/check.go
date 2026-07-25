package auth

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/permission"

	"github.com/go-kratos/kratos/v2/transport"
	"github.com/golang-jwt/jwt/v5"
)

// JwtPayload JWT 载荷
type JwtPayload struct {
	jwt.RegisteredClaims
	UserID             uint   `json:"userId"`
	Username           string `json:"username"`
	Name               string `json:"name"`
	Email              string `json:"email"`
	RoleID             int    `json:"roleId"` // 兼容旧字段
	IsSiteAdmin        bool   `json:"isSiteAdmin"`
	IsResourceReviewer bool   `json:"isResourceReviewer"`
	OrgID              uint   `json:"orgId"`
	OrgRole            string `json:"orgRole"` // member | coach | captain | org_admin
}

func parseJWTToken(ctx context.Context) string {
	header, _ := transport.FromServerContext(ctx)
	if header == nil {
		return ""
	}
	auths := strings.Fields(header.RequestHeader().Get("Authorization"))
	if len(auths) != 2 || !strings.EqualFold(auths[0], "Bearer") {
		return ""
	}
	return auths[1]
}

func parsePayload(ctx context.Context) *JwtPayload {
	tokenString := parseJWTToken(ctx)
	if tokenString == "" {
		return nil
	}
	keyFunc := func(token *jwt.Token) (interface{}, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(_const.JWTSecret()), nil
	}

	// 优先严格校验 iss/aud；旧 token 可能无 iss/aud，再宽松解析一次
	pd := &JwtPayload{}
	token, err := jwt.ParseWithClaims(
		tokenString,
		pd,
		keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("goalgo"),
		jwt.WithAudience("goalgo-web"),
	)
	if err != nil || !token.Valid || pd.UserID == 0 {
		pd = &JwtPayload{}
		token, err = jwt.ParseWithClaims(
			tokenString,
			pd,
			keyFunc,
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(),
		)
		if err != nil || !token.Valid || pd.UserID == 0 {
			// MapClaims 兜底：userId 可能是 float64 / json.Number
			if id := userIDFromMapToken(tokenString, keyFunc); id > 0 {
				pd = &JwtPayload{UserID: id}
				if mc, ok := parseMapClaims(tokenString, keyFunc); ok {
					pd.Username, _ = mc["username"].(string)
					pd.Name, _ = mc["name"].(string)
					pd.Email, _ = mc["email"].(string)
					pd.OrgRole, _ = mc["orgRole"].(string)
					pd.IsSiteAdmin = asBool(mc["isSiteAdmin"])
					pd.IsResourceReviewer = asBool(mc["isResourceReviewer"])
					pd.RoleID = asInt(mc["roleId"])
					pd.OrgID = uint(asInt(mc["orgId"]))
				}
			} else {
				return nil
			}
		}
	}
	// 兼容：旧 token 无 isSiteAdmin 时用 roleId==1
	if !pd.IsSiteAdmin && pd.RoleID == permission.RoleAdmin {
		pd.IsSiteAdmin = true
	}
	return pd
}

func parseMapClaims(tokenString string, keyFunc jwt.Keyfunc) (jwt.MapClaims, bool) {
	token, err := jwt.Parse(tokenString, keyFunc, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil, false
	}
	mc, ok := token.Claims.(jwt.MapClaims)
	return mc, ok
}

func userIDFromMapToken(tokenString string, keyFunc jwt.Keyfunc) uint {
	mc, ok := parseMapClaims(tokenString, keyFunc)
	if !ok {
		return 0
	}
	return uint(asInt(mc["userId"]))
}

func asInt(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case float32:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	default:
		return 0
	}
}

func asBool(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case string:
		return t == "true" || t == "1"
	default:
		return false
	}
}

// VerifyMinRole 兼容旧权限序
func VerifyMinRole(ctx context.Context, minRole int) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin {
		return true
	}
	// 组织 staff（教练/队长/组织管理员）≈ 旧教练级（管理端）
	if isOrgStaffRole(pd.OrgRole) && permission.RoleRank(minRole) <= permission.RoleRank(permission.RoleCoach) {
		return true
	}
	return permission.RoleRank(pd.RoleID) >= permission.RoleRank(minRole)
}

func isOrgStaffRole(role string) bool {
	return role == "coach" || role == "captain" || role == "org_admin"
}

// VerifySelfOrAbove 自己或站点管理员
func VerifySelfOrAbove(ctx context.Context, targetUserId uint) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin {
		return true
	}
	return pd.UserID == targetUserId
}

func GetCurrentUser(ctx context.Context) *JwtPayload {
	return parsePayload(ctx)
}

func GetCurrentUserId(ctx context.Context) uint {
	pd := parsePayload(ctx)
	if pd == nil {
		return 0
	}
	return pd.UserID
}

// VerifyAdmin / VerifySiteAdmin 站点管理员
func VerifyAdmin(ctx context.Context) bool {
	return VerifySiteAdmin(ctx)
}

func VerifySiteAdmin(ctx context.Context) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	return pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin
}

// IsContentModerator 站管或资源审核员（题库审核 / 博客 moderate / 举报处理）
func IsContentModerator(pd *JwtPayload) bool {
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin {
		return true
	}
	return pd.IsResourceReviewer
}

// VerifyContentModerator 站点管理员或资源审核员
func VerifyContentModerator(ctx context.Context) bool {
	return IsContentModerator(parsePayload(ctx))
}

// VerifyResourceReviewer 仅资源审核员（不含站管）；一般业务用 VerifyContentModerator
func VerifyResourceReviewer(ctx context.Context) bool {
	pd := parsePayload(ctx)
	return pd != nil && pd.IsResourceReviewer
}

// VerifyOrgAdmin 当前 JWT 组织的组织管理员，或站点管理员
func VerifyOrgAdmin(ctx context.Context) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin {
		return true
	}
	return pd.OrgRole == "org_admin" && pd.OrgID > 0
}

// VerifyOrgAdminOf 指定组织的管理员（JWT org 匹配或站点管理员；业务层应再查库）
func VerifyOrgAdminOf(ctx context.Context, orgID uint) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin {
		return true
	}
	return pd.OrgRole == "org_admin" && pd.OrgID == orgID
}

func VerifyCoach(ctx context.Context) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	return pd.RoleID == permission.RoleCoach
}

// VerifyStaff 管理端：站点管理员 或 当前组织教练/队长/组织管理员 或 旧 staff role
func VerifyStaff(ctx context.Context) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || isOrgStaffRole(pd.OrgRole) {
		return true
	}
	return permission.IsStaff(pd.RoleID)
}

// VerifyOrgCoach 当前组织教练及以上（coach/captain/org_admin）或站点管理员
func VerifyOrgCoach(ctx context.Context) bool {
	return VerifyStaff(ctx)
}

func VerifyCaptain(ctx context.Context) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin {
		return true
	}
	if pd.OrgRole == "captain" || pd.OrgRole == "org_admin" {
		return true
	}
	return pd.RoleID == permission.RoleCaptain
}
