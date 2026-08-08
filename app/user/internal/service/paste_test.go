package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "cwxu-algo/api/user/v1/paste"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupPasteTest(t *testing.T) (*PasteService, *gorm.DB) {
	t.Helper()
	if err := _const.ConfigureJWTSecret("paste-test-jwt-secret-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:paste_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Paste{}); err != nil {
		t.Fatal(err)
	}
	// admin-list LEFT JOIN users；测试里建一张最小 users 表
	if err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, name TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	return &PasteService{db: db}, db
}

func pasteTestToken(t *testing.T, uid uint, isAdmin bool) string {
	t.Helper()
	pd := auth.JwtPayload{UserID: uid, IsSiteAdmin: isAdmin}
	pd.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, pd)
	s, err := tok.SignedString([]byte(_const.JWTSecret()))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pasteTestRequest(t *testing.T, srv *khttp.Server, method, path, token, body string, query map[string]string) (int, map[string]interface{}) {
	t.Helper()
	rawURL := path
	if len(query) > 0 {
		parts := make([]string, 0, len(query))
		for k, v := range query {
			parts = append(parts, k+"="+v)
		}
		rawURL += "?" + strings.Join(parts, "&")
	}
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, rawURL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, rawURL, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	m := map[string]interface{}{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
	}
	return rec.Code, m
}

func TestPasteRoutesAndShape(t *testing.T) {
	svc, _ := setupPasteTest(t)
	srv := khttp.NewServer()
	pastepbRegister(t, srv, svc)

	token := pasteTestToken(t, 7, false)

	// 未登录：create/mine/delete/admin-list 都返回 code=1
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/user/paste/create", `{"content":"x"}`},
		{"GET", "/v1/user/paste/mine", ""},
		{"POST", "/v1/user/paste/delete", `{"slug":"abc"}`},
		{"GET", "/v1/user/paste/admin-list", ""},
	} {
		code, m := pasteTestRequest(t, srv, tc.method, tc.path, "", tc.body, nil)
		if code != 200 {
			t.Fatalf("%s %s: status=%d want 200", tc.method, tc.path, code)
		}
		if m["code"] != float64(1) || m["message"] != "请先登录" {
			t.Fatalf("%s %s: got %v", tc.method, tc.path, m)
		}
	}

	// create：成功形状 {code, message, data:{id,slug,title,language,userId,createdAt,expireAt,content}}
	code, m := pasteTestRequest(t, srv, "POST", "/v1/user/paste/create", token,
		`{"title":"  hello  ","content":"line1\r\nline2","language":"cpp","expire":"never"}`, nil)
	if code != 200 || m["code"] != float64(0) || m["message"] != "success" {
		t.Fatalf("create: status=%d body=%v", code, m)
	}
	if len(m) != 3 || m["data"] == nil {
		t.Fatalf("create: 应只有 code/message/data 三个键，got %v", m)
	}
	data := m["data"].(map[string]interface{})
	if data["title"] != "hello" { // TrimSpace
		t.Fatalf("title=%v want hello", data["title"])
	}
	if data["content"] != "line1\nline2" { // CRLF 归一化
		t.Fatalf("content=%q", data["content"])
	}
	if data["language"] != "cpp" {
		t.Fatalf("language=%v", data["language"])
	}
	if data["userId"] != "7" || data["createdAt"] == "" {
		t.Fatalf("userId=%v createdAt=%v", data["userId"], data["createdAt"])
	}
	if v, ok := data["expireAt"]; !ok || v != nil {
		t.Fatalf("never 过期应输出 expireAt=null，got %v (ok=%v)", v, ok)
	}
	slug, _ := data["slug"].(string)
	if slug == "" {
		t.Fatal("create 应返回 slug")
	}

	// get：公开路径，无需 token；形状与 create 相同
	code, m = pasteTestRequest(t, srv, "GET", "/v1/user/paste/get", "", "", map[string]string{"slug": slug})
	if code != 200 || m["code"] != float64(0) {
		t.Fatalf("get: status=%d body=%v", code, m)
	}
	if m["data"].(map[string]interface{})["content"] != "line1\nline2" {
		t.Fatalf("get content=%v", m["data"])
	}

	// mine：{code,message,list}；不带正文
	code, m = pasteTestRequest(t, srv, "GET", "/v1/user/paste/mine", token, "", map[string]string{"page": "1", "pageSize": "100"})
	if code != 200 || m["code"] != float64(0) {
		t.Fatalf("mine: status=%d body=%v", code, m)
	}
	if len(m) != 3 {
		t.Fatalf("mine 应只有 code/message/list 三个键，got %v", m)
	}
	list := m["list"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("mine list len=%d", len(list))
	}
	item := list[0].(map[string]interface{})
	if item["slug"] != slug || item["content"] != "" {
		t.Fatalf("mine item=%v（不应带正文）", item)
	}

	// admin-list：普通用户无治理权限
	code, m = pasteTestRequest(t, srv, "GET", "/v1/user/paste/admin-list", token, "", map[string]string{"page": "1", "pageSize": "10"})
	if code != 200 || m["code"] != float64(1) || m["message"] != "没有内容治理权限" {
		t.Fatalf("admin-list(普通用户): %v", m)
	}

	// delete：成功后 get 404 语义
	code, m = pasteTestRequest(t, srv, "POST", "/v1/user/paste/delete", token, `{"slug":"`+slug+`"}`, nil)
	if code != 200 || m["code"] != float64(0) || m["message"] != "已删除" {
		t.Fatalf("delete: %v", m)
	}
	if len(m) != 2 {
		t.Fatalf("delete 应只有 code/message 两个键，got %v", m)
	}
	code, m = pasteTestRequest(t, srv, "GET", "/v1/user/paste/get", "", "", map[string]string{"slug": slug})
	if m["message"] != "内容不存在或已删除" {
		t.Fatalf("get after delete: %v", m)
	}

	// create 校验分支
	code, m = pasteTestRequest(t, srv, "POST", "/v1/user/paste/create", token, `{"content":"   "}`, nil)
	if m["message"] != "内容不能为空" {
		t.Fatalf("empty content: %v", m)
	}
	code, m = pasteTestRequest(t, srv, "POST", "/v1/user/paste/create", token, `{"content":"x","expire":"2d"}`, nil)
	if !strings.Contains(m["message"].(string), "有效期无效") {
		t.Fatalf("bad expire: %v", m)
	}
	code, m = pasteTestRequest(t, srv, "POST", "/v1/user/paste/create", token, `{"title":"`+strings.Repeat("长", 300)+`","content":"x"}`, nil)
	if m["message"] != "标题过长" {
		t.Fatalf("long title: %v", m)
	}
}

