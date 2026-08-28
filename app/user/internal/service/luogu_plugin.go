package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	pb "cwxu-algo/api/user/v1/plugin"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	LuoguPluginRiskVersion = "2026-08-28-v1"
	LuoguPluginScope       = "luogu.sync"

	luoguPluginProvider       = "luogu"
	luoguAuthorizationCodeTTL = 2 * time.Minute
	luoguDeviceTokenTTL       = 90 * 24 * time.Hour
	luoguRevokedMinimumTTL    = 30 * time.Minute
)

var (
	luoguUIDPattern        = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	pkceVerifierPattern    = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
	consumeLuoguCodeScript = redis.NewScript(`
local value = redis.call("GET", KEYS[1])
if value then
  redis.call("DEL", KEYS[1])
end
return value
`)
)

type luoguAuthorizationCode struct {
	UserID        uint      `json:"userId"`
	LuoguUID      string    `json:"luoguUid"`
	ClientKind    string    `json:"clientKind"`
	ClientVersion string    `json:"clientVersion"`
	CodeChallenge string    `json:"codeChallenge"`
	State         string    `json:"state"`
	RiskVersion   string    `json:"riskVersion"`
	AcceptedAt    time.Time `json:"acceptedAt"`
}

// LuoguPluginService issues and validates least-privilege browser-sync device
// credentials. Authorization codes live only in Redis; device tokens live only
// as SHA-256 digests in PostgreSQL.
type LuoguPluginService struct {
	pb.UnimplementedLuoguPluginServer
	db  *gorm.DB
	rdb *redis.Client
	now func() time.Time
}

func NewLuoguPluginService(d *data.Data) *LuoguPluginService {
	return &LuoguPluginService{db: d.DB, rdb: d.RDB, now: time.Now}
}

func luoguPluginError(status int, reason, message string) error {
	return kerrors.New(status, reason, message)
}

func randomLuoguSecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func hashLuoguPluginToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func luoguAuthorizationCodeKey(code string) string {
	return "luogu:plugin:authorize-code:" + strings.TrimPrefix(hashLuoguPluginToken(code), "sha256:")
}

func luoguPluginRevokedKey(authorizationID uint64) string {
	return fmt.Sprintf("luogu:plugin:authorization:revoked:%d", authorizationID)
}

