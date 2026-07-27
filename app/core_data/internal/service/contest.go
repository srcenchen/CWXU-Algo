package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/common/utils/auth"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/redis/go-redis/v9"
	grpc2 "google.golang.org/grpc"
	"gorm.io/gorm"
)

// contestListCacheMaxOffset 「全部比赛」列表短缓存覆盖的最大 offset（再深就直接查库）
const contestListCacheMaxOffset = 500

type ContestLogService struct {
	contest_log.UnimplementedContestServer
	sbDal *dal.SpiderDal
	db    *gorm.DB
	rdb   *redis.Client
	reg   *registry.Registrar
	prob  *bizservice.ProblemUseCase
}

func (c ContestLogService) userRPC() (*grpc2.ClientConn, error) {
	return userrpc.Conn(c.reg)
}

func (c ContestLogService) GetContestList(ctx context.Context, req *contest_log.GetContestListReq) (*contest_log.GetContestListRes, error) {
	var memberIDs []int64
	var resolvedOrg uint
	if req.UserId == -1 {
		ids, orgID, unrestricted, err := ResolveOrgMemberIDs(ctx, c.reg, 0, false)
		if err != nil {
			log.Warnf("org members for contest list: %v", err)
			ids = []int64{}
		}
		resolvedOrg = orgID
		if !unrestricted {
			memberIDs = ids
		}
	}

	type listPayload struct {
		Logs  []model.ContestLog
		Total int64
	}
	var logs []model.ContestLog
	var total int64
	var err error

	listQ := dal.ContestListQuery{
		UserId:    req.UserId,
		Offset:    req.Offset,
		Limit:     req.Limit,
		Platform:  req.Platform,
		Keyword:   req.Keyword,
		TimeFrom:  req.TimeFrom,
		TimeTo:    req.TimeTo,
		MemberIDs: memberIDs,
	}
	// 有关键字/时间筛选时跳过短缓存，避免 key 爆炸；翻页前几屏也缓存（key 含 offset，数量可控）
	useCache := req.UserId == -1 && c.rdb != nil &&
		req.Offset >= 0 && req.Offset <= contestListCacheMaxOffset &&
		req.Limit > 0 && req.Limit <= 50 &&
		strings.TrimSpace(req.Keyword) == "" && req.TimeFrom == 0 && req.TimeTo == 0

	// 组织首页短缓存（90s + global ver）
	if useCache {
		ver := "0"
		if v, e := c.rdb.Get(ctx, "core:contest:list:global:ver").Result(); e == nil && v != "" {
			ver = v
		}
		key := fmt.Sprintf("core:contest:list:org%d:p%s:off%d:lim%d:v%s",
			resolvedOrg, req.Platform, req.Offset, req.Limit, ver)
		if b, e := c.rdb.Get(ctx, key).Bytes(); e == nil && len(b) > 0 {
			var p listPayload
			if utils.GobDecoder(b, &p) == nil {
				logs, total = p.Logs, p.Total
			}
		}
		if logs == nil {
			logs, total, err = c.sbDal.GetContestListScoped(ctx, listQ)
			if err != nil {
				return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
			}
			if b, e := utils.GobEncoder(listPayload{Logs: logs, Total: total}); e == nil {
				_ = c.rdb.Set(ctx, key, b, 90*time.Second).Err()
			}
		}
	} else {
		logs, total, err = c.sbDal.GetContestListScoped(ctx, listQ)
		if err != nil {
			return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
		}
	}

	items := c.contestLogsToProto(ctx, logs)
	// 「全部比赛」按场次去重后代表行可能是别人的成绩：只保留当前登录用户自己的排名/过题，否则清空
	if req.UserId == -1 {
		c.attachViewerPersonalStats(ctx, items)
	}

	return &contest_log.GetContestListRes{
		Code:    0,
		Message: "OK",
		Data:    items,
		Total:   total,
	}, nil
}

// attachViewerPersonalStats 「全部比赛」列表：只暴露当前用户自己的成绩。
// 若当前用户未打过该场，清空他人 userId/rank/ac，保留 totalCount 供前端展示「共 N 题」。
func (c ContestLogService) attachViewerPersonalStats(ctx context.Context, items []*contest_log.ContestLog) {
	if len(items) == 0 {
		return
	}
	viewerID := int64(auth.GetCurrentUserId(ctx))
	myByKey := map[string]model.ContestLog{}
	if viewerID > 0 {
		or := c.db.Where("1 = 0")
		for _, it := range items {
			if it == nil || it.Platform == "" || it.ContestId == "" {
				continue
			}
			or = or.Or("platform = ? AND contest_id = ?", it.Platform, it.ContestId)
		}
		var mine []model.ContestLog
		if err := c.db.WithContext(ctx).Where("user_id = ?", viewerID).Where(or).Find(&mine).Error; err != nil {
			log.Warnf("viewer contest stats: %v", err)
		} else {
			for _, m := range mine {
				key := m.Platform + "\x00" + m.ContestId
				// 同一场多次记录取 id 较大者
				if prev, ok := myByKey[key]; !ok || m.ID > prev.ID {
					myByKey[key] = m
				}
			}
		}
	}

	var viewerName string
	if viewerID > 0 && len(myByKey) > 0 {
		if cli, err := userrpc.ProfileClient(c.reg); err == nil {
			seed := make([]model.ContestLog, 0, 1)
			for _, m := range myByKey {
				seed = append(seed, m)
				break
			}
			if names, _ := c.fetchUserNames(ctx, cli, seed); len(names) > 0 {
				viewerName = names[viewerID].Name
			}
		}
	}

	for _, it := range items {
		if it == nil {
			continue
		}
		key := it.Platform + "\x00" + it.ContestId
		if m, ok := myByKey[key]; ok {
			it.Id = uint32(m.ID)
			it.UserId = m.UserID
			it.Rank = int32(m.Rank)
			it.AcCount = int32(m.AcCount)
			if m.TotalCount > 0 {
				it.TotalCount = int32(m.TotalCount)
			}
			it.UserName = viewerName
			continue
		}
		// 未参赛 / 未登录：不暴露他人成绩
		it.UserId = 0
		it.UserName = ""
		it.Rank = 0
		it.AcCount = 0
	}
}

