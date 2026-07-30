package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func adminBlogImageServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:service_admin_blog_images_" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&model.User{},
		&model.BlogImageAsset{},
		&model.BlogArticle{},
		&model.BlogPage{},
		&model.BlogSiteConfig{},
		&model.SiteConfig{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func adminBlogImageToken(t *testing.T, userID uint, isSiteAdmin bool) string {
	t.Helper()
	if err := _const.ConfigureJWTSecret("admin-blog-image-test-secret-32chars"); err != nil {
		t.Fatal(err)
	}
	claims := auth.JwtPayload{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		UserID:      userID,
		Username:    "tester",
		IsSiteAdmin: isSiteAdmin,
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(_const.JWTSecret()))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func adminBlogImageRequest(server *khttp.Server, method, path, token string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func TestAdminBlogImagesRequiresSiteAdmin(t *testing.T) {
	db := adminBlogImageServiceDB(t)
	server := khttp.NewServer()
	RegisterBlogRoutes(server, &BlogService{db: db})

	unauthenticated := adminBlogImageRequest(server, http.MethodGet, "/v1/user/blog/admin/images?mode=all", "", "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}

	regular := adminBlogImageRequest(server, http.MethodGet, "/v1/user/blog/admin/images?mode=all", adminBlogImageToken(t, 2, false), "")
	if regular.Code != http.StatusForbidden {
		t.Fatalf("regular status=%d body=%s", regular.Code, regular.Body.String())
	}

	admin := adminBlogImageRequest(server, http.MethodGet, "/v1/user/blog/admin/images?mode=all", adminBlogImageToken(t, 1, true), "")
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", admin.Code, admin.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(admin.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != float64(0) {
		t.Fatalf("admin payload=%v", payload)
	}
}

func TestAdminBlogImageRoutesReplaceAuthorCleanupRoutes(t *testing.T) {
	server := khttp.NewServer()
	RegisterBlogRoutes(server, &BlogService{})
	routes := map[string]bool{}
	if err := server.WalkHandle(func(method, path string, _ http.HandlerFunc) {
		routes[method+" "+path] = true
	}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{
		"GET /v1/user/blog/admin/images",
		"POST /v1/user/blog/admin/images/delete",
		"POST /v1/user/blog/admin/images/delete-batch",
	} {
		if !routes[route] {
			t.Fatalf("missing route %s", route)
		}
	}
	for _, route := range []string{
		"POST /v1/user/blog/images/orphans",
		"POST /v1/user/blog/images/gc",
	} {
		if routes[route] {
			t.Fatalf("legacy author cleanup route remains: %s", route)
		}
	}
}

func TestAdminBlogImageDeleteAuthAndConflictMapping(t *testing.T) {
	db := adminBlogImageServiceDB(t)
	if err := db.Create(&model.SiteConfig{
		ID: 1, UpyunBucket: "test", UpyunOperator: "operator",
		UpyunPassword: "password", UpyunDomain: "cdn.example.com", UpyunScheme: "https",
	}).Error; err != nil {
		t.Fatal(err)
	}
	asset := model.BlogImageAsset{
		UserID: 2, ObjectKey: "/blog/2/fresh.webp", URL: "/blog/2/fresh.webp",
		Status: model.BlogImageAssetReady,
	}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	server := khttp.NewServer()
	RegisterBlogRoutes(server, &BlogService{db: db})

	for _, path := range []string{
		"/v1/user/blog/admin/images/delete",
		"/v1/user/blog/admin/images/delete-batch",
	} {
		regular := adminBlogImageRequest(
			server, http.MethodPost, path, adminBlogImageToken(t, 2, false), `{"id":1,"ids":[1],"snapshot":"bad"}`,
		)
		if regular.Code != http.StatusForbidden {
			t.Fatalf("regular %s status=%d body=%s", path, regular.Code, regular.Body.String())
		}
	}

	adminToken := adminBlogImageToken(t, 1, true)
	single := adminBlogImageRequest(
		server, http.MethodPost, "/v1/user/blog/admin/images/delete", adminToken,
		`{"id":`+strconv.FormatUint(uint64(asset.ID), 10)+`}`,
	)
	if single.Code != http.StatusConflict {
		t.Fatalf("single status=%d body=%s", single.Code, single.Body.String())
	}
	batch := adminBlogImageRequest(
		server, http.MethodPost, "/v1/user/blog/admin/images/delete-batch", adminToken,
		`{"ids":[`+strconv.FormatUint(uint64(asset.ID), 10)+`],"snapshot":"stale"}`,
	)
	if batch.Code != http.StatusConflict {
		t.Fatalf("batch status=%d body=%s", batch.Code, batch.Body.String())
	}
}
