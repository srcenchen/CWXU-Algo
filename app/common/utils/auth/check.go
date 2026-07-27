package auth

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/permission"
	"cwxu-algo/app/common/rbac"

	"github.com/go-kratos/kratos/v2/transport"
	"github.com/golang-jwt/jwt/v5"
)

// JWT 解析结果小缓存：同一请求内多次 Verify* 会重复走签名校验，开销可观。
// 不改任何导出签名，仅在包内以 token 字符串为 key 短缓存成功解析结果；
// 条目过期取 min(token exp, 60s)，容量满时整体清空（简单胜过 LRU）。
const (
	jwtCacheMaxEntries = 2048
	jwtCacheTTL        = 60 * time.Second
)

type jwtCacheEntry struct {
	pd  JwtPayload
	exp time.Time
}

var (
	jwtCacheMu sync.Mutex
	jwtCache   = map[string]jwtCacheEntry{}
)

func cachedPayload(token string) *JwtPayload {
	jwtCacheMu.Lock()
	defer jwtCacheMu.Unlock()
	e, ok := jwtCache[token]
	if !ok {
		return nil
	}
	if time.Now().After(e.exp) {
		delete(jwtCache, token)
		return nil
	}
	pd := e.pd // 返回副本，避免调用方改写缓存
	return &pd
}

func storePayload(token string, pd *JwtPayload) {
	if pd == nil {
		return
	}
	exp := time.Now().Add(jwtCacheTTL)
	if pd.ExpiresAt != nil && pd.ExpiresAt.Time.Before(exp) {
		exp = pd.ExpiresAt.Time
	}
	if !time.Now().Before(exp) {
		return
	}
	jwtCacheMu.Lock()
	defer jwtCacheMu.Unlock()
	if len(jwtCache) >= jwtCacheMaxEntries {
		jwtCache = make(map[string]jwtCacheEntry, jwtCacheMaxEntries/4)
	}
	jwtCache[token] = jwtCacheEntry{pd: *pd, exp: exp}
}

// JwtPayload JWT 载荷
type JwtPayload struct {
	jwt.RegisteredClaims
	UserID             uint   `json:"userId"`
	Username           string `json:"username"`
	Name               string `json:"name"`
	Email              string `json:"email"`
	RoleID             int    `json:"roleId"` // 兼容旧字段
	IsSiteAdmin        bool   `json:"isSiteAdmin"`
	OrgID              uint   `json:"orgId"`
	OrgRole            string `json:"orgRole"` // member | captain | group_leader | coach | org_admin
	Pm                 string `json:"pm,omitempty"` // 权限位图（站点权限 ∪ 当前组织权限），见 app/common/rbac
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
	if pd := cachedPayload(tokenString); pd != nil {
		return pd
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
					pd.RoleID = asInt(mc["roleId"])
					pd.OrgID = uint(asInt(mc["orgId"]))
					pd.Pm, _ = mc["pm"].(string)
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
	storePayload(tokenString, pd)
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
	// 组织 staff（教练/组长/队长/组织管理员）≈ 旧教练级（管理端）
	if isOrgStaffRole(pd.OrgRole) && permission.RoleRank(minRole) <= permission.RoleRank(permission.RoleCoach) {
		return true
	}
	return permission.RoleRank(pd.RoleID) >= permission.RoleRank(minRole)
}

func isOrgStaffRole(role string) bool {
	return role == "coach" || role == "group_leader" || role == "captain" || role == "org_admin"
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

// IsContentModerator 具备任一内容审核权限（题库审查 / 博客审核 / 社区治理 / 举报处理）。
// 资源审核员内置身份已下线，改由站点自定义角色按权限点授予。
func IsContentModerator(pd *JwtPayload) bool {
	if pd == nil {
		return false
	}
	for _, code := range rbac.ContentPerms() {
		if PayloadHasPerm(pd, code) {
			return true
		}
	}
	return false
}

// VerifyContentModerator 当前请求是否具备任一内容审核权限
func VerifyContentModerator(ctx context.Context) bool {
	return IsContentModerator(parsePayload(ctx))
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

// —— 细粒度权限（RBAC）——
// 权威顺序：站点管理员旁路 → JWT pm 位图 → 旧 token 按旧 claims 推导。
// 组织级权限只代表「当前 JWT 组织」内的授权；跨组织操作须走 DB 兜底（user 服务 hasPermInOrgDB）。

// PayloadHasPerm 按 payload 判定权限
func PayloadHasPerm(pd *JwtPayload, code string) bool {
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin {
		return true
	}
	if has, valid := rbac.MaskHas(pd.Pm, code); valid {
		return has
	}
	// 旧 token（无 pm）：按组织角色模板推导
	return rbac.LegacyHas(code, pd.OrgRole)
}

// HasPerm 当前请求是否具备权限（站点级权限，或当前 JWT 组织内的组织级权限）
func HasPerm(ctx context.Context, code string) bool {
	return PayloadHasPerm(parsePayload(ctx), code)
}

// HasOrgPerm 当前请求是否对指定组织具备组织级权限（仅信 JWT：要求 JWT 组织与目标一致）。
// JWT 组织不一致时返回 false，由业务层决定是否查库兜底。
func HasOrgPerm(ctx context.Context, orgID uint, code string) bool {
	pd := parsePayload(ctx)
	if pd == nil {
		return false
	}
	if pd.IsSiteAdmin || pd.RoleID == permission.RoleAdmin {
		return true
	}
	if orgID == 0 || pd.OrgID != orgID {
		return false
	}
	return PayloadHasPerm(pd, code)
}
