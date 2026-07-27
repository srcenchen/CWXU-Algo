package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cwxu-algo/api/core/v1/statistic"
	data2 "cwxu-algo/app/common/data"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/core_data/internal/data/dal"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/redis/go-redis/v9"
)

// StatisticUseCase 统计业务逻辑层
type StatisticUseCase struct {
	dal *dal.StatisticDal
	rdb *redis.Client
	reg *registry.Registrar
}

// NewStatisticUseCase 创建统计业务逻辑层
func NewStatisticUseCase(dal *dal.StatisticDal, rdb *redis.Client, reg *discovery.Register) *StatisticUseCase {
	var r *registry.Registrar
	if reg != nil {
		r = &reg.Reg
	}
	return &StatisticUseCase{dal: dal, rdb: rdb, reg: r}
}

// resolveMembers 当前组织成员；siteWide=true 时返回 nil（全站不限制，公开聚合）
func (uc *StatisticUseCase) resolveMembers(ctx context.Context, siteWide bool) (memberIDs []int64, orgID uint) {
	return uc.resolveMembersScoped(ctx, siteWide, 0, 0)
}

// resolveMembersScoped 支持 groupId / squadId 缩小成员集合
func (uc *StatisticUseCase) resolveMembersScoped(ctx context.Context, siteWide bool, groupID, squadID int64) (memberIDs []int64, orgID uint) {
	if siteWide {
		return nil, 0
	}
	ids, resolvedOrgID, err := fetchOrgMemberIDsScoped(ctx, uc.reg, 0, groupID, squadID)
	if err != nil {
		log.Warnf("statistic org members: %v", err)
		return []int64{}, resolvedOrgID
	}
	return ids, resolvedOrgID
}

// userId 约定：个人>0；0=组织热力；-1=组织时段；-2=全站时段/热力（公开聚合，无需站管）
func isSiteWideUserId(userId int64) bool {
	return userId == -2
}

func (uc *StatisticUseCase) redisVer(ctx context.Context, key string) string {
	if v, err := uc.rdb.Get(ctx, key).Result(); err == nil && v != "" {
		return v
	}
	return "0"
}

// 热力跨度上限：个人可拉生涯（稀疏日行）；组织/全站仍约 13 个月防大聚合
const (
	heatmapMaxDaysPersonal  = 365 * 20 // ~20 年
	heatmapMaxDaysAggregate = 400
	// heatmapCacheSchema 稀疏日行 + 个人稳定 career key；改序列化/语义时 bump
	// s3：个人 isAc 热力改走 user_ac_problem_days 去重（排除力扣官方 ac-*）
	heatmapCacheSchema = "3"
)

// clampHeatmapRange 规范化并限制日期跨度；入参支持 20060102 / 2006-01-02
func clampHeatmapRange(startS, endS string, maxDays int) (start, end string, err error) {
	parse := func(s string) (time.Time, error) {
		if t, e := time.ParseInLocation("2006-01-02", s, time.Local); e == nil {
			return t, nil
		}
		return time.ParseInLocation("20060102", s, time.Local)
	}
	st, e1 := parse(startS)
	en, e2 := parse(endS)
	if e1 != nil || e2 != nil {
		return "", "", errors.BadRequest("参数错误", "日期格式错误")
	}
	if en.Before(st) {
		st, en = en, st
	}
	if maxDays < 1 {
		maxDays = heatmapMaxDaysAggregate
	}
	if int(en.Sub(st).Hours()/24) > maxDays {
		st = en.AddDate(0, 0, -maxDays)
	}
	return st.Format("2006-01-02"), en.Format("2006-01-02"), nil
}

// personalHeatmapCareerRange 个人缓存固定窗口：今天往前 ~20 年（与 end 日期解耦，避免每日 cache key 漂移）
func personalHeatmapCareerRange() (start, end string) {
	now := time.Now()
	endT := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	startT := endT.AddDate(0, 0, -heatmapMaxDaysPersonal)
	return startT.Format("2006-01-02"), endT.Format("2006-01-02")
}

