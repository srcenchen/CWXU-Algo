package server

import (
	"context"
	nethttp "net/http"
	"strings"

	"cwxu-algo/api/user/v1/auth"
	blog "cwxu-algo/api/user/v1/blog"
	"cwxu-algo/api/user/v1/group"
	notificationpb "cwxu-algo/api/user/v1/notification"
	orgpb "cwxu-algo/api/user/v1/org"
	pastepb "cwxu-algo/api/user/v1/paste"
	"cwxu-algo/api/user/v1/profile"
	rbacpb "cwxu-algo/api/user/v1/rbac"
	"cwxu-algo/api/user/v1/role"
	"cwxu-algo/api/user/v1/site"
	backuppb "cwxu-algo/api/user/v1/site/backup"
	"cwxu-algo/api/user/v1/social"
	subscriptionpb "cwxu-algo/api/user/v1/subscription"
	"cwxu-algo/app/common/conf"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/opsmetrics"
	authutil "cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/health"
	"cwxu-algo/app/common/utils/safeerrors"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/service"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/auth/jwt"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/selector"
	"github.com/go-kratos/kratos/v2/transport/http"
	jwt2 "github.com/golang-jwt/jwt/v5"
)

func NewWhiteListMatcher() selector.MatchFunc {
	whiteList := map[string]string{
		"/api.user.v1.Auth/Login":                      "",
		"/api.user.v1.Auth/Register":                   "",
		"/api.user.v1.Auth/SendCode":                   "",
		"/api.user.v1.Auth/ResetPassword":              "",
		"/api.user.v1.Auth/Logout":                     "",
		"/api.user.v1.Profile/GetById":                 "",
		"/api.user.v1.Profile/GetByUsername":           "",
		"/api.user.v1.Profile/GetByName":               "",
		"/api.user.v1.Profile/GetFollowingIds":         "",
		"/api.user.v1.Profile/FilterPublicFeedUserIds": "",
		"/api.user.v1.role.Role/List":                  "",
		"/api.user.v1.site.Site/GetConfig":             "",
		"/api.user.v1.site.Site/VisitPing":             "",
		// C 端订阅：套餐列表公开（前端对比表）
		"/api.user.v1.subscription.Subscription/ListPlans": "",
		// 社交：搜索/列表/计数/关系/身份/隐私状态公开读（JWT 可选，有则按当前域解析）；关注操作仍需登录
		"/api.user.v1.social.Social/Search":        "",
		"/api.user.v1.social.Social/Following":     "",
		"/api.user.v1.social.Social/Followers":     "",
		"/api.user.v1.social.Social/Counts":        "",
		"/api.user.v1.social.Social/Relation":      "",
		"/api.user.v1.social.Social/Identity":      "",
		"/api.user.v1.social.Social/PrivacyStatus": "",
		// 粘贴板公开查看（单条内容）
		"/api.user.v1.paste.Paste/Get": "",
		// 支付宝支付回调（原生路由；operation=路径；签名验签在服务内完成）
		"/v1/payment/notify": "",
		// 组织广场公开（仅名/logo/人数）；邀请链接预览公开
		"/api.user.v1.org.Org/Discover":      "",
		"/api.user.v1.org.Org/InvitePreview": "",
	}
	return func(ctx context.Context, operation string) bool {
		// 静态资源公开
		if strings.Contains(operation, "static") {
			return false
		}
		// 博客公开读（写仍需登录；JWT 可选，有则识别作者）。
		// proto 迁移后 operation 形如 /api.user.v1.blog.Blog/ListByUsername，
		// 白名单按新 operation 名匹配，与迁移前「blog/...」公开读集合完全一致。
		if strings.HasPrefix(operation, "/api.user.v1.blog.Blog/") {
			switch strings.TrimPrefix(operation, "/api.user.v1.blog.Blog/") {
			case "ListByUsername", "GetArticle", "Unlock", "Recommend", "Plaza", "Authors",
				"ListCategoriesPublic", "ListComments", "ThemeStatus", "AgreementGet",
				// publish 用 X-Plugin-Publish-Token，不经 JWT
				"ObsidianPluginLatest", "ObsidianPluginPublish":
				return false
			}
		}
		// SEO HTML / sitemap 公开
		if strings.Contains(operation, "seo/html") ||
			strings.Contains(operation, "seo/meta") ||
			strings.Contains(operation, "seo/sitemap") {
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
	subscriptionService *service.SubscriptionService,
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
	profile.RegisterProfileHTTPServer(srv, profileService)
	group.RegisterGroupHTTPServer(srv, groupService)
	role.RegisterRoleHTTPServer(srv, roleService)
	site.RegisterSiteHTTPServer(srv, siteService)
	service.RegisterUploadRoutes(srv, d)
	orgpb.RegisterOrgHTTPServer(srv, orgService)
	rbacpb.RegisterRbacHTTPServer(srv, rbacService)
	pastepb.RegisterPasteHTTPServer(srv, pasteService)
	social.RegisterSocialHTTPServer(srv, socialService)
	notificationpb.RegisterNotificationHTTPServer(srv, notificationService)
	blog.RegisterBlogHTTPServer(srv, blogService)
	service.RegisterSEORoutes(srv, seoService)
	service.RegisterBackupRoutes(srv, d)
	backuppb.RegisterBackupHTTPServer(srv, service.NewBackupService(d))
	// C 端订阅（套餐/订单/站管管理）
	subscriptionpb.RegisterSubscriptionHTTPServer(srv, subscriptionService)
	// 支付宝异步回调：x-www-form-urlencoded 原生 handler（不走 proto JSON）
	srv.Handle("/v1/payment/notify", nethttp.HandlerFunc(subscriptionService.NotifyHTTP))
	return srv
}