func validateLuoguAuthorizeRequest(req *pb.AuthorizeCodeReq) error {
	if req == nil {
		return luoguPluginError(http.StatusBadRequest, "INVALID_REQUEST", "授权参数不完整")
	}
	if !req.RiskAccepted {
		return luoguPluginError(http.StatusBadRequest, "RISK_ACCEPTANCE_REQUIRED", "请先阅读并确认风险协议")
	}
	if req.RiskVersion != LuoguPluginRiskVersion {
		return luoguPluginError(http.StatusBadRequest, "RISK_VERSION_MISMATCH", "风险协议版本已更新，请重新确认")
	}
	if req.Scope != LuoguPluginScope {
		return luoguPluginError(http.StatusBadRequest, "INVALID_SCOPE", "授权范围无效")
	}
	if !luoguUIDPattern.MatchString(req.LuoguUid) {
		return luoguPluginError(http.StatusBadRequest, "INVALID_LUOGU_UID", "洛谷 UID 无效")
	}
	if req.ClientKind != "userscript" && req.ClientKind != "chrome-extension" {
		return luoguPluginError(http.StatusBadRequest, "INVALID_CLIENT_KIND", "客户端类型无效")
	}
	if len(strings.TrimSpace(req.ClientVersion)) == 0 || len(req.ClientVersion) > 64 {
		return luoguPluginError(http.StatusBadRequest, "INVALID_CLIENT_VERSION", "客户端版本无效")
	}
	if req.CodeChallengeMethod != "S256" || len(req.CodeChallenge) != 43 {
		return luoguPluginError(http.StatusBadRequest, "INVALID_PKCE_CHALLENGE", "PKCE challenge 无效")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(req.CodeChallenge)
	if err != nil || len(challenge) != sha256.Size {
		return luoguPluginError(http.StatusBadRequest, "INVALID_PKCE_CHALLENGE", "PKCE challenge 无效")
	}
	if len(req.State) < 16 || len(req.State) > 256 {
		return luoguPluginError(http.StatusBadRequest, "INVALID_STATE", "state 无效")
	}
	return nil
}

func (s *LuoguPluginService) AuthorizeCode(ctx context.Context, req *pb.AuthorizeCodeReq) (*pb.AuthorizeCodeRes, error) {
	current := auth.GetCurrentUser(ctx)
	if current == nil || current.UserID == 0 {
		return nil, luoguPluginError(http.StatusUnauthorized, "UNAUTHENTICATED", "请先登录")
	}
	if err := validateLuoguAuthorizeRequest(req); err != nil {
		return nil, err
	}
	code, err := randomLuoguSecret()
	if err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "AUTHORIZATION_CODE_CREATE_FAILED", "创建授权码失败")
	}
	now := s.now().UTC()
	payload, err := json.Marshal(luoguAuthorizationCode{
		UserID:        current.UserID,
		LuoguUID:      req.LuoguUid,
		ClientKind:    req.ClientKind,
		ClientVersion: req.ClientVersion,
		CodeChallenge: req.CodeChallenge,
		State:         req.State,
		RiskVersion:   req.RiskVersion,
		AcceptedAt:    now,
	})
	if err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "AUTHORIZATION_CODE_CREATE_FAILED", "创建授权码失败")
	}
	if err := s.rdb.Set(ctx, luoguAuthorizationCodeKey(code), payload, luoguAuthorizationCodeTTL).Err(); err != nil {
		return nil, luoguPluginError(http.StatusServiceUnavailable, "AUTHORIZATION_CODE_STORE_FAILED", "授权服务暂时不可用")
	}
	return &pb.AuthorizeCodeRes{
		Code: code, State: req.State, ExpiresAt: now.Add(luoguAuthorizationCodeTTL).Unix(), Scope: LuoguPluginScope,
	}, nil
}

func (s *LuoguPluginService) Token(ctx context.Context, req *pb.TokenReq) (*pb.TokenRes, error) {
	if req == nil || strings.TrimSpace(req.Code) == "" {
		return nil, luoguPluginError(http.StatusUnauthorized, "AUTHORIZATION_CODE_INVALID", "授权码无效或已过期")
	}
	if req.Scope != LuoguPluginScope {
		return nil, luoguPluginError(http.StatusBadRequest, "INVALID_SCOPE", "授权范围无效")
	}
	raw, err := consumeLuoguCodeScript.Run(ctx, s.rdb, []string{luoguAuthorizationCodeKey(req.Code)}).Text()
	if err == redis.Nil {
		return nil, luoguPluginError(http.StatusUnauthorized, "AUTHORIZATION_CODE_INVALID", "授权码无效或已过期")
	}
	if err != nil {
		return nil, luoguPluginError(http.StatusServiceUnavailable, "AUTHORIZATION_CODE_STORE_FAILED", "授权服务暂时不可用")
	}
	var grant luoguAuthorizationCode
	if err := json.Unmarshal([]byte(raw), &grant); err != nil {
		return nil, luoguPluginError(http.StatusUnauthorized, "AUTHORIZATION_CODE_INVALID", "授权码无效或已过期")
	}
	if req.State != grant.State {
		return nil, luoguPluginError(http.StatusUnauthorized, "AUTHORIZATION_STATE_MISMATCH", "授权状态校验失败")
	}
	if !pkceVerifierPattern.MatchString(req.Verifier) {
		return nil, luoguPluginError(http.StatusUnauthorized, "PKCE_VERIFICATION_FAILED", "PKCE 校验失败")
	}
	verifierSum := sha256.Sum256([]byte(req.Verifier))
	if base64.RawURLEncoding.EncodeToString(verifierSum[:]) != grant.CodeChallenge {
		return nil, luoguPluginError(http.StatusUnauthorized, "PKCE_VERIFICATION_FAILED", "PKCE 校验失败")
	}
	if grant.RiskVersion != LuoguPluginRiskVersion {
		return nil, luoguPluginError(http.StatusForbidden, "RISK_REACCEPT_REQUIRED", "风险协议已更新，请重新授权")
	}

	deviceToken, err := randomLuoguSecret()
	if err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "DEVICE_TOKEN_CREATE_FAILED", "创建设备授权失败")
	}
	now := s.now().UTC()
	authorization := model.PluginAuthorization{
		UserID:        grant.UserID,
		Provider:      luoguPluginProvider,
		ClientKind:    grant.ClientKind,
		ClientVersion: grant.ClientVersion,
		LuoguUID:      grant.LuoguUID,
		TokenHash:     hashLuoguPluginToken(deviceToken),
		RiskVersion:   grant.RiskVersion,
		AcceptedAt:    grant.AcceptedAt,
		ExpiresAt:     now.Add(luoguDeviceTokenTTL),
	}
	if err := s.db.WithContext(ctx).Create(&authorization).Error; err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "DEVICE_TOKEN_CREATE_FAILED", "创建设备授权失败")
	}
	return &pb.TokenRes{
		DeviceToken:     deviceToken,
		AuthorizationId: uint64(authorization.ID),
		ExpiresAt:       authorization.ExpiresAt.Unix(),
		Scope:           LuoguPluginScope,
	}, nil
}

