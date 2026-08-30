package service

import (
	"context"
	"strings"
	"testing"
	"time"

	spiderpb "cwxu-algo/api/core/v1/spider"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func coreTokenWithPermissions(t *testing.T, userID uint, perms ...string) string {
	t.Helper()
	privPEM, pubPEM := genTestRSAKeys(t)
	if err := _const.ConfigureJWTKeys(privPEM, pubPEM); err != nil {
		t.Fatal(err)
	}
	claims := auth.JwtPayload{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, UserID: userID, Pm: rbac.Encode(perms)}
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(_const.JWTPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAdminClientSyncAuditPostgresKeywordUsesILike(t *testing.T) {
	condition := clientSyncAuditKeywordCondition("postgres")
	for _, column := range []string{"oj_uid ILIKE", "client_version ILIKE", "CAST(user_id AS TEXT) ILIKE"} {
		if !strings.Contains(condition, column) {
			t.Fatalf("condition %q missing %q", condition, column)
		}
	}
}

func TestAdminListClientSyncAuditsRequiresPermissionAndFiltersBeforePagination(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []model.ClientSyncAudit{
		{SessionID: "a", AuthorizationID: 1, UserID: 42, Platform: "luogu", OJUID: "998877", ClientKind: "userscript", ClientVersion: "0.2.7-beta", Status: "completed", StartedAt: now, UpdatedAt: now},
		{SessionID: "b", AuthorizationID: 2, UserID: 43, Platform: "luogu", OJUID: "112233", ClientKind: "userscript", ClientVersion: "0.1.0", Status: "failed", StartedAt: now.Add(-time.Hour), UpdatedAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO users (id, username, name) VALUES (42, 'user42', 'User 42'), (43, 'user43', 'User 43')").Error; err != nil {
		t.Fatal(err)
	}
	svc := &SpiderService{db: db}
	if _, err := svc.AdminListClientSyncAudits(context.Background(), &spiderpb.AdminListClientSyncAuditsReq{}); errors.Reason(err) != "SYNC_AUDIT_PERMISSION_DENIED" {
		t.Fatalf("ordinary reason = %s", errors.Reason(err))
	}
	adminCtx := luoguHeaderContext("Authorization", "Bearer "+coreSiteAdminToken(t, 10))
	if _, err := svc.AdminListClientSyncAudits(adminCtx, &spiderpb.AdminListClientSyncAuditsReq{PageNum: 1, PageSize: 1}); err != nil {
		t.Fatalf("site admin bypass: %v", err)
	}
	ctx := luoguHeaderContext("Authorization", "Bearer "+coreTokenWithPermissions(t, 9, rbac.PermSiteUserSync))
	res, err := svc.AdminListClientSyncAudits(ctx, &spiderpb.AdminListClientSyncAuditsReq{PageNum: 1, PageSize: 1, Keyword: "0.2.7", Platform: "luogu", Status: "completed", From: now.Add(-time.Minute).Unix(), To: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.List) != 1 || res.List[0].UserId != 42 || res.List[0].SessionId != "a" {
		t.Fatalf("unexpected page: %+v", res)
	}
	if res.List[0].Platform != "luogu" {
		t.Fatalf("response platform = %q, want luogu", res.List[0].Platform)
	}
}

func TestAdminListClientSyncAuditsReturnsUsername(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, name TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Create(&model.ClientSyncAudit{SessionID: "username-session", UserID: 42, Username: "alice", Platform: "luogu", OJUID: "1", ClientKind: "userscript", ClientVersion: "0.1.6", Status: "completed", StartedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &SpiderService{db: db}
	ctx := luoguHeaderContext("Authorization", "Bearer "+coreTokenWithPermissions(t, 9, rbac.PermSiteUserSync))
	res, err := svc.AdminListClientSyncAudits(ctx, &spiderpb.AdminListClientSyncAuditsReq{})
	if err != nil || len(res.List) != 1 || res.List[0].Username != "alice" {
		t.Fatalf("username = %+v, err=%v", res, err)
	}
}

func TestAdminListClientSyncAuditsNormalizesLuoguAliasAndRejectsInvalidPlatform(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, name TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Create(&model.ClientSyncAudit{SessionID: "platform-session", UserID: 42, Platform: "luogu", OJUID: "1", ClientKind: "userscript", ClientVersion: "1", Status: "completed", StartedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &SpiderService{db: db}
	ctx := luoguHeaderContext("Authorization", "Bearer "+coreTokenWithPermissions(t, 9, rbac.PermSiteUserSync))
	res, err := svc.AdminListClientSyncAudits(ctx, &spiderpb.AdminListClientSyncAuditsReq{Platform: "LuoGu"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.List) != 1 || res.List[0].Platform != "luogu" {
		t.Fatalf("normalized platform result = %+v", res)
	}
	if _, err := svc.AdminListClientSyncAudits(ctx, &spiderpb.AdminListClientSyncAuditsReq{Platform: "qoj"}); errors.Reason(err) != "INVALID_PLATFORM" {
		t.Fatalf("invalid platform reason = %s", errors.Reason(err))
	}
}

func coreSiteAdminToken(t *testing.T, userID uint) string {
	t.Helper()
	privPEM, pubPEM := genTestRSAKeys(t)
	if err := _const.ConfigureJWTKeys(privPEM, pubPEM); err != nil {
		t.Fatal(err)
	}
	claims := auth.JwtPayload{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, UserID: userID, IsSiteAdmin: true}
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(_const.JWTPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	return token
}
