package jwt

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	config "github.com/go-kratos/gateway/api/gateway/config/v1"
	jwtv1 "github.com/go-kratos/gateway/api/gateway/middleware/jwt/v1"
	"google.golang.org/protobuf/types/known/anypb"
)

func middlewareConfig(t *testing.T, value string) *config.Middleware {
	t.Helper()
	options, err := anypb.New(&jwtv1.JWT{Secret: value})
	if err != nil {
		t.Fatal(err)
	}
	return &config.Middleware{Name: "jwt", Options: options}
}

// testPubKeyPEM 生成测试 RSA 公钥 PEM。
func testPubKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

// 有效 RSA 公钥 PEM 可从配置读取（env 空时 fallback options.secret）
func TestMiddlewareReadsSecretFromConfig(t *testing.T) {
	t.Setenv("CWXU_JWT_PUBLIC_KEY", "")
	if _, err := Middleware(middlewareConfig(t, testPubKeyPEM(t))); err != nil {
		t.Fatal(err)
	}
}

// 非 PEM / 非公钥 / 空值必须被拒绝（fail closed）
func TestMiddlewareRejectsInvalidConfigSecret(t *testing.T) {
	t.Setenv("CWXU_JWT_PUBLIC_KEY", "")
	if _, err := Middleware(middlewareConfig(t, strings.Repeat("c", 32))); err == nil {
		t.Fatal("expected non-PEM config secret to be rejected")
	}
	if _, err := Middleware(middlewareConfig(t, "")); err == nil {
		t.Fatal("expected empty config secret to be rejected")
	}
	// 私钥 PEM 不是公钥 → 拒绝
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))
	if _, err := Middleware(middlewareConfig(t, privPEM)); err == nil {
		t.Fatal("expected private-key PEM to be rejected")
	}
}

func TestBlogPagePublicReadsAreWhitelisted(t *testing.T) {
	for _, path := range []string{
		"/v1/user/blog/page/list",
		"/api/user/blog/page/list",
		"/v1/user/blog/page/get",
		"/api/user/blog/page/get",
		"/v1/user/blog/obsidian-plugin/latest",
		"/api/user/blog/obsidian-plugin/latest",
		"/v1/user/blog/obsidian-plugin/publish",
		"/api/user/blog/obsidian-plugin/publish",
	} {
		if _, ok := publicExact[path]; !ok {
			t.Errorf("public blog page path missing from JWT whitelist: %s", path)
		}
	}
}

// TestPaymentNotifyIsWhitelisted 支付FM回调免 JWT（验签在 user 服务内完成）
func TestPaymentNotifyIsWhitelisted(t *testing.T) {
	for _, path := range []string{
		"/v1/payment/notify",
		"/api/payment/notify",
	} {
		if _, ok := publicExact[path]; !ok {
			t.Errorf("payment notify path missing from JWT whitelist: %s", path)
		}
	}
}

// TestSupportEventsIsWhitelisted 客户中心 webhook 回调免 JWT（HMAC 验签在 user 服务内完成）
func TestSupportEventsIsWhitelisted(t *testing.T) {
	for _, path := range []string{
		"/v1/support/events",
		"/api/support/events",
	} {
		if _, ok := publicExact[path]; !ok {
			t.Errorf("support events path missing from JWT whitelist: %s", path)
		}
	}
}
