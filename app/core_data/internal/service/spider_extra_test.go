package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cwxu-algo/api/core/v1/spider"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

// genTestRSAKeys 生成测试用 RSA 密钥对（PEM）。
func genTestRSAKeys(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privPEM, pubPEM
}

func TestTogglePlatformDispatchesModuleAndReportsRedisFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	server := khttp.NewServer()
	spider.RegisterSpiderHTTPServer(server, &SpiderService{rdb: rdb})
	admin := spiderExtraAdminToken(t, 1, true)

	var r = spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/toggle-platform", admin,
		`{"platform":"LuoGu","enabled":true,"module":"submit"}`)
	var body map[string]interface{}

	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("luogu no-account toggle invalid json: %v", err)
	}
	if body["code"] != "1" || task.IsPlatformPaused(rdb, "LuoGu") {
		t.Fatalf("洛谷无账号时不应允许开启提交同步，body=%s", r.Body.String())
	}

	r = spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/toggle-platform", admin,
		`{"platform":"NowCoder","enabled":false,"module":"contest"}`)
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid module json: %v", err)
	}
	if body["code"] != "1" {
		t.Fatalf("非法 module 应拒绝，body=%s", r.Body.String())
	}

	unavailable := khttp.NewServer()
	spider.RegisterSpiderHTTPServer(unavailable, &SpiderService{})
	r = spiderExtraRequest(unavailable, http.MethodPost, "/v1/core/spider/toggle-platform", admin,
		`{"platform":"NowCoder","enabled":false}`)
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("unavailable redis json: %v", err)
	}
	if body["code"] != "1" {
		t.Fatalf("Redis 不可用不能误报成功，body=%s", r.Body.String())
	}
}

// spiderExtraAdminToken 签发站管 JWT（IsSiteAdmin=true 绕过细粒度权限位图）。
func spiderExtraAdminToken(t *testing.T, userID uint, isSiteAdmin bool) string {
	t.Helper()
	privPEM, pubPEM := genTestRSAKeys(t)
	if err := _const.ConfigureJWTKeys(privPEM, pubPEM); err != nil {
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
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(_const.JWTPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// spiderExtraServer 用 proto 注册的 khttp server 挂载 SpiderService，
// 配 miniredis 支撑限流（rdb 为 nil 时限流 fail-closed，无法测到业务分支）。
func spiderExtraServer(t *testing.T) *khttp.Server {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	server := khttp.NewServer()
	spider.RegisterSpiderHTTPServer(server, &SpiderService{rdb: rdb})
	return server
}

// spiderExtraRequest 通过 proto 注册的 server 发起真实 HTTP 请求。
func spiderExtraRequest(server *khttp.Server, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

// TestSpiderExtraRoutesRegisteredByProto 验证两个 extra 路径由 proto 注册可达，
// 且未授权时返回手写同款 JSON 形状（HTTP 200 + code/success 标记，不返回 Kratos error）。
func TestSpiderExtraRoutesRegisteredByProto(t *testing.T) {
	server := spiderExtraServer(t)
	admin := spiderExtraAdminToken(t, 1, true)

	// update-platform：未授权 → code=1，形状与手写一致（count 为 0）
	r := spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/update-platform", "", `{"platform":"LeetCode"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("update-platform no-auth status=%d body=%s", r.Code, r.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("update-platform no-auth invalid json: %v body=%s", err, r.Body.String())
	}
	if body["code"].(float64) != 1 || body["message"] != "仅站点管理员可操作" {
		t.Fatalf("update-platform no-auth unexpected body=%s", r.Body.String())
	}

	// repair-contest-cells：未授权 → success=false
	r = spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/repair-contest-cells", "", `{}`)
	if r.Code != http.StatusOK {
		t.Fatalf("repair no-auth status=%d body=%s", r.Code, r.Body.String())
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("repair no-auth invalid json: %v body=%s", err, r.Body.String())
	}
	if body["success"].(bool) || body["message"] != "仅管理员可操作" {
		t.Fatalf("repair no-auth unexpected body=%s", r.Body.String())
	}

	// 站管 + nil spider/db：update-platform 成功，code=0 且平台已规范化（力扣 → LeetCode）
	r = spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/update-platform", admin, `{"platform":"力扣"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("update-platform admin status=%d body=%s", r.Code, r.Body.String())
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("update-platform admin invalid json: %v body=%s", err, r.Body.String())
	}
	if body["code"].(float64) != 0 || body["platform"] != "LeetCode" {
		t.Fatalf("update-platform admin unexpected body=%s", r.Body.String())
	}

	// 同一窗口内第二次请求命中限流（SETNX 语义，与手写 429 分支一致）
	r = spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/update-platform", admin, `{"platform":"cf"}`)
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("update-platform ratelimit invalid json: %v body=%s", err, r.Body.String())
	}
	if body["code"].(float64) != 1 || body["message"] != "请求过于频繁，请稍后再试" {
		t.Fatalf("update-platform ratelimit unexpected body=%s", r.Body.String())
	}

	// 站管 + nil db：repair 成功，data 为空 map（幂等，无 DB 也正常返回）
	r = spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/repair-contest-cells", admin, `{}`)
	if r.Code != http.StatusOK {
		t.Fatalf("repair admin status=%d body=%s", r.Code, r.Body.String())
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("repair admin invalid json: %v body=%s", err, r.Body.String())
	}
	if !body["success"].(bool) || body["message"] != "ok" {
		t.Fatalf("repair admin unexpected body=%s", r.Body.String())
	}
}

// TestSpiderUpdatePlatformParamValidation 参数校验分支：缺少 platform / 不支持的平台。
// 每个用例独立 server（独立 redis），避免 2 分钟限流窗口互相干扰。
func TestSpiderUpdatePlatformParamValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing platform", `{}`, "缺少 platform"},
		{"unsupported platform", `{"platform":"foo"}`, "不支持的平台: foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := spiderExtraServer(t)
			admin := spiderExtraAdminToken(t, 1, true)
			r := spiderExtraRequest(server, http.MethodPost, "/v1/core/spider/update-platform", admin, tc.body)
			if r.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			var body map[string]interface{}
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid json: %v body=%s", err, r.Body.String())
			}
			if body["code"].(float64) != 1 || body["message"] != tc.want {
				t.Fatalf("unexpected body=%s", r.Body.String())
			}
		})
	}
}
