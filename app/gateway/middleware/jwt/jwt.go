package jwt

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	config "github.com/go-kratos/gateway/api/gateway/config/v1"
	jwtv1 "github.com/go-kratos/gateway/api/gateway/middleware/jwt/v1"
	"github.com/go-kratos/gateway/middleware"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func init() {
	middleware.Register("jwt", Middleware)
}

// jwtPublicKey parses the RSA public key PEM used to verify product JWTs.
// Env CWXU_JWT_PUBLIC_KEY wins over options.secret; PEM parse failure is fatal.
func jwtPublicKey(configValue string) (*rsa.PublicKey, error) {
	v := strings.TrimSpace(os.Getenv("CWXU_JWT_PUBLIC_KEY"))
	if v == "" {
		v = strings.TrimSpace(configValue)
	}
	if v == "" {
		return nil, errors.New("jwt middleware options.secret must be an RSA public key PEM")
	}
	block, _ := pem.Decode([]byte(v))
	if block == nil {
		return nil, errors.New("jwt middleware options.secret is not valid PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("jwt middleware options.secret: parse public key: " + err.Error())
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("jwt middleware options.secret is not an RSA public key")
	}
	return pub, nil
}

// exact public path suffixes (after cleaning)
// 与 shared/api.md「Auth: 否」及 user 服务 NewWhiteListMatcher 对齐；
// 可选 JWT 的接口也列入：有合法 token 则透传，无/非法则按匿名放行。
var publicExact = map[string]struct{}{
	"/v1/user/auth/login":          {},
	"/v1/user/auth/logout":         {},
	"/v1/user/auth/register":       {},
	"/v1/user/auth/send-code":      {},
	"/v1/user/auth/reset-password": {},
	// 洛谷浏览器同步：令牌交换与三个同步精确端点免站内 JWT；
	// user/core 服务仍分别强制校验一次性码、PKCE、设备令牌或会话令牌。
	"/v1/user/plugin/luogu/token":        {},
	"/api/user/plugin/luogu/token":       {},
	"/v1/core/spider/luogu-sync/start":   {},
	"/api/core/spider/luogu-sync/start":  {},
	"/v1/core/spider/luogu-sync/status":  {},
	"/api/core/spider/luogu-sync/status": {},
	"/v1/core/spider/luogu-sync/page":    {},
	"/api/core/spider/luogu-sync/page":   {},
	// 洛谷 UID/用户名解析为公开只读接口；绑定保存仍需 GoAlgo JWT。
	"/v1/core/spider/luogu/resolve-user":  {},
	"/api/core/spider/luogu/resolve-user": {},
	// 支付FM回调（GET query / POST form，签名验签由 user 服务完成）
	"/v1/payment/notify":  {},
	"/api/payment/notify": {},
	// 客户中心 webhook 回调（HMAC 验签由 user 服务完成，无 JWT）
	"/v1/support/events":  {},
	"/api/support/events": {},
	// Profile 公开读
	"/v1/user/profile/get-by-id":       {},
	"/v1/user/profile/get-by-username": {},
	"/v1/user/profile/get-by-name":     {},
	"/v1/user/profile/following-ids":   {},
	"/v1/user/role/list":               {},
	"/v1/user/paste/get":               {},
	"/api/user/paste/get":              {},
	// C 端订阅：套餐列表公开（前端对比表）
	"/v1/user/subscription/plans":  {},
	"/api/user/subscription/plans": {},
	// Blog public reads
	"/v1/user/blog/by-username":     {},
	"/api/user/blog/by-username":    {},
	"/v1/user/blog/article/get":     {},
	"/api/user/blog/article/get":    {},
	"/v1/user/blog/article/unlock":  {},
	"/api/user/blog/article/unlock": {},
	"/v1/user/blog/recommend":       {},
	"/api/user/blog/recommend":      {},
	"/v1/user/blog/plaza":           {},
	"/api/user/blog/plaza":          {},
	"/v1/user/blog/authors":         {},
	"/api/user/blog/authors":        {},
	"/v1/user/blog/categories":      {},
	"/api/user/blog/categories":     {},
	"/v1/user/blog/tags":            {},
	"/api/user/blog/tags":           {},
	"/v1/user/blog/page/list":       {},
	"/api/user/blog/page/list":      {},
	"/v1/user/blog/page/get":        {},
	"/api/user/blog/page/get":       {},
	"/v1/user/blog/comment/list":    {},
	"/api/user/blog/comment/list":   {},
	"/v1/user/blog/theme/status":    {},
	"/api/user/blog/theme/status":   {},
	"/v1/user/blog/agreement":       {},
	"/api/user/blog/agreement":      {},
	// Obsidian 插件版本（公开读；publish 用 Header 令牌鉴权，不经 JWT）
	"/v1/user/blog/obsidian-plugin/latest":   {},
	"/api/user/blog/obsidian-plugin/latest":  {},
	"/v1/user/blog/obsidian-plugin/publish":  {},
	"/api/user/blog/obsidian-plugin/publish": {},
	"/v1/user/site/config":                   {},
	"/api/user/site/config":                  {},
	"/v1/user/site/visit-ping":               {},
	"/api/user/site/visit-ping":              {},
	// SEO (public HTML meta for crawlers / share previews)
	"/v1/user/seo/html":         {},
	"/api/user/seo/html":        {},
	"/v1/user/seo/meta":         {},
	"/api/user/seo/meta":        {},
	"/v1/user/seo/sitemap.xml":  {},
	"/api/user/seo/sitemap.xml": {},
	// Social 公开读（关注/取关仍需 JWT）
	"/v1/user/social/following": {},
	"/v1/user/social/followers": {},
	"/v1/user/social/counts":    {},
	"/v1/user/social/relation":  {},
	"/v1/user/social/search":    {},
	"/v1/user/social/identity":  {},
	"/v1/user/privacy/status":   {},
	// 组织广场 / 邀请链接预览公开
	"/v1/user/org/discover":       {},
	"/v1/user/org/invite/preview": {},
	// Core 公开读
	"/v1/core/submit-log/get-by-id": {},
	"/v1/core/contest/list":         {},
	"/v1/core/contest/history":      {},
	"/v1/core/contest/ranking":      {},
	"/v1/core/contest/problems":     {},
	"/v1/core/contest/board":        {},
	"/v1/core/contest/cell-submits": {},
	// 比赛日历公开读（与 core 白名单 / api.md Auth:否 对齐）
	"/v1/core/contest-calendar/list":      {},
	"/v1/core/contest-calendar/platforms": {},
	"/v1/core/statistic/heatmap":          {},
	"/v1/core/statistic/period":           {},
	"/v1/core/statistic/rank":             {},
	"/v1/core/bulletin/get":               {},
	"/v1/core/bulletin/list":              {},
	"/v1/core/emergency/active":           {},
	"/v1/core/problem/list":               {},
	"/v1/core/problem/tags":               {},
	"/v1/core/problem/hot":                {},
	"/v1/core/problem/get":                {},
	"/v1/core/problem/related-contests":   {},
	"/v1/core/problem/submissions":        {},
	"/v1/core/problem/user-profile":       {},
	// 题目评论/用户题解/发现讨论（读公开；写仍需 JWT）
	"/v1/core/problem/comment/list":  {},
	"/v1/core/problem/solution/list": {},
	"/v1/core/problem/solution/get":  {},
	"/v1/core/activity/feed":         {},
	"/v1/core/user/recent-comments":  {},
	"/v1/core/user/recent-solutions": {},
	// 题单：广场 / 详情 / 按题关联 / 密码解锁 可匿名
	"/v1/core/problemset/square":     {},
	"/v1/core/problemset/get":        {},
	"/v1/core/problemset/by-problem": {},
	"/v1/core/problemset/unlock":     {},
}

func parseBearer(pub *rsa.PublicKey, authHeader string) (tokenStr string, valid bool) {
	tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
	if tokenStr == authHeader || tokenStr == "" {
		return "", false
	}
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return pub, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("goalgo"),
		jwt.WithAudience("goalgo-web"),
	)
	if err != nil || token == nil || !token.Valid {
		return tokenStr, false
	}
	return tokenStr, true
}

