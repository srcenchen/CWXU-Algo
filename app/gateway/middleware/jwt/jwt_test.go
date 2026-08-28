package jwt

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	config "github.com/go-kratos/gateway/api/gateway/config/v1"
	corsv1 "github.com/go-kratos/gateway/api/gateway/middleware/cors/v1"
	jwtv1 "github.com/go-kratos/gateway/api/gateway/middleware/jwt/v1"
	"github.com/go-kratos/gateway/middleware"
	corsmiddleware "github.com/go-kratos/gateway/middleware/cors"
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

func TestLuoguPluginTokenExchangeIsTheOnlyPublicPluginAuthorizationPath(t *testing.T) {
	for _, publicPath := range []string{
		"/v1/user/plugin/luogu/token",
		"/api/user/plugin/luogu/token",
	} {
		if _, ok := publicExact[publicPath]; !ok {
			t.Errorf("Luogu plugin token path missing from JWT whitelist: %s", publicPath)
		}
	}

	for _, protectedPath := range []string{
		"/v1/user/plugin/luogu/authorize-code",
		"/api/user/plugin/luogu/authorize-code",
		"/v1/user/plugin/luogu/authorizations",
		"/api/user/plugin/luogu/authorizations",
		"/v1/user/plugin/luogu/revoke",
		"/api/user/plugin/luogu/revoke",
	} {
		if _, ok := publicExact[protectedPath]; ok {
			t.Errorf("JWT-protected Luogu plugin path is public: %s", protectedPath)
		}
	}
}

func TestLuoguSyncOnlyExposesExactPluginTokenEndpoints(t *testing.T) {
	for _, publicPath := range []string{
		"/v1/user/plugin/luogu/token",
		"/api/user/plugin/luogu/token",
		"/v1/core/spider/luogu-sync/start",
		"/api/core/spider/luogu-sync/start",
		"/v1/core/spider/luogu-sync/status",
		"/api/core/spider/luogu-sync/status",
		"/v1/core/spider/luogu-sync/page",
		"/api/core/spider/luogu-sync/page",
	} {
		if _, ok := publicExact[publicPath]; !ok {
			t.Errorf("Luogu sync path missing from JWT whitelist: %s", publicPath)
		}
	}

	for _, protectedPath := range []string{
		"/v1/core/spider/luogu-sync",
		"/v1/core/spider/luogu-sync/start/nearby",
		"/v1/core/spider/luogu-sync/status-extra",
		"/api/core/spider/luogu-sync/page/extra",
		"/v1/user/plugin/luogu/authorize-code",
		"/api/user/plugin/luogu/authorizations",
		"/v1/user/plugin/luogu/revoke",
	} {
		if _, ok := publicExact[protectedPath]; ok {
			t.Errorf("JWT-protected Luogu path is public: %s", protectedPath)
		}
	}
}

func TestLuoguSyncPreflightPassesThroughJWTToCORSOnlyForExactPaths(t *testing.T) {
	t.Setenv("CWXU_JWT_PUBLIC_KEY", "")
	extensionOrigin := "chrome-extension://phbnnpidffgnnajfdmgglbphjkbindkd"
	corsOptions, err := anypb.New(&corsv1.Cors{
		AllowOrigins: []string{extensionOrigin},
		AllowHeaders: []string{"Content-Type", "X-GoAlgo-Plugin-Token", "X-GoAlgo-Sync-Session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	corsFactory, err := corsmiddleware.Middleware(&config.Middleware{Name: "cors", Options: corsOptions})
	if err != nil {
		t.Fatal(err)
	}
	jwtFactory, err := Middleware(middlewareConfig(t, testPubKeyPEM(t)))
	if err != nil {
		t.Fatal(err)
	}

	backend := middleware.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return buildUnauthorizedResp("backend must not handle preflight"), nil
	})
	// gateway/proxy wraps global middleware in declared order: jwt is outermost,
	// so this verifies the real JWT -> CORS preflight path.
	chain := jwtFactory(corsFactory(backend))

	for _, test := range []struct {
		name string
		path string
		want int
	}{
		{name: "exact start", path: "/v1/core/spider/luogu-sync/start", want: http.StatusOK},
		{name: "adjacent path", path: "/v1/core/spider/luogu-sync/start/nearby", want: http.StatusUnauthorized},
		{name: "prefix path", path: "/v1/core/spider/luogu-sync", want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodOptions, test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", extensionOrigin)
			req.Header.Set("Access-Control-Request-Headers", "content-type,x-goalgo-plugin-token")
			resp, err := chain.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != test.want {
				t.Fatalf("preflight status = %d, want %d", resp.StatusCode, test.want)
			}
			if test.want == http.StatusOK && resp.Header.Get("Access-Control-Allow-Origin") != extensionOrigin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", resp.Header.Get("Access-Control-Allow-Origin"), extensionOrigin)
			}
		})
	}
}
