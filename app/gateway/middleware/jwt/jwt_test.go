package jwt

import (
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

func TestMiddlewareReadsSecretFromConfig(t *testing.T) {
	t.Setenv("CWXU_JWT_SECRET", "")
	if _, err := Middleware(middlewareConfig(t, strings.Repeat("c", 32))); err != nil {
		t.Fatal(err)
	}
}

func TestMiddlewareRejectsShortConfigSecret(t *testing.T) {
	t.Setenv("CWXU_JWT_SECRET", "")
	if _, err := Middleware(middlewareConfig(t, "short")); err == nil {
		t.Fatal("expected short config secret to be rejected")
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
	} {
		if _, ok := publicExact[path]; !ok {
			t.Errorf("public blog page path missing from JWT whitelist: %s", path)
		}
	}
}