// filterDailyCountsInRange 从已排序/未排序的稀疏日行中裁剪 [start,end]（含）
func filterDailyCountsInRange(rows []dal.DailyCount, start, end string) []dal.DailyCount {
	if len(rows) == 0 || (start == "" && end == "") {
		return rows
	}
	st, e1 := time.ParseInLocation("2006-01-02", start, time.Local)
	en, e2 := time.ParseInLocation("2006-01-02", end, time.Local)
	if e1 != nil || e2 != nil {
		return rows
	}
	out := make([]dal.DailyCount, 0, len(rows))
	for _, r := range rows {
		d := time.Date(r.Day.Year(), r.Day.Month(), r.Day.Day(), 0, 0, 0, 0, time.Local)
		if d.Before(st) || d.After(en) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Heatmap 获取热力图数据
//
// 缓存与即时性：
//   - 个人 userId>0：稳定 key（schema+user+isAc+userVer），爬虫写入后 INCR statistic:user:{id}:ver 即 miss 回源；
//     不把 start/end 写进 key，避免「每天 end=今天」导致日更 miss / 旧 ver 键膨胀。
//   - 组织/全站：短 TTL + global ver（爬虫侧有 2min 节流，可接受略延迟）。
//   - 回源只查稀疏日行（daily_user_stats），多年生涯可扛。
func (uc *StatisticUseCase) Heatmap(ctx context.Context, req *statistic.HeatmapReq) (*statistic.HeatmapResp, error) {
	if req.StartDate == "" || req.EndDate == "" {
		return nil, errors.BadRequest("参数错误", "日期参数错误")
	}

	var memberIDs []int64
	queryUserId := req.UserId
	ttl := data2.DefaultCacheTTL
	var cacheKey string
	// 实际回源区间（个人=生涯全窗；聚合=请求 clamp）
	var queryStart, queryEnd string
	// 响应对客户端请求的裁剪区间
	var respStart, respEnd string

	if req.UserId > 0 {
		maxDays := heatmapMaxDaysPersonal
		respStart, respEnd, err := clampHeatmapRange(req.StartDate, req.EndDate, maxDays)
		if err != nil {
			return nil, err
		}
		// 固定生涯窗回源 + 稳定缓存；再按请求区间裁剪返回
		queryStart, queryEnd = personalHeatmapCareerRange()
		userVer := uc.redisVer(ctx, fmt.Sprintf("statistic:user:%d:ver", req.UserId))
		cacheKey = fmt.Sprintf("statistic:heatmap:s%s:u%d:%t:v%s", heatmapCacheSchema, req.UserId, req.IsAc, userVer)
		// 访问热度：供爬虫后预热 heatmap/period
		TouchUserHeat(ctx, uc.rdb, req.UserId)

		result, _, err := data2.GetCacheDalTTL[[]dal.DailyCount](ctx, uc.rdb, cacheKey, ttl, func(data *[]dal.DailyCount) error {
			rows, err := uc.dal.HeatmapQueryScoped(ctx, queryStart, queryEnd, req.UserId, req.IsAc, nil)
			if err != nil {
				return err
			}
			*data = rows
			return nil
		})
		if err != nil {
			return nil, errors.InternalServer("内部错误", err.Error())
		}
		rows := filterDailyCountsInRange(*result, respStart, respEnd)
		return heatmapRespFromRows(rows), nil
	}

	// 组织 / 全站 / 其它
	respStart, respEnd, err := clampHeatmapRange(req.StartDate, req.EndDate, heatmapMaxDaysAggregate)
	if err != nil {
		return nil, err
	}
	queryStart, queryEnd = respStart, respEnd

	if req.UserId == 0 || isSiteWideUserId(req.UserId) {
		siteWide := isSiteWideUserId(req.UserId)
		var resolvedOrgID uint
		memberIDs, resolvedOrgID = uc.resolveMembersScoped(ctx, siteWide, req.GetGroupId(), req.GetSquadId())
		queryUserId = 0
		globalVer := uc.redisVer(ctx, "statistic:heatmap:global:ver")
		ttl = data2.OrgStatsCacheTTL // 短 TTL，补 global ver 节流下的即时性
		if siteWide {
			cacheKey = fmt.Sprintf("statistic:heatmap:s%s:site:%s:%s:%t:v%s", heatmapCacheSchema, queryStart, queryEnd, req.IsAc, globalVer)
		} else {
			cacheKey = fmt.Sprintf("statistic:heatmap:s%s:org%d:%s:%s:%t:v%s", heatmapCacheSchema, resolvedOrgID, queryStart, queryEnd, req.IsAc, globalVer)
		}
	} else {
		globalVer := uc.redisVer(ctx, "statistic:heatmap:global:ver")
		cacheKey = fmt.Sprintf("statistic:heatmap:s%s:%d:%s:%s:%t:v%s", heatmapCacheSchema, req.UserId, queryStart, queryEnd, req.IsAc, globalVer)
	}

	result, _, err := data2.GetCacheDalTTL[[]dal.DailyCount](ctx, uc.rdb, cacheKey, ttl, func(data *[]dal.DailyCount) error {
		rows, err := uc.dal.HeatmapQueryScoped(ctx, queryStart, queryEnd, queryUserId, req.IsAc, memberIDs)
		if err != nil {
			return err
		}
		*data = rows
		return nil
	})
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return heatmapRespFromRows(*result), nil
}

func heatmapRespFromRows(rows []dal.DailyCount) *statistic.HeatmapResp {
	items := make([]*statistic.HeatmapResp_HeatmapItem, len(rows))
	for i, v := range rows {
		items[i] = &statistic.HeatmapResp_HeatmapItem{
			Date:  v.Day.Format("2006-01-02"),
			Count: v.Cnt,
		}
	}
	return &statistic.HeatmapResp{Code: 0, Data: items}
}

// Rank 按日期区间获取排行
func (uc *StatisticUseCase) Rank(ctx context.Context, req *statistic.RankReq) (*statistic.RankResp, error) {
	if req.StartDate == "" || req.EndDate == "" {
		return nil, errors.BadRequest("参数错误", "日期参数错误")
	}
	start, err := time.ParseInLocation("2006-01-02", req.StartDate, time.Local)
	if err != nil {
		return nil, errors.BadRequest("参数错误", "startDate 格式应为 YYYY-MM-DD")
	}
	end, err := time.ParseInLocation("2006-01-02", req.EndDate, time.Local)
	if err != nil {
		return nil, errors.BadRequest("参数错误", "endDate 格式应为 YYYY-MM-DD")
	}
	endExclusive := end.AddDate(0, 0, 1)

	scoreType := req.ScoreType
	if scoreType == "" {
		scoreType = "submit"
	}
	groupId := req.GetGroupId()
	squadId := req.GetSquadId()
	// 成员集合已按 group/squad 过滤；预聚合表无 group 列，DAL 传 -1
	memberIDs, orgID := uc.resolveMembersScoped(ctx, false, groupId, squadId)
	groupId = -1
	// 公共域 / 未登录回落公共域：全站聚合（nil），避免对全体成员做巨型 IN，
	// 也避免 resolve 失败时空列表导致「无法加载」。私有域仍按成员隔离。
	if isPublicOrgID(ctx, uc.reg, orgID) {
		memberIDs = nil
	}

	// TopN 首页快照：page=1、无 group、pageSize≤50
	useSnap := req.Page <= 1 && groupId < 0 && req.PageSize > 0 && req.PageSize <= 50 && uc.rdb != nil
	globalVer := uc.redisVer(ctx, "statistic:period:global:ver")
	snapKey := ""
	if useSnap {
		scope := "org"
		if memberIDs == nil {
			scope = "all"
		} else if orgID == 0 {
			scope = "all"
		} else {
			scope = fmt.Sprintf("org%d", orgID)
		}
		// schema s3：生涯过题力扣优先官方 acTotal 合成键
		snapKey = fmt.Sprintf("rank:snap:s3:%s:%s:%s_%s:v%s:ps%d",
			scope, scoreType, req.StartDate, req.EndDate, globalVer, req.PageSize)
		if b, e := uc.rdb.Get(ctx, snapKey).Bytes(); e == nil && len(b) > 0 {
			var cached statistic.RankResp
			if json.Unmarshal(b, &cached) == nil && cached.Data != nil {
				cached.Code = 0
				return &cached, nil
			}
		}
	}

	items, total, err := uc.dal.GetRankByRangeScoped(ctx, start, endExclusive, scoreType, groupId, req.Page, req.PageSize, memberIDs)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}

	uids := make([]int64, 0, len(items))
	for _, v := range items {
		if v.UserID > 0 {
			uids = append(uids, v.UserID)
		}
	}
	nameMap := fetchDisplayNames(ctx, uc.reg, uids)

	data := make([]*statistic.RankItem, len(items))
	for i, v := range items {
		name := nameMap[v.UserID]
		if name == "" {
			name = v.Name
		}
		data[i] = &statistic.RankItem{
			Rank:   v.Rank,
			UserId: v.UserID,
			Name:   name,
			Score:  v.Score,
		}
	}
	resp := &statistic.RankResp{Code: 0, Data: data, Total: total}
	if useSnap && snapKey != "" {
		if b, e := json.Marshal(resp); e == nil {
			_ = uc.rdb.Set(ctx, snapKey, b, data2.OrgStatsCacheTTL).Err()
		}
	}
	return resp, nil
}

// PeriodCount 获取时间段统计数据
func (uc *StatisticUseCase) PeriodCount(ctx context.Context, req *statistic.PeriodCountReq) (*statistic.PeriodCountResp, error) {
	var memberIDs []int64
	queryUserId := req.UserId
	// schema v6：个人 AC 去重走 user_ac_problem_* 预聚合
	// s9：生涯 total 力扣优先官方 acTotal 合成键（与平台过题一致，避免 recentAC 双计）
	// s10：时段今日/本周排除 e:LeetCode:ac-*（与 lc-prob 同题双计修复）
	const periodCacheSchema = "10"
	ttl := data2.DefaultCacheTTL
	var cacheKey string

	if req.UserId > 0 {
		userVer := uc.redisVer(ctx, fmt.Sprintf("statistic:user:%d:ver", req.UserId))
		cacheKey = fmt.Sprintf("statistic:period:s%s:u%d:v%s", periodCacheSchema, req.UserId, userVer)
		// 热度：供爬虫后自主预热决策
		TouchUserHeat(ctx, uc.rdb, req.UserId)
	} else if req.UserId == -1 || isSiteWideUserId(req.UserId) {
		// -2=全站公开聚合；-1=当前组织时段
		siteWide := isSiteWideUserId(req.UserId)
		var resolvedOrgID uint
		memberIDs, resolvedOrgID = uc.resolveMembersScoped(ctx, siteWide, req.GetGroupId(), req.GetSquadId())
		queryUserId = -1
		globalVer := uc.redisVer(ctx, "statistic:period:global:ver")
		ttl = data2.OrgStatsCacheTTL
		if siteWide {
			cacheKey = fmt.Sprintf("statistic:period:s%s:site:v%s", periodCacheSchema, globalVer)
		} else {
			cacheKey = fmt.Sprintf("statistic:period:s%s:org:%d:v%s", periodCacheSchema, resolvedOrgID, globalVer)
		}
	} else {
		// 兼容 0 等：按全局
		globalVer := uc.redisVer(ctx, "statistic:period:global:ver")
		cacheKey = fmt.Sprintf("statistic:period:s%s:%d:v%s", periodCacheSchema, req.UserId, globalVer)
	}

	type PeriodCountData struct {
		Submit dal.PeriodSubmitCount
		Ac     dal.PeriodAcCount
	}

	result, _, err := data2.GetCacheDalTTL[PeriodCountData](ctx, uc.rdb, cacheKey, ttl, func(data *PeriodCountData) error {
		var err error
		data.Submit, data.Ac, err = uc.dal.GetPeriodCountScoped(queryUserId, memberIDs)
		return err
	})
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}

	return &statistic.PeriodCountResp{
		Code: 0,
		Data: &statistic.PeriodData{
			Submit: &statistic.SubmitCount{
				Today:     result.Submit.Today,
				ThisWeek:  result.Submit.ThisWeek,
				LastWeek:  result.Submit.LastWeek,
				ThisMonth: result.Submit.ThisMonth,
				LastMonth: result.Submit.LastMonth,
				ThisYear:  result.Submit.ThisYear,
				LastYear:  result.Submit.LastYear,
				Total:     result.Submit.Total,
			},
			Ac: &statistic.AcCount{
				Today:     result.Ac.Today,
				ThisWeek:  result.Ac.ThisWeek,
				LastWeek:  result.Ac.LastWeek,
				ThisMonth: result.Ac.ThisMonth,
				LastMonth: result.Ac.LastMonth,
				ThisYear:  result.Ac.ThisYear,
				LastYear:  result.Ac.LastYear,
				Total:     result.Ac.Total,
				TotalRaw:  result.Ac.TotalRaw,
			},
		},
	}, nil
}