func contestLogToProto(v model.ContestLog) *contest_log.ContestLog {
	t := int64(0)
	if !v.Time.IsZero() {
		t = v.Time.Unix()
	}
	return &contest_log.ContestLog{
		Id:          uint32(v.ID),
		Platform:    v.Platform,
		UserId:      v.UserID,
		ContestId:   v.ContestId,
		ContestName: v.ContestName,
		ContestUrl:  v.ContestUrl,
		Rank:        int32(v.Rank),
		TotalCount:  int32(v.TotalCount),
		AcCount:     int32(v.AcCount),
		Time:        t,
	}
}

// contestLogsToProto 填起止时间；展示名仅在按用户筛选时有意义（「全部比赛」由 attachViewerPersonalStats 处理）
func (c ContestLogService) contestLogsToProto(ctx context.Context, logs []model.ContestLog) []*contest_log.ContestLog {
	items := make([]*contest_log.ContestLog, 0, len(logs))
	if len(logs) == 0 {
		return items
	}
	// 绑定请求 ctx：客户端断开/网关超时能及时取消这里的库查询
	times := bizservice.BatchContestDisplayTimes(c.db.WithContext(ctx), logs)
	for _, v := range logs {
		p := contestLogToProto(v)
		key := v.Platform + "\x00" + v.ContestId
		if se, ok := times[key]; ok {
			p.StartTime = se[0]
			p.EndTime = se[1]
			// 兼容：time 优先展示开赛
			if p.StartTime > 0 {
				p.Time = p.StartTime
			}
		}
		items = append(items, p)
	}
	return items
}

func (c ContestLogService) GetContestRanking(ctx context.Context, req *contest_log.GetContestRankingReq) (*contest_log.GetContestRankingRes, error) {
	contest := model.ContestLog{}
	_ = c.db.Where("id = ?", req.ContestId).First(&contest)

	contestProto := contestLogToProto(contest)
	// 与 contestMapWithTimes 一致：牛客可拉官方页补全赛时
	if m := contestMapWithTimes(c.db, contest); m != nil {
		if v, ok := m["startTime"].(int64); ok && v > 0 {
			contestProto.StartTime = v
			contestProto.Time = v
		}
		if v, ok := m["endTime"].(int64); ok && v > 0 {
			contestProto.EndTime = v
		}
	}

	// 复用进程内 user 长连接
	var userClient profile.ProfileClient
	if cli, err := userrpc.ProfileClient(c.reg); err != nil {
		log.Errorf("userRPC failed: %v", err)
	} else {
		userClient = cli
	}

	var userIds []int64
	if req.GroupId != nil && userClient != nil {
		res, err := userClient.GetUserIdsByGroup(ctx, &profile.GetUserIdsByGroupReq{GroupId: *req.GroupId})
		if err != nil {
			log.Errorf("GetUserIdsByGroup failed: %v", err)
			return nil, errors.InternalServer("内部服务器错误", "获取用户组信息失败")
		}
		userIds = res.UserIds
		if len(userIds) == 0 {
			return &contest_log.GetContestRankingRes{
				Code:    0,
				Message: "OK",
				Contest: contestProto,
				Data:    make([]*contest_log.RankingItem, 0),
				Total:   0,
			}, nil
		}
	} else if userClient != nil {
		// 默认队内榜：当前组织成员
		ids, _, unrestricted, err := ResolveOrgMemberIDsFromConn(ctx, userClient, 0, false)
		if err != nil {
			log.Warnf("org members for ranking: %v", err)
			ids = []int64{}
		}
		if !unrestricted {
			userIds = ids
			if len(userIds) == 0 {
				return &contest_log.GetContestRankingRes{
					Code:    0,
					Message: "OK",
					Contest: contestProto,
					Data:    make([]*contest_log.RankingItem, 0),
					Total:   0,
				}, nil
			}
		}
	}

	// 只看关注：与当前域/分组成员求交（仍受域限制）
	if req.FollowingOnly {
		viewer := auth.GetCurrentUserId(ctx)
		if viewer == 0 {
			return &contest_log.GetContestRankingRes{
				Code:    0,
				Message: "OK",
				Contest: contestProto,
				Data:    make([]*contest_log.RankingItem, 0),
				Total:   0,
			}, nil
		}
		following := fetchFollowingIDs(ctx, c.reg, int64(viewer))
		if userIds == nil {
			// unrestricted 全站站管路径：仅关注
			userIds = following
		} else {
			userIds = intersectIDs(userIds, following)
		}
		if len(userIds) == 0 {
			return &contest_log.GetContestRankingRes{
				Code:    0,
				Message: "OK",
				Contest: contestProto,
				Data:    make([]*contest_log.RankingItem, 0),
				Total:   0,
			}, nil
		}
	}

	// 非 following 的榜单短缓存（60s）；scope 用 group 或成员数量哈希
	type rankPayload struct {
		Logs  []model.ContestLog
		Total int64
	}
	var logs []model.ContestLog
	var total int64
	var err error
	rankCacheKey := ""
	if !req.FollowingOnly && c.rdb != nil && req.Offset == 0 && req.Limit > 0 && req.Limit <= 50 {
		scope := "all"
		if req.GroupId != nil {
			scope = fmt.Sprintf("g%d", *req.GroupId)
		} else if userIds != nil {
			scope = fmt.Sprintf("n%d", len(userIds))
		}
		rankCacheKey = fmt.Sprintf("core:contest:rank:%s:%s:%s:off%d:lim%d",
			contest.Platform, contest.ContestId, scope, req.Offset, req.Limit)
		if b, e := c.rdb.Get(ctx, rankCacheKey).Bytes(); e == nil && len(b) > 0 {
			var p rankPayload
			if utils.GobDecoder(b, &p) == nil {
				logs, total = p.Logs, p.Total
			}
		}
	}
	if logs == nil {
		logs, total, err = c.sbDal.GetContestRanking(ctx, contest.ContestId, contest.Platform, req.Offset, req.Limit, userIds)
		if err != nil {
			return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
		}
		if rankCacheKey != "" {
			if b, e := utils.GobEncoder(rankPayload{Logs: logs, Total: total}); e == nil {
				_ = c.rdb.Set(ctx, rankCacheKey, b, 60*time.Second).Err()
			}
		}
	}

	// 批量获取用户信息，一次 RPC 替代原来的 N 次 GetById
	nameMap, _ := c.fetchUserNames(ctx, userClient, logs)

	// 站内榜：有官方 rank 用官方；整页全是 0（未出分/爬失败）则按 AC 排序后模拟 1..n
	allZeroRank := true
	for _, v := range logs {
		if v.Rank > 0 {
			allZeroRank = false
			break
		}
	}

	items := make([]*contest_log.RankingItem, 0, len(logs))
	for i, v := range logs {
		u := nameMap[v.UserID]
		rank := int64(v.Rank)
		if rank <= 0 && allZeroRank {
			rank = req.Offset + int64(i) + 1
		}
		items = append(items, &contest_log.RankingItem{
			Rank:       rank,
			UserId:     v.UserID,
			Name:       u.Name,
			Avatar:     u.Avatar,
			AcCount:    int32(v.AcCount),
			TotalCount: int32(v.TotalCount),
		})
	}

	return &contest_log.GetContestRankingRes{
		Code:    0,
		Message: "OK",
		Contest: contestProto,
		Data:    items,
		Total:   total,
	}, nil
}

