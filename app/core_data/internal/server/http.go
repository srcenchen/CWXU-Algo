package server

import (
	"context"
	"strings"

	"cwxu-algo/api/core/v1/bulletin"
	"cwxu-algo/api/core/v1/contest_calendar"
	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/api/core/v1/emergency"
	"cwxu-algo/api/core/v1/problem"
	"cwxu-algo/api/core/v1/spider"
	statistic2 "cwxu-algo/api/core/v1/statistic"
	"cwxu-algo/api/core/v1/submit_log"
	"cwxu-algo/app/common/conf"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/opsmetrics"
	authutil "cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/health"
	"cwxu-algo/app/common/utils/safeerrors"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/service"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/auth/jwt"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/selector"
	"github.com/go-kratos/kratos/v2/transport/http"
	jwt2 "github.com/golang-jwt/jwt/v5"
)

func NewWhiteListMatcher() selector.MatchFunc {
	whiteList := map[string]string{
		"/api.core.v1.submit_log.Submit/GetSubmitLog":        "",
		"/api.core.v1.contest_log.Contest/GetContestList":         "",
		"/api.core.v1.contest_log.Contest/GetUserContestHistory": "",
		"/api.core.v1.contest_log.Contest/GetContestRanking": "",
		"/api.core.v1.spider.Spider/GetSpider":               "",
		"/api.core.v1.statistic.Statistic/Heatmap":           "",
		"/api.core.v1.statistic.Statistic/PeriodCount":       "",
		"/api.core.v1.statistic.Statistic/Rank":              "",
		"/api.core.v1.bulletin.Bulletin/Get":                 "",
		"/api.core.v1.bulletin.Bulletin/List":                "",
		"/api.core.v1.emergency.Emergency/Active":            "",
		"/api.core.v1.problem.Problem/List":                  "",
		"/api.core.v1.problem.Problem/ListTags":              "",
		"/api.core.v1.problem.Problem/Get":                   "",
		"/api.core.v1.problem.Problem/RelatedContests":        "",
		"/api.core.v1.problem.Problem/ListSubmissions":       "",
		"/api.core.v1.problem.Problem/UserProfile":           "",
		"/api.core.v1.contest_calendar.ContestCalendar/ListCalendar":  "",
		"/api.core.v1.contest_calendar.ContestCalendar/ListPlatforms": "",
	}
	return func(ctx context.Context, operation string) bool {
		//log.Info(operation)
		// 评论/题解列表与资料近期、发现流公开读；写操作仍需登录
		// 题单：广场 / 公有详情 / 按题关联 可匿名；其余需登录
		if strings.Contains(operation, "problemset/square") ||
			strings.Contains(operation, "problemset/get") ||
			strings.Contains(operation, "problemset/by-problem") ||
			strings.Contains(operation, "problemset/unlock") {
			return false
		}
		if strings.Contains(operation, "problem/comment/list") ||
			strings.Contains(operation, "problem/solution/list") ||
			strings.Contains(operation, "problem/solution/get") ||
			strings.Contains(operation, "activity/feed") ||
			strings.Contains(operation, "user/recent-comments") ||
			strings.Contains(operation, "user/recent-solutions") ||
			strings.Contains(operation, "contest/problems") ||
			strings.Contains(operation, "contest/board") ||
			strings.Contains(operation, "contest/cell-submits") {
			return false
		}
		if _, ok := whiteList[operation]; ok {
			return false
		}
		return true
	}
}

// NewHTTPServer new an HTTP server.
func NewHTTPServer(c *conf.Server, logger log.Logger, d *data.Data, submitService *service.SubmitLogService, spiderService *service.SpiderService, statisticService *service.StatisticService, contestLogService *service.ContestLogService, bulletinService *service.BulletinService, problemService *service.ProblemService, emergencyService *service.EmergencyService, contestCalendarService *service.ContestCalendarService, communityService *service.CommunityService, problemsetService *service.ProblemsetService) *http.Server {
	var opts = []http.ServerOption{
		http.Middleware(
			recovery.Recovery(),
			safeerrors.Middleware(),
			opsmetrics.Middleware(d.RDB, "core"),
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
	submit_log.RegisterSubmitHTTPServer(srv, submitService)
	spider.RegisterSpiderHTTPServer(srv, spiderService)
	statistic2.RegisterStatisticHTTPServer(srv, statisticService)
	contest_log.RegisterContestHTTPServer(srv, contestLogService)
	bulletin.RegisterBulletinHTTPServer(srv, bulletinService)
	problem.RegisterProblemHTTPServer(srv, problemService)
	emergency.RegisterEmergencyHTTPServer(srv, emergencyService)
	contest_calendar.RegisterContestCalendarHTTPServer(srv, contestCalendarService)
	service.RegisterCommunityRoutes(srv, communityService)
	service.RegisterProblemsetRoutes(srv, problemsetService)
	service.RegisterContestExtraRoutes(srv, contestLogService)
	service.RegisterSpiderExtraRoutes(srv, spiderService)
	service.RegisterProblemExtraRoutes(srv, problemService)
	return srv
}
