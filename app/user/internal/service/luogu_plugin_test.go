package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "cwxu-algo/api/user/v1/plugin"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/alicebob/miniredis/v2"
	kerrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const testLuoguVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"

func newLuoguPluginTestService(t *testing.T) (*LuoguPluginService, *gorm.DB, *miniredis.Miniredis) {
	t.Helper()
	dsn := "file:luogu_plugin_" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.PluginAuthorization{}); err != nil {
		t.Fatalf("migrate plugin authorizations: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewLuoguPluginService(&data.Data{DB: db, RDB: rdb}), db, mr
}

func luoguChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func luoguAuthorizeReq() *pb.AuthorizeCodeReq {
	return &pb.AuthorizeCodeReq{
		LuoguUid:            "2245873",
		ClientKind:          "userscript",
		ClientVersion:       "0.1.0",
		CodeChallenge:       luoguChallenge(testLuoguVerifier),
		CodeChallengeMethod: "S256",
		State:               "state_0123456789abcdef",
		RiskAccepted:        true,
		RiskVersion:         LuoguPluginRiskVersion,
		Scope:               LuoguPluginScope,
	}
}

func luoguUserContext(t *testing.T, userID uint) context.Context {
	t.Helper()
	return ctxWithBearer(adminBlogImageToken(t, userID, false))
}

func luoguErrorReason(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	return kerrors.FromError(err).Reason
}

func issueLuoguCode(t *testing.T, svc *LuoguPluginService, userID uint) *pb.AuthorizeCodeRes {
	t.Helper()
	res, err := svc.AuthorizeCode(luoguUserContext(t, userID), luoguAuthorizeReq())
	if err != nil {
		t.Fatalf("AuthorizeCode: %v", err)
	}
	return res
}

func exchangeLuoguCode(t *testing.T, svc *LuoguPluginService, code *pb.AuthorizeCodeRes) *pb.TokenRes {
	t.Helper()
	res, err := svc.Token(context.Background(), &pb.TokenReq{
		Code: code.Code, Verifier: testLuoguVerifier, State: code.State, Scope: LuoguPluginScope,
	})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	return res
}

func TestLuoguPluginAuthorizationRequiresCurrentRiskAcceptance(t *testing.T) {
	svc, _, _ := newLuoguPluginTestService(t)
	ctx := luoguUserContext(t, 41)

	unchecked := luoguAuthorizeReq()
	unchecked.RiskAccepted = false
	if _, err := svc.AuthorizeCode(ctx, unchecked); luoguErrorReason(t, err) != "RISK_ACCEPTANCE_REQUIRED" {
		t.Fatalf("unchecked risk reason = %s", kerrors.FromError(err).Reason)
	}

	old := luoguAuthorizeReq()
	old.RiskVersion = "2026-08-27-v1"
	if _, err := svc.AuthorizeCode(ctx, old); luoguErrorReason(t, err) != "RISK_VERSION_MISMATCH" {
		t.Fatalf("old risk reason = %s", kerrors.FromError(err).Reason)
	}
}

func TestLuoguPluginAuthorizationCodeHasTwoMinuteTTLAndExpires(t *testing.T) {
	svc, _, mr := newLuoguPluginTestService(t)
	code := issueLuoguCode(t, svc, 42)
	if code.Code == "" {
		t.Fatal("authorization code is empty")
	}
	if code.State != luoguAuthorizeReq().State {
		t.Fatal("authorization response state mismatch")
	}
	if code.Scope != LuoguPluginScope {
		t.Fatalf("authorization response scope = %q, want %q", code.Scope, LuoguPluginScope)
	}
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("redis keys = %v, want one authorization code", keys)
	}
	if ttl := mr.TTL(keys[0]); ttl != 2*time.Minute {
		t.Fatalf("authorization code TTL = %v, want 2m", ttl)
	}
	mr.FastForward(2*time.Minute + time.Second)
	_, err := svc.Token(context.Background(), &pb.TokenReq{
		Code: code.Code, Verifier: testLuoguVerifier, State: code.State, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "AUTHORIZATION_CODE_INVALID" {
		t.Fatalf("expired code reason = %s", reason)
	}
}

func TestLuoguPluginAuthorizationCodeRejectsRiskVersionChangedBeforeExchange(t *testing.T) {
	svc, _, _ := newLuoguPluginTestService(t)
	code := issueLuoguCode(t, svc, 46)
	key := luoguAuthorizationCodeKey(code.Code)
	raw, err := svc.rdb.Get(context.Background(), key).Bytes()
	if err != nil {
		t.Fatalf("load authorization grant: %v", err)
	}
	var grant luoguAuthorizationCode
	if err := json.Unmarshal(raw, &grant); err != nil {
		t.Fatalf("decode authorization grant: %v", err)
	}
	grant.RiskVersion = "2026-08-27-v1"
	raw, err = json.Marshal(grant)
	if err != nil {
		t.Fatalf("encode authorization grant: %v", err)
	}
	if err := svc.rdb.Set(context.Background(), key, raw, time.Minute).Err(); err != nil {
		t.Fatalf("store authorization grant: %v", err)
	}

	_, err = svc.Token(context.Background(), &pb.TokenReq{
		Code: code.Code, Verifier: testLuoguVerifier, State: code.State, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "RISK_REACCEPT_REQUIRED" {
		t.Fatalf("changed risk version reason = %s", reason)
	}
}

func TestLuoguPluginAuthorizationCodeValidatesStatePKCES256AndIsSingleUse(t *testing.T) {
	svc, _, _ := newLuoguPluginTestService(t)

	wrongState := issueLuoguCode(t, svc, 43)
	_, err := svc.Token(context.Background(), &pb.TokenReq{
		Code: wrongState.Code, Verifier: testLuoguVerifier, State: "wrong_state", Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "AUTHORIZATION_STATE_MISMATCH" {
		t.Fatalf("wrong state reason = %s", reason)
	}
	_, err = svc.Token(context.Background(), &pb.TokenReq{
		Code: wrongState.Code, Verifier: testLuoguVerifier, State: wrongState.State, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "AUTHORIZATION_CODE_INVALID" {
		t.Fatalf("consumed state-mismatch code reason = %s", reason)
	}

	wrongVerifier := issueLuoguCode(t, svc, 43)
	_, err = svc.Token(context.Background(), &pb.TokenReq{
		Code: wrongVerifier.Code, Verifier: strings.Repeat("x", 64), State: wrongVerifier.State, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "PKCE_VERIFICATION_FAILED" {
		t.Fatalf("wrong verifier reason = %s", reason)
	}

	valid := issueLuoguCode(t, svc, 43)
	token := exchangeLuoguCode(t, svc, valid)
	if token.DeviceToken == "" {
		t.Fatal("device token is empty")
	}
	_, err = svc.Token(context.Background(), &pb.TokenReq{
		Code: valid.Code, Verifier: testLuoguVerifier, State: valid.State, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "AUTHORIZATION_CODE_INVALID" {
		t.Fatalf("reused code reason = %s", reason)
	}
}

func TestLuoguPluginDeviceTokenStoresOnlyHashExpiresInNinetyDaysAndValidatesScope(t *testing.T) {
	svc, db, _ := newLuoguPluginTestService(t)
	issuedAt := time.Now()
	token := exchangeLuoguCode(t, svc, issueLuoguCode(t, svc, 44))

	var row model.PluginAuthorization
	if err := db.First(&row, uint(token.AuthorizationId)).Error; err != nil {
		t.Fatalf("load authorization: %v", err)
	}
	wantHash := hashLuoguPluginToken(token.DeviceToken)
	if row.TokenHash != wantHash || !strings.HasPrefix(row.TokenHash, "sha256:") {
		t.Fatalf("stored token hash = %q, want %q", row.TokenHash, wantHash)
	}
	if strings.Contains(row.TokenHash, token.DeviceToken) {
		t.Fatal("stored token hash contains plaintext device token")
	}
	if delta := row.ExpiresAt.Sub(issuedAt); delta < 90*24*time.Hour-time.Minute || delta > 90*24*time.Hour+time.Minute {
		t.Fatalf("device expiry delta = %v, want 90d", delta)
	}

	validated, err := svc.ValidateLuoguPluginToken(context.Background(), &pb.ValidateLuoguPluginTokenReq{
		Token: token.DeviceToken, Scope: LuoguPluginScope,
	})
	if err != nil {
		t.Fatalf("ValidateLuoguPluginToken: %v", err)
	}
	if validated.AuthorizationId != token.AuthorizationId || validated.UserId != 44 || validated.Scope != LuoguPluginScope {
		t.Fatalf("unexpected validation response: %+v", validated)
	}

	_, err = svc.ValidateLuoguPluginToken(context.Background(), &pb.ValidateLuoguPluginTokenReq{
		Token: token.DeviceToken, Scope: "profile.read",
	})
	if reason := luoguErrorReason(t, err); reason != "INVALID_SCOPE" {
		t.Fatalf("wrong scope reason = %s", reason)
	}

	if err := db.Model(&row).Update("risk_version", "2026-08-27-v1").Error; err != nil {
		t.Fatal(err)
	}
	_, err = svc.ValidateLuoguPluginToken(context.Background(), &pb.ValidateLuoguPluginTokenReq{
		Token: token.DeviceToken, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "RISK_REACCEPT_REQUIRED" {
		t.Fatalf("old device risk reason = %s", reason)
	}

	if err := db.Model(&row).Updates(map[string]interface{}{
		"risk_version": LuoguPluginRiskVersion,
		"expires_at":   time.Now().Add(-time.Second),
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = svc.ValidateLuoguPluginToken(context.Background(), &pb.ValidateLuoguPluginTokenReq{
		Token: token.DeviceToken, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "TOKEN_EXPIRED" {
		t.Fatalf("expired token reason = %s", reason)
	}
}

func TestLuoguPluginRevokesOneOrAllAndWritesSharedRedisMarkers(t *testing.T) {
	svc, db, mr := newLuoguPluginTestService(t)
	tokens := make([]*pb.TokenRes, 0, 3)
	for i := 0; i < 3; i++ {
		tokens = append(tokens, exchangeLuoguCode(t, svc, issueLuoguCode(t, svc, 45)))
	}
	ctx := luoguUserContext(t, 45)
	if err := db.Model(&model.PluginAuthorization{}).
		Where("id = ?", tokens[0].AuthorizationId).
		Update("expires_at", time.Now().Add(time.Minute)).Error; err != nil {
		t.Fatalf("shorten token expiry: %v", err)
	}

	one, err := svc.Revoke(ctx, &pb.RevokeReq{AuthorizationId: tokens[0].AuthorizationId})
	if err != nil || one.RevokedCount != 1 {
		t.Fatalf("revoke one: res=%+v err=%v", one, err)
	}
	if !mr.Exists(luoguPluginRevokedKey(tokens[0].AuthorizationId)) {
		t.Fatal("single revoke marker missing")
	}
	if ttl := mr.TTL(luoguPluginRevokedKey(tokens[0].AuthorizationId)); ttl != 30*time.Minute {
		t.Fatalf("short-lived token revoke marker TTL = %v, want 30m", ttl)
	}
	var row model.PluginAuthorization
	if err := db.First(&row, uint(tokens[0].AuthorizationId)).Error; err != nil || row.RevokedAt == nil {
		t.Fatalf("single authorization not revoked: row=%+v err=%v", row, err)
	}
	_, err = svc.ValidateLuoguPluginToken(context.Background(), &pb.ValidateLuoguPluginTokenReq{
		Token: tokens[0].DeviceToken, Scope: LuoguPluginScope,
	})
	if reason := luoguErrorReason(t, err); reason != "TOKEN_REVOKED" {
		t.Fatalf("revoked token reason = %s", reason)
	}

	all, err := svc.Revoke(ctx, &pb.RevokeReq{All: true})
	if err != nil || all.RevokedCount != 2 {
		t.Fatalf("revoke all: res=%+v err=%v", all, err)
	}
	for _, token := range tokens {
		if !mr.Exists(luoguPluginRevokedKey(token.AuthorizationId)) {
			t.Fatalf("revoke marker missing for %d", token.AuthorizationId)
		}
	}
}

func TestLuoguPluginAuthorizationModelHasOnlySecurityReviewedFields(t *testing.T) {
	typ := reflect.TypeOf(model.PluginAuthorization{})
	want := []string{
		"ID", "UserID", "Provider", "ClientKind", "ClientVersion", "LuoguUID",
		"TokenHash", "RiskVersion", "AcceptedAt", "ExpiresAt", "LastUsedAt",
		"RevokedAt", "CreatedAt", "UpdatedAt",
	}
	if typ.NumField() != len(want) {
		t.Fatalf("field count = %d, want %d", typ.NumField(), len(want))
	}
	for i, field := range want {
		if got := typ.Field(i).Name; got != field {
			t.Fatalf("field %d = %s, want %s", i, got, field)
		}
	}
}