type userInfo struct {
	Avatar   string
	Name     string
	Username string
}

// displayNameFromProfile 组织昵称 → 用户名；绝不回落到「用户{id}」（那是内部编号，不是给人看的）
func displayNameFromProfile(name, username string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return strings.TrimSpace(username)
}

// fetchUserNames 批量获取用户展示名和头像。
// 使用独立超时上下文，避免请求尾部 deadline 把名字 RPC 掐掉后整榜变成「未知选手」并被 Redis 缓存。
// complete=true 表示每个非 0 userId 都解析到了非空展示名（可安全写榜单缓存）。
func (c ContestLogService) fetchUserNames(ctx context.Context, client profile.ProfileClient, logs []model.ContestLog) (map[int64]userInfo, bool) {
	result := map[int64]userInfo{}
	if client == nil || len(logs) == 0 {
		return result, true
	}

	idSet := map[int64]struct{}{}
	for _, v := range logs {
		if v.UserID != 0 {
			idSet[v.UserID] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return result, true
	}
	userIds := make([]int64, 0, len(idSet))
	for id := range idSet {
		userIds = append(userIds, id)
	}

	var orgID int64
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = int64(pd.OrgID)
	}

	// 脱离 HTTP 请求 cancel/deadline；仍用短超时避免挂死
	rpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fill := func(org int64) error {
		res, err := client.GetByIds(rpcCtx, &profile.GetByIdsReq{UserIds: userIds, OrgId: org})
		if err != nil {
			return err
		}
		for _, p := range res.Profiles {
			name := displayNameFromProfile(p.Name, p.Username)
			// 已有非空名不覆盖（org 优先解析，公共域仅补缺）
			if prev, ok := result[p.UserId]; ok && strings.TrimSpace(prev.Name) != "" {
				continue
			}
			result[p.UserId] = userInfo{
				Name:     name,
				Avatar:   p.Avatar,
				Username: strings.TrimSpace(p.Username),
			}
		}
		return nil
	}

	if err := fill(orgID); err != nil {
		log.Errorf("GetByIds batch failed (org=%d): %v", orgID, err)
		// 立刻重试一次
		if err2 := fill(orgID); err2 != nil {
			log.Errorf("GetByIds batch retry failed (org=%d): %v", orgID, err2)
		}
	}
	hasDisplay := func(u userInfo) bool {
		return strings.TrimSpace(u.Name) != "" || strings.TrimSpace(u.Username) != ""
	}
	// 仍有缺口：用公共域/org0 补一次（展示名至少回落到 username）
	missing := false
	for _, id := range userIds {
		if u, ok := result[id]; !ok || !hasDisplay(u) {
			missing = true
			break
		}
	}
	if missing && orgID != 0 {
		if err := fill(0); err != nil {
			log.Warnf("GetByIds public fallback failed: %v", err)
		}
	}

	complete := true
	for _, id := range userIds {
		if u, ok := result[id]; !ok || !hasDisplay(u) {
			complete = false
			break
		}
	}
	if !complete {
		log.Warnf("fetchUserNames incomplete: want=%d got=%d org=%d", len(userIds), len(result), orgID)
	}
	return result, complete
}

func (c ContestLogService) GetUserContestHistory(ctx context.Context, req *contest_log.GetUserContestHistoryReq) (*contest_log.GetUserContestHistoryRes, error) {
	logs, err := c.sbDal.GetContestByUserId(ctx, req.UserId, req.Cursor, req.Limit, req.Platform)
	if err != nil {
		return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
	}

	return &contest_log.GetUserContestHistoryRes{
		Code:    0,
		Message: "OK",
		Data:    c.contestLogsToProto(ctx, logs),
	}, nil
}

func NewContestLogService(sbDal *dal.SpiderDal, data *data.Data, reg *discovery.Register, prob *bizservice.ProblemUseCase) *ContestLogService {
	return &ContestLogService{
		sbDal: sbDal,
		db:    data.DB,
		rdb:   data.RDB,
		reg:   &reg.Reg,
		prob:  prob,
	}
}