func (s *LuoguPluginService) ListAuthorizations(ctx context.Context, _ *pb.ListAuthorizationsReq) (*pb.ListAuthorizationsRes, error) {
	current := auth.GetCurrentUser(ctx)
	if current == nil || current.UserID == 0 {
		return nil, luoguPluginError(http.StatusUnauthorized, "UNAUTHENTICATED", "请先登录")
	}
	var rows []model.PluginAuthorization
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND provider = ?", current.UserID, luoguPluginProvider).
		Order("id DESC").Find(&rows).Error; err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "AUTHORIZATION_LIST_FAILED", "加载授权失败")
	}
	items := make([]*pb.PluginAuthorizationInfo, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		item := &pb.PluginAuthorizationInfo{
			Id: uint64(row.ID), Provider: row.Provider, ClientKind: row.ClientKind,
			ClientVersion: row.ClientVersion, LuoguUid: row.LuoguUID,
			RiskVersion: row.RiskVersion, AcceptedAt: row.AcceptedAt.Unix(),
			ExpiresAt: row.ExpiresAt.Unix(), CreatedAt: row.CreatedAt.Unix(), Scope: LuoguPluginScope,
		}
		if row.LastUsedAt != nil {
			item.LastUsedAt = row.LastUsedAt.Unix()
		}
		if row.RevokedAt != nil {
			item.RevokedAt = row.RevokedAt.Unix()
		}
		items = append(items, item)
	}
	return &pb.ListAuthorizationsRes{Authorizations: items}, nil
}