// pastepbRegister 生成代码注册：5 个路径全部由 proto 注册。
func pastepbRegister(t *testing.T, srv *khttp.Server, svc *PasteService) {
	t.Helper()
	pb.RegisterPasteHTTPServer(srv, svc)
}

func TestPasteAdminListShapeAndPagination(t *testing.T) {
	svc, db := setupPasteTest(t)
	srv := khttp.NewServer()
	pastepbRegister(t, srv, svc)

	now := time.Now()
	// 两条：一条永不过期，一条 1h 后过期；关联用户 7/8
	_ = db.Create(&model.Paste{Slug: "slug-a", Title: "A", Content: "alpha", Language: "text", UserID: 7}).Error
	exp := now.Add(time.Hour)
	_ = db.Create(&model.Paste{Slug: "slug-b", Title: "B", Content: "beta", Language: "go", UserID: 8, ExpireAt: &exp}).Error
	_ = db.Exec(`INSERT INTO users (id, username, name) VALUES (7, 'alice', 'Alice'), (8, 'bob', 'Bob')`).Error

	admin := pasteTestToken(t, 1, true)
	code, m := pasteTestRequest(t, srv, "GET", "/v1/user/paste/admin-list", admin, "", map[string]string{"page": "1", "pageSize": "10"})
	if code != 200 || m["code"] != float64(0) {
		t.Fatalf("admin-list: status=%d body=%v", code, m)
	}
	if len(m) != 6 { // code/message/list/total/page/pageSize
		t.Fatalf("admin-list 键数=%d，got %v", len(m), m)
	}
	if m["total"] != float64(2) || m["page"] != float64(1) || m["pageSize"] != float64(10) {
		t.Fatalf("分页字段: %v", m)
	}
	list := m["list"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("list len=%d", len(list))
	}
	first := list[0].(map[string]interface{}) // id DESC → slug-b 在前
	if first["slug"] != "slug-b" || first["content"] != "beta" ||
		first["username"] != "bob" || first["name"] != "Bob" || first["expireAt"] == nil {
		t.Fatalf("admin row1=%v", first)
	}
	second := list[1].(map[string]interface{})
	if second["slug"] != "slug-a" || second["username"] != "alice" || second["expireAt"] != nil {
		t.Fatalf("admin row2=%v", second)
	}

	// 过期行不在列表里（总数为有效期内）
	past := now.Add(-time.Hour)
	_ = db.Model(&model.Paste{}).Where("slug = ?", "slug-a").Update("expire_at", past).Error
	code, m = pasteTestRequest(t, srv, "GET", "/v1/user/paste/admin-list", admin, "", map[string]string{"page": "1", "pageSize": "10"})
	if m["total"] != float64(1) || len(m["list"].([]interface{})) != 1 {
		t.Fatalf("过期过滤失败: %v", m)
	}
}

func TestPasteExpiredGetDeletesRow(t *testing.T) {
	svc, db := setupPasteTest(t)
	srv := khttp.NewServer()
	pastepbRegister(t, srv, svc)

	exp := time.Now().Add(-time.Minute) // 已过期
	_ = db.Create(&model.Paste{Slug: "stale", Title: "S", Content: "old", Language: "text", UserID: 7, ExpireAt: &exp}).Error

	code, m := pasteTestRequest(t, srv, "GET", "/v1/user/paste/get", "", "", map[string]string{"slug": "stale"})
	if code != 200 || m["code"] != float64(1) || m["message"] != "内容已过期" {
		t.Fatalf("expired get: %v", m)
	}
	var n int64
	_ = db.Model(&model.Paste{}).Where("slug = ?", "stale").Count(&n).Error
	if n != 0 {
		t.Fatalf("过期内容应被硬删，剩余 %d", n)
	}
}