// RegisterContestExtraRoutes 比赛题目目录 + XCPCIO 风格站内榜（手写 HTTP）。
func RegisterContestExtraRoutes(srv *khttp.Server, s *ContestLogService) {
	if srv == nil || s == nil {
		return
	}
	r := srv.Route("/")
	r.GET("/v1/core/contest/problems", s.handleContestProblems)
	r.GET("/v1/core/contest/board", s.handleContestBoard)
	// 站内榜格子：该用户本场该题的赛时提交明细
	r.GET("/v1/core/contest/cell-submits", s.handleContestCellSubmits)
}

// handleContestProblems GET ?id= 或 ?contestId=（contest_logs 行 id）
// force=1：管理员强制重跑 ensure（牛客会优先走比赛页抓题面）
// 返回题目 Tab 列表；默认每场 ensure 只成功跑一次。
func (c *ContestLogService) handleContestProblems(ctx khttp.Context) error {
	idStr := strings.TrimSpace(ctx.Query().Get("id"))
	if idStr == "" {
		idStr = strings.TrimSpace(ctx.Query().Get("contestId"))
	}
	id, _ := strconv.ParseUint(idStr, 10, 64)
	if id == 0 {
		writeContestJSON(ctx, 400, map[string]interface{}{"success": false, "message": "缺少比赛 id"})
		return nil
	}
	forceQ := strings.TrimSpace(ctx.Query().Get("force"))
	force := forceQ == "1" || strings.EqualFold(forceQ, "true")
	if force && !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		writeContestJSON(ctx, 403, map[string]interface{}{"success": false, "message": "仅管理员可强制 ensure"})
		return nil
	}
	var cl model.ContestLog
	if c.db.First(&cl, uint(id)).Error != nil {
		writeContestJSON(ctx, 404, map[string]interface{}{"success": false, "message": "比赛不存在"})
		return nil
	}

	// 先读目录；未完成则异步 ensure（避免 CF standings 阻塞 HTTP 网关）
	// 状态如实返回：failed 不再伪装成 running，避免前端永久轮询。
	list := []map[string]interface{}{}
	ensureStatus := ""
	ensureError := ""
	if c.prob != nil {
		items, st, errMsg, err := c.prob.ListContestProblems(cl.Platform, cl.ContestId)
		if err == nil {
			list = items
			ensureStatus = st
			ensureError = errMsg
		}
		// 已有目录：不再触发；done 不再触发；running 由 Ensure 内部处理超时抢占
		// failed：允许重试，但由 EnsureContestProblemsOnce 内部节流（ensured_at）
		// force=1：管理员强制重跑（忽略 done）
		needEnsure := force || (len(list) == 0 && ensureStatus != model.ContestEnsureDone)
		if needEnsure && (force || ensureStatus != model.ContestEnsureRunning) {
			plat, cid := cl.Platform, cl.ContestId
			go func() {
				var e error
				if force {
					_, e = c.prob.EnsureContestProblemsForce(plat, cid)
				} else {
					_, e = c.prob.EnsureContestProblemsOnce(plat, cid)
				}
				if e != nil {
					log.Warnf("ensure contest problems async force=%v %s/%s: %v", force, plat, cid, e)
				}
			}()
			// 仅「从未 ensure」或强制时对外显示 running；failed 保持 failed 让前端停轮询
			if force || ensureStatus == "" {
				ensureStatus = model.ContestEnsureRunning
			}
		}
	}

	writeContestJSON(ctx, 200, map[string]interface{}{
		"success": true,
		"message": "ok",
		"data": map[string]interface{}{
			"contest":      contestMapWithTimes(c.db, cl),
			"ensureStatus": ensureStatus,
			"ensureError":  ensureError,
			"list":         list,
			"force":        force,
		},
	})
	return nil
}

func contestMap(cl model.ContestLog) map[string]interface{} {
	t := int64(0)
	if !cl.Time.IsZero() {
		t = cl.Time.Unix()
	}
	m := map[string]interface{}{
		"id":          cl.ID,
		"platform":    cl.Platform,
		"userId":      cl.UserID,
		"contestId":   cl.ContestId,
		"contestName": cl.ContestName,
		"contestUrl":  cl.ContestUrl,
		"rank":        cl.Rank,
		"totalCount":  cl.TotalCount,
		"acCount":     cl.AcCount,
		"time":        t,
	}
	return m
}

// contestMapWithTimes 附带开赛/结束时间。
// 牛客：Resolve 内会拉参赛历史/比赛页实时 start+end（各场真实赛长），不写死 3h。
func contestMapWithTimes(db *gorm.DB, cl model.ContestLog) map[string]interface{} {
	m := contestMap(cl)
	// 爬虫内存 EndTime 优先塞进 Ensure（展示路径 cl 可能仍带 gorm:"-" 字段）
	if bizservice.NormalizeCalendarPlatform(cl.Platform) == "NowCoder" && cl.EndTime.After(cl.Time) {
		if s, e, ok2 := bizservice.EnsureNowCoderContestCalendar(db, cl.ContestId, cl.ContestName, cl.ContestUrl, cl.Time, cl.EndTime); ok2 {
			m["startTime"] = s.Unix()
			m["endTime"] = e.Unix()
			m["time"] = s.Unix()
			return m
		}
	}
	// 读路径用 Cached 变体：只查库 + 进程内 memo，缺日历的牛客场次异步补，绝不同步打 OJ
	start, end, ok := bizservice.ResolveContestDisplayWindowCached(db, cl.Platform, cl.ContestId, cl.Time)
	if ok {
		m["startTime"] = start.Unix()
		m["endTime"] = end.Unix()
		m["time"] = start.Unix()
	}
	return m
}

