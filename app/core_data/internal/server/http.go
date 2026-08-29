package server

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"strconv"
	"strings"

	backuppb "cwxu-algo/api/core/v1/backup"
	"cwxu-algo/api/core/v1/bulletin"
	"cwxu-algo/api/core/v1/community"
	"cwxu-algo/api/core/v1/contest_calendar"
	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/api/core/v1/emergency"
	healthpb "cwxu-algo/api/core/v1/health"
	"cwxu-algo/api/core/v1/problem"
	"cwxu-algo/api/core/v1/problemset"
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

	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware/auth/jwt"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/selector"
	"github.com/go-kratos/kratos/v2/transport/http"
	jwt2 "github.com/golang-jwt/jwt/v5"
)

func NewWhiteListMatcher() selector.MatchFunc {
	whiteList := map[string]string{
		"/api.core.v1.submit_log.Submit/GetSubmitLog":            "",
		"/api.core.v1.contest_log.Contest/GetContestList":        "",
		"/api.core.v1.contest_log.Contest/GetUserContestHistory": "",
		"/api.core.v1.contest_log.Contest/GetContestRanking":     "",
		// 比赛题目目录 / 站内榜 / 格子提交明细 公开读（proto 化后 operation 全名；
		// 网关侧对应 /v1/core/contest/{problems,board,cell-submits} 路径白名单不变）
		"/api.core.v1.contest_log.Contest/GetContestProblems":         "",
		"/api.core.v1.contest_log.Contest/GetContestBoard":            "",
		"/api.core.v1.contest_log.Contest/GetContestCellSubmits":      "",
		"/api.core.v1.spider.Spider/GetSpider":                        "",
		"/api.core.v1.statistic.Statistic/Heatmap":                    "",
		"/api.core.v1.statistic.Statistic/PeriodCount":                "",
		"/api.core.v1.statistic.Statistic/Rank":                       "",
		"/api.core.v1.bulletin.Bulletin/Get":                          "",
		"/api.core.v1.bulletin.Bulletin/List":                         "",
		"/api.core.v1.emergency.Emergency/Active":                     "",
		"/api.core.v1.problem.Problem/List":                           "",
		"/api.core.v1.problem.Problem/ListTags":                       "",
		"/api.core.v1.problem.Problem/Get":                            "",
		"/api.core.v1.problem.Problem/RelatedContests":                "",
		"/api.core.v1.problem.Problem/ListSubmissions":                "",
		"/api.core.v1.problem.Problem/UserProfile":                    "",
		"/api.core.v1.contest_calendar.ContestCalendar/ListCalendar":  "",
		"/api.core.v1.contest_calendar.ContestCalendar/ListPlatforms": "",
		// 题单公开读（proto 化后 operation 全名）
		"/api.core.v1.problemset.Problemset/Square":    "",
		"/api.core.v1.problemset.Problemset/Get":       "",
		"/api.core.v1.problemset.Problemset/ByProblem": "",
		"/api.core.v1.problemset.Problemset/Unlock":    "",
		// 社区公开读（评论/题解列表与详情、发现流、资料近期）；写操作仍需登录
		"/api.core.v1.community.Community/CommentList":         "",
		"/api.core.v1.community.Community/SolutionList":        "",
		"/api.core.v1.community.Community/SolutionGet":         "",
		"/api.core.v1.community.Community/ActivityFeed":        "",
		"/api.core.v1.community.Community/UserRecentComments":  "",
		"/api.core.v1.community.Community/UserRecentSolutions": "",
		// 浏览器本地同步使用独立设备/session token；这里只跳过站内 JWT，
		// SpiderService 仍对精确 header 做强制鉴权。
		"/api.core.v1.spider.Spider/StartLuoguSync":      "",
		"/api.core.v1.spider.Spider/LuoguSyncStatus":     "",
		"/api.core.v1.spider.Spider/UploadLuoguSyncPage": "",
		"/api.core.v1.spider.Spider/ResolveLuoguUser":    "",
	}
	return func(ctx context.Context, operation string) bool {
		//log.Info(operation)
		// 题单：广场 / 公有详情 / 按题关联 / 解锁 可匿名；其余需登录
		if _, ok := whiteList[operation]; ok {
			return false
		}
		return true
	}
}