func (s *LuoguPluginService) Revoke(ctx context.Context, req *pb.RevokeReq) (*pb.RevokeRes, error) {
	current := auth.GetCurrentUser(ctx)
	if current == nil || current.UserID == 0 {
		return nil, luoguPluginError(http.StatusUnauthorized, "UNAUTHENTICATED", "请先登录")
	}
	if req == nil || (!req.All && req.AuthorizationId == 0) || (req.All && req.AuthorizationId != 0) {
		return nil, luoguPluginError(http.StatusBadRequest, "INVALID_REQUEST", "撤销参数无效")
	}
	now := s.now().UTC()
	var revoked []model.PluginAuthorization
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND provider = ? AND revoked_at IS NULL", current.UserID, luoguPluginProvider)
		if !req.All {
			query = query.Where("id = ?", req.AuthorizationId)
		}
		if err := query.Find(&revoked).Error; err != nil {
			return err
		}
		if len(revoked) == 0 {
			return nil
		}
		if err := tx.Model(&model.PluginAuthorization{}).
			Where("id IN ?", pluginAuthorizationIDs(revoked)).
			Update("revoked_at", now).Error; err != nil {
			return err
		}
		pipe := s.rdb.Pipeline()
		for i := range revoked {
			ttl := revoked[i].ExpiresAt.Sub(now)
			if ttl < luoguRevokedMinimumTTL {
				ttl = luoguRevokedMinimumTTL
			}
			pipe.Set(ctx, luoguPluginRevokedKey(uint64(revoked[i].ID)), now.Unix(), ttl)
		}
		_, err := pipe.Exec(ctx)
		return err
	})
	if err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "AUTHORIZATION_REVOKE_FAILED", "撤销授权失败")
	}
	return &pb.RevokeRes{RevokedCount: uint32(len(revoked))}, nil
}

func pluginAuthorizationIDs(rows []model.PluginAuthorization) []uint {
	ids := make([]uint, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	return ids
}

func (s *LuoguPluginService) ValidateLuoguPluginToken(ctx context.Context, req *pb.ValidateLuoguPluginTokenReq) (*pb.ValidateLuoguPluginTokenRes, error) {
	if req == nil || strings.TrimSpace(req.Token) == "" {
		return nil, luoguPluginError(http.StatusUnauthorized, "GOALGO_CONNECT_REQUIRED", "设备授权无效")
	}
	if req.Scope != LuoguPluginScope {
		return nil, luoguPluginError(http.StatusForbidden, "INVALID_SCOPE", "设备授权范围无效")
	}
	var row model.PluginAuthorization
	if err := s.db.WithContext(ctx).Where("token_hash = ? AND provider = ?", hashLuoguPluginToken(req.Token), luoguPluginProvider).First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, luoguPluginError(http.StatusUnauthorized, "GOALGO_CONNECT_REQUIRED", "设备授权无效")
		}
		return nil, luoguPluginError(http.StatusInternalServerError, "TOKEN_VALIDATE_FAILED", "设备授权校验失败")
	}
	if row.RevokedAt != nil {
		return nil, luoguPluginError(http.StatusUnauthorized, "TOKEN_REVOKED", "设备授权已撤销")
	}
	revoked, err := s.rdb.Exists(ctx, luoguPluginRevokedKey(uint64(row.ID))).Result()
	if err != nil {
		return nil, luoguPluginError(http.StatusServiceUnavailable, "TOKEN_VALIDATE_FAILED", "设备授权校验失败")
	}
	if revoked > 0 {
		return nil, luoguPluginError(http.StatusUnauthorized, "TOKEN_REVOKED", "设备授权已撤销")
	}
	now := s.now().UTC()
	if !now.Before(row.ExpiresAt) {
		return nil, luoguPluginError(http.StatusUnauthorized, "TOKEN_EXPIRED", "设备授权已过期")
	}
	if row.RiskVersion != LuoguPluginRiskVersion {
		return nil, luoguPluginError(http.StatusForbidden, "RISK_REACCEPT_REQUIRED", "风险协议已更新，请重新授权")
	}
	if err := s.db.WithContext(ctx).Model(&row).Update("last_used_at", now).Error; err != nil {
		return nil, luoguPluginError(http.StatusInternalServerError, "TOKEN_VALIDATE_FAILED", "设备授权校验失败")
	}
	return &pb.ValidateLuoguPluginTokenRes{
		AuthorizationId: uint64(row.ID), UserId: uint64(row.UserID), LuoguUid: row.LuoguUID,
		ClientKind: row.ClientKind, ClientVersion: row.ClientVersion,
		RiskVersion: row.RiskVersion, ExpiresAt: row.ExpiresAt.Unix(), Scope: LuoguPluginScope,
	}, nil
}