// handleContestBoard GET ?id=|contestId= contest_logs 行 id
// 返回 XCPCIO 风格：problems[] + rows[{cells}]；组织成员过滤与 ranking 一致。
//
// 只读快照；补题状态直接从已有 submit_logs 推导，不触发爬虫：
//  1. Redis 整包缓存（~90s，随 contest list global ver 失效）——热路径不扫库
//  2. 回源：contest_logs + contest_problems + contest_user_problems（已入库格子）
//     题目 ensure 走 /contest/problems；题级明细由爬虫同步写入。
func (c *ContestLogService) handleContestBoard(ctx khttp.Context) error {
	idStr := strings.TrimSpace(ctx.Query().Get("id"))
	if idStr == "" {
		idStr = strings.TrimSpace(ctx.Query().Get("contestId"))
	}
	id, _ := strconv.ParseUint(idStr, 10, 64)
	if id == 0 {
		writeContestJSON(ctx, 400, map[string]interface{}{"success": false, "message": "缺少比赛 id"})
		return nil
	}
	var seed model.ContestLog
	if c.db.First(&seed, uint(id)).Error != nil {
		writeContestJSON(ctx, 404, map[string]interface{}{"success": false, "message": "比赛不存在"})
		return nil
	}

	// 组织成员范围（与 list userId=-1 一致）
	var memberIDs []int64
	var resolvedOrg uint
	if ids, orgID, unrestricted, err := ResolveOrgMemberIDs(ctx, c.reg, 0, false); err == nil {
		resolvedOrg = orgID
		if !unrestricted {
			memberIDs = ids
		}
	}

	// followingOnly：与 ranking 一致
	followingOnly := strings.EqualFold(ctx.Query().Get("followingOnly"), "true") ||
		ctx.Query().Get("followingOnly") == "1"
	viewerID := int64(0)
	if followingOnly {
		viewerID = int64(auth.GetCurrentUserId(ctx))
		if viewerID == 0 {
			memberIDs = []int64{}
		} else {
			following := fetchFollowingIDs(ctx, c.reg, viewerID)
			if memberIDs == nil {
				memberIDs = following
			} else {
				memberIDs = intersectIDs(memberIDs, following)
			}
		}
	}

	// groupId / squadId 可选（分队优先）
	var groupID, squadID int64
	if sStr := strings.TrimSpace(ctx.Query().Get("squadId")); sStr != "" {
		if sid, err := strconv.ParseInt(sStr, 10, 64); err == nil {
			squadID = sid
		}
	}
	if gStr := strings.TrimSpace(ctx.Query().Get("groupId")); gStr != "" {
		if gid, err := strconv.ParseInt(gStr, 10, 64); err == nil {
			groupID = gid
		}
	}
	if squadID > 0 || groupID > 0 {
		if cli, err := userrpc.ProfileClient(c.reg); err == nil {
			// 用当前 JWT org；0 回落
			var orgID int64
			if pd := auth.GetCurrentUser(ctx); pd != nil {
				orgID = int64(pd.OrgID)
			}
			if res, err := cli.GetUserIdsByOrg(ctx, &profile.GetUserIdsByOrgReq{
				OrgId: orgID, GroupId: groupID, SquadId: squadID,
			}); err == nil {
				if memberIDs == nil {
					memberIDs = res.UserIds
				} else {
					gset := map[int64]struct{}{}
					for _, u := range res.UserIds {
						gset[u] = struct{}{}
					}
					var inter []int64
					for _, u := range memberIDs {
						if _, ok := gset[u]; ok {
							inter = append(inter, u)
						}
					}
					memberIDs = inter
				}
			}
		}
	}

	// --- Redis 整包 JSON 缓存（following 个性化不缓存）---
	boardCacheKey := ""
	reqCtx := context.Background()
	if c.rdb != nil && !followingOnly {
		ver := "0"
		if v, e := c.rdb.Get(reqCtx, "core:contest:list:global:ver").Result(); e == nil && v != "" {
			ver = v
		}
		scope := fmt.Sprintf("org%d", resolvedOrg)
		if squadID > 0 {
			scope = fmt.Sprintf("org%d:s%d", resolvedOrg, squadID)
		} else if groupID > 0 {
			scope = fmt.Sprintf("org%d:g%d", resolvedOrg, groupID)
		} else if memberIDs != nil {
			scope = fmt.Sprintf("org%d:n%d", resolvedOrg, len(memberIDs))
		}
		// v6：名字 RPC 失败时不写缓存，避免整榜「未知选手」被 90s 固化
		boardCacheKey = fmt.Sprintf("core:contest:board:v6:%s:%s:%s:v%s",
			seed.Platform, seed.ContestId, scope, ver)
		if b, e := c.rdb.Get(reqCtx, boardCacheKey).Bytes(); e == nil && len(b) > 0 {
			var cached map[string]interface{}
			if json.Unmarshal(b, &cached) == nil && cached != nil && boardCacheNamesOK(cached) {
				writeContestJSON(ctx, 200, cached)
				return nil
			}
		}
	}

	// 本场全部站内参赛行
	var logs []model.ContestLog
	q := c.db.Where("platform = ? AND contest_id = ?", seed.Platform, seed.ContestId)
	if memberIDs != nil {
		if len(memberIDs) == 0 {
			logs = nil
		} else {
			q = q.Where("user_id IN ?", memberIDs)
		}
	}
	if memberIDs == nil || len(memberIDs) > 0 {
		_ = q.Order("CASE WHEN rank > 0 THEN 0 ELSE 1 END, rank ASC, ac_count DESC, id ASC").Find(&logs).Error
	}
	contestantIDs := map[int64]bool{}
	seenUserIDs := map[int64]bool{}
	for _, l := range logs {
		contestantIDs[l.UserID] = true
		seenUserIDs[l.UserID] = true
	}
	practiceCells, pErr := bizservice.ListContestPracticeCells(c.db, seed.Platform, seed.ContestId, memberIDs, seed.Time)
	if pErr != nil {
		log.Warnf("contest board practice cells %s/%s: %v", seed.Platform, seed.ContestId, pErr)
	}
	// 只有补题、赛时未参赛的用户追加为零分行；原赛时顺序完全不动。
	for _, cell := range practiceCells {
		if !seenUserIDs[cell.UserID] {
			seenUserIDs[cell.UserID] = true
			contestantIDs[cell.UserID] = false
			logs = append(logs, model.ContestLog{Platform: seed.Platform, ContestId: seed.ContestId, UserID: cell.UserID, ContestName: seed.ContestName, Time: seed.Time})
		}
	}

	// 题目目录：只读表内已有目录（ensure 由 /contest/problems 负责，榜单不触发）
	problems := []map[string]interface{}{}
	if c.prob != nil {
		items, _, _, err := c.prob.ListContestProblems(seed.Platform, seed.ContestId)
		if err == nil {
			problems = items
		}
	}

	// 题级格子：只读 contest_user_problems（爬虫同步已入库），不扫 submit、不异步 Infer
	userIDs := make([]int64, 0, len(logs))
	for _, l := range logs {
		userIDs = append(userIDs, l.UserID)
	}
	cellsByUser := map[int64][]model.ContestUserProblem{}
	if len(userIDs) > 0 {
		var cells []model.ContestUserProblem
		_ = c.db.Where("platform = ? AND contest_id = ? AND user_id IN ?",
			seed.Platform, seed.ContestId, userIDs).Find(&cells).Error
		for _, cell := range cells {
			cellsByUser[cell.UserID] = append(cellsByUser[cell.UserID], cell)
		}
	}
	// submit_logs 是补题真源：覆盖旧补题/脏赛时快照。
	// ListContestPracticeCells 仅在「赛时窗内无 AC」时产出 UPSOLVE/UPSOLVE_TRIED，
	// 故可安全覆盖库内误标的 AC（例如 AtCoder 未参赛用户赛后提交曾被写成 AC）。
	for _, cell := range practiceCells {
		list := cellsByUser[cell.UserID]
		replaced := false
		for i := range list {
			if strings.EqualFold(list[i].ExternalID, cell.ExternalID) {
				list[i] = cell
				replaced = true
				break
			}
		}
		if !replaced {
			list = append(list, cell)
		}
		cellsByUser[cell.UserID] = list
	}

	// 用户资料
	var userClient profile.ProfileClient
	if cli, err := userrpc.ProfileClient(c.reg); err == nil {
		userClient = cli
	}
	nameMap, namesComplete := c.fetchUserNames(ctx, userClient, logs)

	scoring := "icpc"
	if seed.Platform == "LeetCode" {
		scoring = "leetcode"
	}
	const penaltyPerWrong = 20 * 60 // ICPC 默认 20min

	// 本地排名：官方 rank 优先；全 0 则按 solved/penalty
	allZero := true
	for _, l := range logs {
		if l.Rank > 0 {
			allZero = false
			break
		}
	}

	type rowDraft struct {
		log          model.ContestLog
		solved       int
		penalty      int
		score        int
		hasDetail    bool
		cellMaps     []map[string]interface{}
		isContestant bool
		// 仅用于「纯补题」展示排序：补题 AC 数、最后一次补题通过时间（unix）
		upsolveSolved int
		upsolveLastAt int64
	}
	// 全场是否有任意逐题明细（含补题；无则前端只展示 AC 题数，不画空格子）
	boardHasDetail := false
	for _, cells := range cellsByUser {
		for _, cell := range cells {
			if model.ContestCellHasDetail(cell.Status) {
				boardHasDetail = true
				break
			}
		}
		if boardHasDetail {
			break
		}
	}

	drafts := make([]rowDraft, 0, len(logs))
	for _, l := range logs {
		userCells := cellsByUser[l.UserID]
		byLabel := map[string]model.ContestUserProblem{}
		byExt := map[string]model.ContestUserProblem{}
		rowHasDetail := false
		for _, cell := range userCells {
			byExt[cell.ExternalID] = cell
			if cell.Label != "" {
				byLabel[cell.Label] = cell
			}
			if model.ContestCellHasDetail(cell.Status) {
				rowHasDetail = true
			}
		}
		cellMaps := make([]map[string]interface{}, 0, len(problems))
		solved := 0
		penalty := 0
		score := 0
		upsolveSolved := 0
		var upsolveLastAt int64
		if scoring == "leetcode" {
			score = l.AcCount // 力扣 ac_count 存 score
		}
		// 有逐题明细时才铺格子；否则只靠场级 AC
		if boardHasDetail && rowHasDetail {
			if len(problems) == 0 {
				for _, cell := range userCells {
					cm := cellToMap(cell)
					cellMaps = append(cellMaps, cm)
					if cell.Status == model.ContestCellAC {
						solved++
						if scoring == "icpc" {
							if cell.RelativeSec != nil {
								penalty += *cell.RelativeSec + cell.Attempts*penaltyPerWrong
							} else {
								penalty += cell.Attempts * penaltyPerWrong
							}
						}
					}
					if cell.Status == model.ContestCellUpsolve {
						upsolveSolved++
						if cell.FirstACAt != nil {
							if t := cell.FirstACAt.Unix(); t > upsolveLastAt {
								upsolveLastAt = t
							}
						}
					}
				}
			} else {
				for _, p := range problems {
					label, _ := p["label"].(string)
					ext, _ := p["externalId"].(string)
					if ext == "" {
						ext, _ = p["external_id"].(string)
					}
					cell, ok := byExt[ext]
					if !ok {
						cell, ok = byLabel[label]
					}
					if !ok {
						cellMaps = append(cellMaps, map[string]interface{}{
							"label":    label,
							"status":   model.ContestCellNone,
							"attempts": 0,
						})
						continue
					}
					cm := cellToMap(cell)
					if cm["label"] == "" {
						cm["label"] = label
					}
					cellMaps = append(cellMaps, cm)
					if cell.Status == model.ContestCellAC {
						solved++
						if scoring == "icpc" {
							if cell.RelativeSec != nil {
								penalty += *cell.RelativeSec + cell.Attempts*penaltyPerWrong
							} else {
								penalty += cell.Attempts * penaltyPerWrong
							}
						} else {
							score += cell.ScoreDelta
						}
					}
					if cell.Status == model.ContestCellUpsolve {
						upsolveSolved++
						if cell.FirstACAt != nil {
							if t := cell.FirstACAt.Unix(); t > upsolveLastAt {
								upsolveLastAt = t
							}
						}
					}
				}
			}
		} else {
			// 无题列铺格时仍统计补题 AC，供纯补题用户展示排序
			for _, cell := range userCells {
				if cell.Status == model.ContestCellUpsolve {
					upsolveSolved++
					if cell.FirstACAt != nil {
						if t := cell.FirstACAt.Unix(); t > upsolveLastAt {
							upsolveLastAt = t
						}
					}
				}
			}
		}
		// 无格子 / 无明细时用场级 ac_count
		if solved == 0 && l.AcCount > 0 {
			if scoring == "icpc" {
				solved = l.AcCount
			}
		}
		drafts = append(drafts, rowDraft{
			log: l, solved: solved, penalty: penalty, score: score,
			hasDetail: rowHasDetail, cellMaps: cellMaps, isContestant: contestantIDs[l.UserID],
			upsolveSolved: upsolveSolved, upsolveLastAt: upsolveLastAt,
		})
	}

	if allZero && scoring == "icpc" {
		// 赛时本地序：按 solved desc, penalty asc（仅赛时选手之间）
		sort.SliceStable(drafts, func(i, j int) bool {
			a, b := drafts[i], drafts[j]
			if a.isContestant != b.isContestant {
				return a.isContestant
			}
			if !a.isContestant {
				return false
			}
			if a.solved != b.solved {
				return a.solved > b.solved
			}
			return a.penalty < b.penalty
		})
	}

	// 展示序（非名次）：赛时选手保持上方原序；仅补题用户按补题 AC 数 desc，
	// 相同则谁最后一次补题通过更早谁在前（无时间的排后）。
	sort.SliceStable(drafts, func(i, j int) bool {
		a, b := drafts[i], drafts[j]
		if a.isContestant != b.isContestant {
			return a.isContestant
		}
		if a.isContestant {
			return false // 稳定保持赛时序
		}
		if a.upsolveSolved != b.upsolveSolved {
			return a.upsolveSolved > b.upsolveSolved
		}
		if a.upsolveLastAt != b.upsolveLastAt {
			if a.upsolveLastAt == 0 {
				return false
			}
			if b.upsolveLastAt == 0 {
				return true
			}
			return a.upsolveLastAt < b.upsolveLastAt
		}
		return a.log.UserID < b.log.UserID
	})

	rows := make([]map[string]interface{}, 0, len(drafts))
	// 名次：赛时选手仍用官方 rank / 本地 1..n；仅补题用户不赋展示名次（前端显示 —）
	contestantOrd := 0
	for _, d := range drafts {
		u := nameMap[d.log.UserID]
		rankOff := d.log.Rank
		rankLocal := rankOff
		if d.isContestant {
			contestantOrd++
			if rankLocal <= 0 {
				rankLocal = contestantOrd
			}
		} else {
			// 纯补题：不参与名次编号
			rankOff = 0
			rankLocal = 0
		}
		// 无明细时不铺空格子，避免「AC 了 6 题但格子全空」的错觉
		cells := d.cellMaps
		if !boardHasDetail || !d.hasDetail {
			cells = []map[string]interface{}{}
		}
		displayName := strings.TrimSpace(u.Name)
		if displayName == "" {
			displayName = strings.TrimSpace(u.Username)
		}
		row := map[string]interface{}{
			"userId":       d.log.UserID,
			"name":         displayName,
			"username":     u.Username,
			"avatar":       u.Avatar,
			"rankOfficial": rankOff,
			"rankLocal":    rankLocal,
			"solved":       d.solved,
			"penaltySec":   d.penalty,
			"score":        d.score,
			"acCount":      d.log.AcCount,
			"hasDetail":    d.hasDetail,
			"isContestant": d.isContestant,
			"cells":        cells,
		}
		rows = append(rows, row)
	}

	// problems 规范化；全场无明细时不返回题列（前端只显示 AC 题数）
	probOut := make([]map[string]interface{}, 0, len(problems))
	if boardHasDetail {
		for _, p := range problems {
			label, _ := p["label"].(string)
			ext, _ := p["externalId"].(string)
			if ext == "" {
				ext, _ = p["external_id"].(string)
			}
			title, _ := p["title"].(string)
			probOut = append(probOut, map[string]interface{}{
				"label":      label,
				"externalId": ext,
				"title":      title,
			})
		}
	}

	resp := map[string]interface{}{
		"success": true,
		"message": "ok",
		"data": map[string]interface{}{
			"contest":       contestMapWithTimes(c.db, seed),
			"scoring":       scoring,
			"hasCellDetail": boardHasDetail,
			"problems":      probOut,
			"rows":          rows,
			"total":         len(rows),
		},
	}
	// 只读快照，统一缓存 90s；名字未齐时不写缓存，避免「未知选手」被固化
	if boardCacheKey != "" && c.rdb != nil && namesComplete {
		if b, e := json.Marshal(resp); e == nil {
			_ = c.rdb.Set(reqCtx, boardCacheKey, b, 90*time.Second).Err()
		}
	}
	writeContestJSON(ctx, 200, resp)
	return nil
}