func luoguSyncErrorEncoder(w stdhttp.ResponseWriter, r *stdhttp.Request, err error) {
	if r == nil || !strings.HasPrefix(r.URL.Path, "/v1/core/spider/luogu-sync/") {
		http.DefaultErrorEncoder(w, r, err)
		return
	}
	serviceErr := kratoserrors.FromError(err)
	code := serviceErr.Reason
	if code == "" {
		code = "SYNC_UNAVAILABLE"
	}
	body := map[string]interface{}{"code": code, "message": serviceErr.Message}
	for _, key := range []string{"nextAvailableAt", "retryAfterSeconds"} {
		if raw := serviceErr.Metadata[key]; raw != "" {
			if value, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
				body[key] = value
			}
		}
	}
	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		w.WriteHeader(stdhttp.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(int(serviceErr.Code))
	_, _ = w.Write(payload)
}

// NewHTTPServer new an HTTP server.
func NewHTTPServer(c *conf.Server, logger log.Logger, d *data.Data, submitService *service.SubmitLogService, spiderService *service.SpiderService, statisticService *service.StatisticService, contestLogService *service.ContestLogService, bulletinService *service.BulletinService, problemService *service.ProblemService, emergencyService *service.EmergencyService, contestCalendarService *service.ContestCalendarService, communityService *service.CommunityService, problemsetService *service.ProblemsetService, healthService *service.HealthService, backupService *service.BackupService) *http.Server {
	var opts = []http.ServerOption{
		http.ErrorEncoder(luoguSyncErrorEncoder),
		http.Middleware(
			recovery.Recovery(),
			safeerrors.Middleware(),
			opsmetrics.Middleware(d.RDB, "core"),
			authutil.CookieBearer(),
			selector.Server(jwt.Server(func(token *jwt2.Token) (interface{}, error) {
				if token.Method != jwt2.SigningMethodRS256 {
					return nil, jwt2.ErrSignatureInvalid
				}
				return _const.JWTPublicKey(), nil
			}, jwt.WithSigningMethod(jwt2.SigningMethodRS256))).Match(NewWhiteListMatcher()).Build(),
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
	// 站管触发：全站仅重建能力画像（不重爬 OJ）。HandleFunc 不走 selector 中间件，
	// auth.VerifySiteAdmin 会自行解析 Authorization JWT 校验站管身份。
	srv.HandleFunc("/v1/core/problem/rebuild-profiles", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.Method != stdhttp.MethodPost {
			w.Header().Set("Allow", stdhttp.MethodPost)
			stdhttp.Error(w, "method not allowed", stdhttp.StatusMethodNotAllowed)
			return
		}
		candidates, published, unauthorized, rebuildErr := problemService.RebuildAllProfiles(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if unauthorized {
			w.WriteHeader(stdhttp.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "message": "仅站点管理员可操作"})
			return
		}
		if rebuildErr != nil {
			w.WriteHeader(stdhttp.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 1, "message": rebuildErr.Error(),
				"candidates": 0, "published": 0,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "success",
			"candidates": candidates, "published": published,
		})
	})
	emergency.RegisterEmergencyHTTPServer(srv, emergencyService)
	contest_calendar.RegisterContestCalendarHTTPServer(srv, contestCalendarService)
	community.RegisterCommunityHTTPServer(srv, communityService)
	problemset.RegisterProblemsetHTTPServer(srv, problemsetService)
	healthpb.RegisterHealthHTTPServer(srv, healthService)
	backuppb.RegisterBackupHTTPServer(srv, backupService)
	return srv
}
