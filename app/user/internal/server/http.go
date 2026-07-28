package server

import (
	"context"
	"cwxu-algo/api/user/v1/auth"
	"cwxu-algo/api/user/v1/group"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/api/user/v1/role"
	"cwxu-algo/api/user/v1/site"
	"cwxu-algo/app/common/conf"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/opsmetrics"
	authutil "cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/health"
	"cwxu-algo/app/common/utils/safeerrors"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/service"
	"strings"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/auth/jwt"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/selector"
	"github.com/go-kratos/kratos/v2/transport/http"
	jwt2 "github.com/golang-jwt/jwt/v5"
)

func NewWhiteListMatcher() selector.MatchFunc {
	whiteList := map[string]string{
		"/api.user.v1.Auth/Login":                    "",
		"/api.user.v1.Auth/Register":                 "",
		"/api.user.v1.Auth/SendCode":                 "",
		"/api.user.v1.Auth/ResetPassword":            "",
		"/api.user.v1.Profile/GetById":               "",
		"/api.user.v1.Profile/GetByUsername":         "",
		"/api.user.v1.Profile/GetByName":             "",
		"/api.user.v1.Profile/GetFollowingIds":       "",
		"/api.user.v1.Profile/FilterPublicFeedUserIds": "",
		"/api.user.v1.role.Role/List":                "",
		"/api.user.v1.site.Site/GetConfig":           "",
		"/api.user.v1.site.Site/VisitPing":           "",
	}
	return func(ctx context.Context, operation string) bool {
		if strings.Contains(operation, "auth/logout") {
			return false
		}
		// 静态资源公开
		if strings.Contains(operation, "static") {
			return false
		}
		// 粘贴板公开查看
		if strings.Contains(operation, "paste/get") {
			return false
		}
		// 博客公开读（写仍需登录；JWT 可选，有则识别作者）
		if strings.Contains(operation, "blog/by-username") ||
			strings.Contains(operation, "blog/article/get") ||
			strings.Contains(operation, "blog/article/unlock") ||
			strings.Contains(operation, "blog/recommend") ||
			strings.Contains(operation, "blog/plaza") ||
			strings.Contains(operation, "blog/authors") ||
			strings.Contains(operation, "blog/categories") ||
			strings.Contains(operation, "blog/comment/list") ||
			strings.Contains(operation, "blog/theme/status") ||
			strings.Contains(operation, "blog/agreement") {
			return false
		}
		// SEO HTML / sitemap 公开
		if strings.Contains(operation, "seo/html") ||
			strings.Contains(operation, "seo/meta") ||
			strings.Contains(operation, "seo/sitemap") {
			return false
		}
		// 社交：搜索/列表/计数/身份展示公开（关注操作仍需登录；JWT 可选，有则按当前域解析）
		if strings.Contains(operation, "social/search") ||
			strings.Contains(operation, "social/following") ||
			strings.Contains(operation, "social/followers") ||
			strings.Contains(operation, "social/counts") ||
			strings.Contains(operation, "social/relation") ||
			strings.Contains(operation, "social/identity") ||
			strings.Contains(operation, "privacy/status") {
			return false
		}
		// 组织广场公开（仅名/logo/人数）；邀请链接预览公开
		if strings.Contains(operation, "org/discover") ||
			strings.Contains(operation, "org/invite/preview") {
			return false
		}
		if _, ok := whiteList[operation]; ok {
			return false
		}
		return true
	}
}

// NewHTTPServer new an HTTP server.
func NewHTTPServer(
	c *conf.Server,
	d *data.Data,
	authService *service.AuthService,
	profileService *service.ProfileService,
	groupService *service.GroupService,
	roleService *service.RoleService,
	siteService *service.SiteService,
	orgService *service.OrgService,
	rbacService *service.RbacService,
	pasteService *service.PasteService,
	socialService *service.SocialService,
	notificationService *service.NotificationService,
	blogService *service.BlogService,
	seoService *service.SEOService,
	logger log.Logger,

) *http.Server {
	var opts = []http.ServerOption{
		http.Middleware(
			recovery.Recovery(),
			safeerrors.Middleware(),
			opsmetrics.Middleware(d.RDB, "user"),
			authutil.CookieBearer(),
			selector.Server(jwt.Server(func(token *jwt2.Token) (interface{}, error) {
				if token.Method != jwt2.SigningMethodHS256 {
					return nil, jwt2.ErrSignatureInvalid
				}
				return []byte(_const.JWTSecret()), nil
			})).Match(NewWhiteListMatcher()).Build(),
		),
	}
	if c.Http.Network != "" {
		opts = append(opts, http.Network(c.Http.Network))
	}
	if c.Http.Addr != "" {
		opts = append(opts, http.Address(c.Http.Addr))
	}
	if c.Http.Timeout != nil {
		opts = append(opts, http.Timeout(c.Http.Timeout.AsDuration()))
	}
	srv := http.NewServer(opts...)
	health.Register(srv, health.Checker{DB: d.DB, RDB: d.RDB})
	auth.RegisterAuthHTTPServer(srv, authService)
	service.RegisterAuthSessionRoutes(srv)
	profile.RegisterProfileHTTPServer(srv, profileService)
	group.RegisterGroupHTTPServer(srv, groupService)
	role.RegisterRoleHTTPServer(srv, roleService)
	site.RegisterSiteHTTPServer(srv, siteService)
	service.RegisterUploadRoutes(srv, d)
	service.RegisterOrgRoutes(srv, orgService)
	service.RegisterSquadRoutes(srv, orgService)
	service.RegisterRbacRoutes(srv, rbacService)
	service.RegisterPasteRoutes(srv, pasteService)
	service.RegisterSocialRoutes(srv, socialService)
	service.RegisterNotificationRoutes(srv, notificationService)
	service.RegisterBlogRoutes(srv, blogService)
	service.RegisterSEORoutes(srv, seoService)
	service.RegisterBackupRoutes(srv, d)
	return srv
}