func cellToMap(cell model.ContestUserProblem) map[string]interface{} {
	m := map[string]interface{}{
		"label":      cell.Label,
		"externalId": cell.ExternalID,
		"status":     cell.Status,
		"attempts":   cell.Attempts,
		"scoreDelta": cell.ScoreDelta,
	}
	if cell.RelativeSec != nil {
		m["relativeSec"] = *cell.RelativeSec
	}
	if cell.FirstACAt != nil {
		m["firstAcAt"] = cell.FirstACAt.Unix()
	}
	return m
}

// handleContestCellSubmits GET ?id=|contestId=&userId=&label=&externalId=
// 返回该用户在本场该题的提交列表（赛时 + 赛后补题，phase 区分；供站内榜格子弹窗）。
func (c *ContestLogService) handleContestCellSubmits(ctx khttp.Context) error {
	idStr := strings.TrimSpace(ctx.Query().Get("id"))
	if idStr == "" {
		idStr = strings.TrimSpace(ctx.Query().Get("contestId"))
	}
	id, _ := strconv.ParseUint(idStr, 10, 64)
	userID, _ := strconv.ParseInt(strings.TrimSpace(ctx.Query().Get("userId")), 10, 64)
	label := strings.TrimSpace(ctx.Query().Get("label"))
	externalID := strings.TrimSpace(ctx.Query().Get("externalId"))
	if externalID == "" {
		externalID = strings.TrimSpace(ctx.Query().Get("external_id"))
	}
	if id == 0 || userID == 0 {
		writeContestJSON(ctx, 400, map[string]interface{}{
			"success": false,
			"message": "缺少比赛 id 或 userId",
		})
		return nil
	}
	if label == "" && externalID == "" {
		writeContestJSON(ctx, 400, map[string]interface{}{
			"success": false,
			"message": "缺少题目 label 或 externalId",
		})
		return nil
	}

	var seed model.ContestLog
	if c.db.First(&seed, uint(id)).Error != nil {
		writeContestJSON(ctx, 404, map[string]interface{}{
			"success": false,
			"message": "比赛不存在",
		})
		return nil
	}

	list, start, end, err := bizservice.ListContestCellSubmits(
		c.db, seed.Platform, seed.ContestId, userID, label, externalID, seed.Time,
	)
	if err != nil {
		log.Warnf("cell-submits %s/%s u=%d: %v", seed.Platform, seed.ContestId, userID, err)
		writeContestJSON(ctx, 500, map[string]interface{}{
			"success": false,
			"message": "加载提交记录失败",
		})
		return nil
	}

	// 展示名
	userName := ""
	if cli, e := userrpc.ProfileClient(c.reg); e == nil && cli != nil {
		var orgID int64
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			orgID = int64(pd.OrgID)
		}
		if res, e2 := cli.GetByIds(ctx, &profile.GetByIdsReq{
			UserIds: []int64{userID},
			OrgId:   orgID,
		}); e2 == nil && res != nil {
			for _, p := range res.Profiles {
				if p.UserId == userID {
					userName = displayNameFromProfile(p.Name, p.Username)
					break
				}
			}
		}
	}

	items := make([]map[string]interface{}, 0, len(list))
	for _, s := range list {
		// 原站代码链接需要 contest 字段；提交表缺省时用本场 contest_id
		contestField := strings.TrimSpace(s.Contest)
		if contestField == "" || contestField == "leetcode" {
			contestField = seed.ContestId
		}
		phase := s.Phase
		if phase == "" {
			phase = bizservice.CellSubmitPhaseContest
		}
		m := map[string]interface{}{
			"id":         s.ID,
			"submitId":   model.NormalizeSubmitID(seed.Platform, s.SubmitID),
			"status":     s.Status,
			"lang":       s.Lang,
			"time":       s.Time.Unix(),
			"phase":      phase, // contest | upsolve
			"problem":    s.Problem,
			"contest":    contestField,
			"externalId": s.ExternalID,
			"platform":   seed.Platform,
		}
		if s.RelativeSec != nil {
			m["relativeSec"] = *s.RelativeSec
		}
		if s.ProblemID != nil && *s.ProblemID > 0 {
			m["problemId"] = *s.ProblemID
		}
		items = append(items, m)
	}

	// 若请求 label 为空，用目录/首条补全
	outLabel := label
	outExt := externalID
	if outLabel == "" && outExt != "" {
		// 从目录反查
		if c.prob != nil {
			if probs, _, _, e := c.prob.ListContestProblems(seed.Platform, seed.ContestId); e == nil {
				for _, p := range probs {
					ext, _ := p["externalId"].(string)
					if ext == "" {
						ext, _ = p["external_id"].(string)
					}
					if strings.EqualFold(ext, outExt) {
						if lb, ok := p["label"].(string); ok {
							outLabel = lb
						}
						break
					}
				}
			}
		}
	}
	if outExt == "" && len(list) > 0 {
		outExt = list[0].ExternalID
	}

	data := map[string]interface{}{
		"contest":    contestMapWithTimes(c.db, seed),
		"platform":   seed.Platform,
		"contestId":  seed.ContestId,
		"userId":     userID,
		"userName":   userName,
		"label":      outLabel,
		"externalId": outExt,
		"list":       items,
		"total":      len(items),
	}
	if !start.IsZero() {
		data["startTime"] = start.Unix()
	}
	if !end.IsZero() {
		data["endTime"] = end.Unix()
	}

	writeContestJSON(ctx, 200, map[string]interface{}{
		"success": true,
		"message": "ok",
		"data":    data,
	})
	return nil
}

func writeContestJSON(ctx khttp.Context, status int, v interface{}) {
	w := ctx.Response()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// boardCacheNamesOK 缓存里若出现 userId>0 且 name 为空，视为脏缓存（GetByIds 失败时写入的旧数据）
func boardCacheNamesOK(cached map[string]interface{}) bool {
	data, _ := cached["data"].(map[string]interface{})
	if data == nil {
		return true
	}
	rows, _ := data["rows"].([]interface{})
	for _, raw := range rows {
		row, _ := raw.(map[string]interface{})
		if row == nil {
			continue
		}
		uid := int64(0)
		switch v := row["userId"].(type) {
		case float64:
			uid = int64(v)
		case int64:
			uid = v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				uid = n
			}
		}
		if uid <= 0 {
			continue
		}
		name, _ := row["name"].(string)
		if strings.TrimSpace(name) == "" {
			return false
		}
	}
	return true
}