// Middleware jwt 校验中间件
func Middleware(c *config.Middleware) (middleware.Middleware, error) {
	options := &jwtv1.JWT{}
	if c.Options != nil {
		if err := anypb.UnmarshalTo(c.Options, options, proto.UnmarshalOptions{Merge: true}); err != nil {
			return nil, err
		}
	}
	pub, err := jwtPublicKey(options.Secret)
	if err != nil {
		return nil, err
	}
	return func(next http.RoundTripper) http.RoundTripper {
		return middleware.RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			uriPath := path.Clean(request.URL.Path)
			_, isPublic := publicExact[uriPath]
			// 静态资源公开
			if strings.HasPrefix(uriPath, "/v1/user/static/") || strings.HasPrefix(uriPath, "/api/user/static/") {
				isPublic = true
			}
			// 健康检查
			if uriPath == "/healthz" || uriPath == "/readyz" {
				isPublic = true
			}

			authHeader := request.Header.Get("Authorization")
			if strings.TrimSpace(authHeader) == "" {
				if cookie, err := request.Cookie("goalgo_session"); err == nil && strings.TrimSpace(cookie.Value) != "" {
					authHeader = "Bearer " + cookie.Value
					request.Header.Set("Authorization", authHeader)
				} else if t := strings.TrimSpace(request.URL.Query().Get("access_token")); t != "" {
					// 浏览器原生下载无法带 Authorization，备份等附件下载可用 query 兜底
					authHeader = "Bearer " + t
					request.Header.Set("Authorization", authHeader)
				}
			}

			// 公开接口：无 token 直接放行；有合法 token 透传（域感知展示）；非法/过期则剥掉按匿名
			if isPublic {
				if strings.TrimSpace(authHeader) == "" {
					return next.RoundTrip(request)
				}
				if _, ok := parseBearer(pub, authHeader); ok {
					return next.RoundTrip(request)
				}
				request.Header.Del("Authorization")
				return next.RoundTrip(request)
			}

			// 需登录：必须有合法 JWT
			if strings.TrimSpace(authHeader) == "" {
				return buildUnauthorizedResp("JWT Token not found"), nil
			}
			if _, ok := parseBearer(pub, authHeader); !ok {
				// 区分「没有 Bearer」与「非法/过期」
				tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
				if tokenStr == authHeader || tokenStr == "" {
					return buildUnauthorizedResp("JWT Token not found"), nil
				}
				return buildUnauthorizedResp("JWT Token invalid"), nil
			}
			return next.RoundTrip(request)
		})
	}, nil
}

func buildUnauthorizedResp(msg string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(bytes.NewBufferString(msg)),
		Header:     make(http.Header),
	}
}
