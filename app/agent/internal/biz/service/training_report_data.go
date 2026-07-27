package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/api/core/v1/statistic"
	"cwxu-algo/api/core/v1/submit_log"
	profile2 "cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/agent/internal/agent/tool/core_data"

	"github.com/go-kratos/kratos/v2/log"
	"golang.org/x/sync/errgroup"
	grpc2 "google.golang.org/grpc"
)

const (
	// DetailModeFull 后台训练报告详版
	DetailModeFull = "full"
	// DetailModeCompact 教练周报简版（维度不少，篇幅更短）
	DetailModeCompact = "compact"
	// MaxAIRangeDays AI 分析最长约 8 个月
	MaxAIRangeDays = 243
	// SiteBaseURL 报告内链跳主站
	SiteBaseURL = "https://algo.zhiyuansofts.cn"
	// OrgRoleCoach 组织教练（不计入统计）
	orgRoleCoach = "coach"
)

// BlogBrief 组织博客摘要（无正文）
type BlogBrief struct {
	ID      int64  `json:"id,omitempty"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Author  string `json:"author,omitempty"`
	Time    string `json:"time,omitempty"`
}

// SubmitFeedItem 组织提交动态抽样
type SubmitFeedItem struct {
	UserID   int64    `json:"userId"`
	UserName string   `json:"userName,omitempty"`
	Platform string   `json:"platform"`
	Problem  string   `json:"problem"`
	Title    string   `json:"title,omitempty"`
	Status   string   `json:"status"`
	Lang     string   `json:"lang,omitempty"`
	Time     string   `json:"time,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// ContestBrief 比赛摘要
type ContestBrief struct {
	ID          uint32 `json:"id,omitempty"`
	Platform    string `json:"platform"`
	ContestID   string `json:"contestId"`
	ContestName string `json:"contestName"`
	Rank        int32  `json:"rank,omitempty"`
	ACCount     int32  `json:"acCount"`
	TotalCount  int32  `json:"totalCount,omitempty"`
	Time        string `json:"time,omitempty"`
	UserID      int64  `json:"userId,omitempty"`
	UserName    string `json:"userName,omitempty"`
}

// ContestRankRow 单场排行行
type ContestRankRow struct {
	Rank       int64  `json:"rank"`
	UserID     int64  `json:"userId"`
	Name       string `json:"name"`
	Score      int32  `json:"score"`
	ACCount    int32  `json:"acCount"`
	TotalCount int32  `json:"totalCount"`
}

// ContestRankSnap 重点场次排行摘要
type ContestRankSnap struct {
	ContestID   string           `json:"contestId"`
	ContestName string           `json:"contestName"`
	Platform    string           `json:"platform"`
	Total       int64            `json:"total"`
	Top         []ContestRankRow `json:"top"`
}

// MemberStat 活跃成员综合行（排行榜用；不含 0 提交、不含教练）
type MemberStat struct {
	Rank     int64   `json:"rank"`
	UserID   int64   `json:"userId"`
	Name     string  `json:"name"`
	Username string  `json:"username,omitempty"`
	Submits  int64   `json:"submits"`
	AC       int64   `json:"ac"`
	ACRate   float64 `json:"acRate"` // 百分比 0-100
	Share    float64 `json:"share"`  // 提交占组织总提交 %
	ProfileURL string `json:"profileUrl,omitempty"`
}

// TagHit 团队标签计数
type TagHit struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// ProblemTouch 做题概览：谁交了啥题
type ProblemTouch struct {
	Problem    string   `json:"problem"`
	Title      string   `json:"title,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Platform   string   `json:"platform,omitempty"`
	Submitters int      `json:"submitters"`
	ACCount    int      `json:"acCount"`
	ACUsers    []string `json:"acUsers,omitempty"`
	TryUsers   []string `json:"tryUsers,omitempty"`
}

// TrainingReportData 组织/组训练报告聚合数据（规则模板与 AI 共用）
type TrainingReportData struct {
	OrgID            int64       `json:"orgId"`
	GroupID          int64       `json:"groupId"`
	SquadID          int64       `json:"squadId"`
	GroupName        string      `json:"groupName,omitempty"`
	ScopeLabel       string      `json:"scopeLabel"`
	StartDate        string      `json:"startDate"`
	EndDate          string      `json:"endDate"`
	PrevStartDate    string      `json:"prevStartDate"`
	PrevEndDate      string      `json:"prevEndDate"`
	MemberCount      int         `json:"memberCount"`
	MemberIDs        []int64     `json:"memberIds"`
	TotalSubmits     int64       `json:"totalSubmits"`
	PrevTotalSubmits int64       `json:"prevTotalSubmits"`
	TotalAC          int64       `json:"totalAc"`
	DailyTrend       []DayCount  `json:"dailyTrend"`
	DailyACTrend     []DayCount  `json:"dailyAcTrend,omitempty"`
	TopSubmit        []RankEntry `json:"topSubmit"`
	TopAC            []RankEntry `json:"topAc"`
	// ActiveRanking 全部有提交成员（按提交降序，已剔除 0 提交）
	ActiveRanking   []MemberStat `json:"activeRanking"`
	InactiveMembers []string     `json:"inactiveMembers"`
	ActiveMembers   int          `json:"activeMembers"`
	// 多维度预取（AI 与规则共用）
	TeamTags        []TagHit          `json:"teamTags,omitempty"`
	ProblemOverview []ProblemTouch    `json:"problemOverview,omitempty"`
	RecentBlogs     []BlogBrief       `json:"recentBlogs,omitempty"`
	OrgSubmitSample []SubmitFeedItem  `json:"orgSubmitSample,omitempty"`
	Contests        []ContestBrief    `json:"contests,omitempty"`
	ContestRankings []ContestRankSnap `json:"contestRankings,omitempty"`
	// Initiator 发起人（邮件）
	InitiatorUserID int64  `json:"initiatorUserId"`
	InitiatorName   string `json:"initiatorName"`
	InitiatorEmail  string `json:"initiatorEmail"`
}

// DetailModeFromSource weekly → compact，其余 full
func DetailModeFromSource(source string) string {
	if strings.EqualFold(strings.TrimSpace(source), "weekly") {
		return DetailModeCompact
	}
	return DetailModeFull
}

// ValidateAIDateRange AI 开启时跨度不得超过约 8 个月
func ValidateAIDateRange(start, end time.Time, useAI bool) error {
	if !useAI {
		return nil
	}
	days := int(end.Sub(start).Hours()/24) + 1
	if days > MaxAIRangeDays {
		return fmt.Errorf("AI 分析最长允许 %d 天（约 8 个月），当前 %d 天", MaxAIRangeDays, days)
	}
	return nil
}

// LastWeekRange 相对 now 的上一完整周：周一到周日（end=昨天若 now 为周一则上周日）
// 与既有周报一致：weekEnd = 昨天 0 点所在日，weekStart = weekEnd-6
func LastWeekRange(now time.Time) (start, end time.Time) {
	loc := now.Location()
	weekEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -1)
	weekStart := weekEnd.AddDate(0, 0, -6)
	return weekStart, weekEnd
}

// ParseDateRange 解析 YYYY-MM-DD，含首尾
func ParseDateRange(startS, endS string) (start, end time.Time, err error) {
	start, err = time.ParseInLocation(dateLayout, strings.TrimSpace(startS), time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("startDate 格式应为 YYYY-MM-DD")
	}
	end, err = time.ParseInLocation(dateLayout, strings.TrimSpace(endS), time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("endDate 格式应为 YYYY-MM-DD")
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("endDate 不能早于 startDate")
	}
	if end.Sub(start).Hours()/24 > 400 {
		return time.Time{}, time.Time{}, fmt.Errorf("日期跨度不能超过 400 天")
	}
	return start, end, nil
}

// dialUserCtx / dialCoreCtx 返回按服务名缓存的共享长连接（调用方不得 Close）。
// 整个报告生成过程复用同一条连接；鉴权 metadata 由每次 RPC 的 ctx 携带。
func (uc *SummaryUseCase) dialUserCtx(_ context.Context) (*grpc2.ClientConn, error) {
	if uc == nil || uc.reg == nil {
		return nil, fmt.Errorf("service discovery 未配置")
	}
	return core_data.SharedGRPCConn(uc.reg, "user")
}

func (uc *SummaryUseCase) dialCoreCtx(_ context.Context) (*grpc2.ClientConn, error) {
	if uc == nil || uc.reg == nil {
		return nil, fmt.Errorf("service discovery 未配置")
	}
	return core_data.SharedGRPCConn(uc.reg, "core-data")
}

// resolveMemberIDs 组织成员（排除教练），可选按组过滤。
// coachSet 由调用方一次拉取后传入，避免重复请求成员列表。
func (uc *SummaryUseCase) resolveMemberIDs(ctx context.Context, orgID, groupID, squadID int64, coachSet map[int64]struct{}) ([]int64, error) {
	conn, err := uc.dialUserCtx(ctx)
	if err != nil {
		return nil, err
	}
	cli := profile2.NewProfileClient(conn)

	// 统一走 GetUserIdsByOrg：支持 group/squad，并与 staff 范围兼容（elevated 站管不受限）
	res, err := cli.GetUserIdsByOrg(ctx, &profile2.GetUserIdsByOrgReq{
		OrgId:   orgID,
		GroupId: groupID,
		SquadId: squadID,
	})
	if err != nil {
		return nil, fmt.Errorf("取组织成员失败: %w", err)
	}
	ids := res.GetUserIds()
	if ids == nil {
		ids = []int64{}
	}
	// 排除教练（训练报告只统计队员）
	if len(coachSet) > 0 {
		out := make([]int64, 0, len(ids))
		for _, id := range ids {
			if _, isCoach := coachSet[id]; !isCoach {
				out = append(out, id)
			}
		}
		ids = out
	}
	return ids, nil
}


func (uc *SummaryUseCase) fetchCoachUserIDSet(ctx context.Context, orgID int64) map[int64]struct{} {
	out := map[int64]struct{}{}
	if orgID <= 0 || uc == nil || uc.reg == nil {
		return out
	}
	// 分页拉全员，筛 coach
	for page := 1; page <= 20; page++ {
		path := fmt.Sprintf("/v1/user/org/members?orgId=%d&page=%d&pageSize=100", orgID, page)
		body, code, err := httpDiscoveryGet(ctx, uc.reg, "user", path)
		if err != nil || code >= 400 {
			log.Warnf("fetchCoachUserIDSet org=%d page=%d: code=%d err=%v", orgID, page, code, err)
			break
		}
		var raw map[string]interface{}
		if err := jsonUnmarshal(body, &raw); err != nil {
			break
		}
		list, _ := raw["list"].([]interface{})
		if list == nil {
			if data, ok := raw["data"].(map[string]interface{}); ok {
				list, _ = data["list"].([]interface{})
			}
		}
		if len(list) == 0 {
			break
		}
		for _, it := range list {
			m, ok := it.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if strings.EqualFold(strings.TrimSpace(role), orgRoleCoach) {
				var uid int64
				switch v := m["userId"].(type) {
				case float64:
					uid = int64(v)
				case int64:
					uid = v
				}
				if uid > 0 {
					out[uid] = struct{}{}
				}
			}
		}
		total := 0
		switch v := raw["total"].(type) {
		case float64:
			total = int(v)
		}
		if page*100 >= total && total > 0 {
			break
		}
		if len(list) < 100 {
			break
		}
	}
	return out
}

type userIdentity struct {
	Name     string
	Username string
}

func (uc *SummaryUseCase) fetchNames(ctx context.Context, userIDs []int64, orgID int64) map[int64]string {
	idMap := uc.fetchIdentities(ctx, userIDs, orgID)
	out := make(map[int64]string, len(idMap))
	for id, idn := range idMap {
		if idn.Name != "" {
			out[id] = idn.Name
		} else {
			out[id] = idn.Username
		}
	}
	return out
}

func (uc *SummaryUseCase) fetchIdentities(ctx context.Context, userIDs []int64, orgID int64) map[int64]userIdentity {
	out := make(map[int64]userIdentity, len(userIDs))
	if len(userIDs) == 0 {
		return out
	}
	conn, err := uc.dialUserCtx(ctx)
	if err != nil {
		return out
	}
	cli := profile2.NewProfileClient(conn)
	const batch = 100
	for i := 0; i < len(userIDs); i += batch {
		j := i + batch
		if j > len(userIDs) {
			j = len(userIDs)
		}
		res, err := cli.GetByIds(ctx, &profile2.GetByIdsReq{UserIds: userIDs[i:j], OrgId: orgID})
		if err != nil || res == nil {
			continue
		}
		for _, p := range res.Profiles {
			if p == nil {
				continue
			}
			name := p.Name
			if name == "" {
				name = p.Username
			}
			out[p.UserId] = userIdentity{Name: name, Username: p.Username}
		}
	}
	return out
}

func profileURL(username string, userID int64) string {
	u := strings.TrimSpace(username)
	if u != "" {
		return SiteBaseURL + "/profile/" + u
	}
	if userID > 0 {
		return SiteBaseURL + "/profile"
	}
	return SiteBaseURL
}

func (uc *SummaryUseCase) fetchHeatmapUser(ctx context.Context, userId int64, start, end time.Time, isAC bool) ([]DayCount, error) {
	conn, err := uc.dialCoreCtx(ctx)
	if err != nil {
		return nil, err
	}
	cli := statistic.NewStatisticClient(conn)
	res, err := cli.Heatmap(ctx, &statistic.HeatmapReq{
		UserId:    userId,
		StartDate: start.Format(dateLayout),
		EndDate:   end.Format(dateLayout),
		IsAc:      isAC,
	})
	if err != nil {
		return nil, err
	}
	var items []*statistic.HeatmapResp_HeatmapItem
	if res != nil {
		items = res.Data
	}
	return fillMissingDays(start, end, items), nil
}

// LoadTrainingReportData 拉取组织/组在日期区间的训练数据（排除教练）
func (uc *SummaryUseCase) LoadTrainingReportData(ctx context.Context, orgID, groupID, squadID, initiatorID int64, start, end time.Time) (*TrainingReportData, error) {
	if orgID <= 0 {
		return nil, fmt.Errorf("缺少组织 id")
	}
	elevated, eerr := ContextWithElevatedAgent(ctx, uint(orgID))
	if eerr != nil {
		log.Warnf("training report elevated: %v", eerr)
		elevated = ctx
	}
	// 教练名单只拉一次：成员过滤与提交动态过滤共用
	coachSet := uc.fetchCoachUserIDSet(elevated, orgID)
	// 用 elevated 上下文解析成员并排除教练
	memberIDs, err := uc.resolveMemberIDs(elevated, orgID, groupID, squadID, coachSet)
	if err != nil {
		return nil, err
	}

	days := int(end.Sub(start).Hours()/24) + 1
	prevEnd := start.AddDate(0, 0, -1)
	prevStart := prevEnd.AddDate(0, 0, -(days - 1))

	// 聚合每日提交 / AC
	dayTotals := make([]DayCount, 0, days)
	dayACTotals := make([]DayCount, 0, days)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		dayTotals = append(dayTotals, DayCount{Date: d.Format(dateLayout), Count: 0})
		dayACTotals = append(dayACTotals, DayCount{Date: d.Format(dateLayout), Count: 0})
	}
	submitByUser := make(map[int64]int64, len(memberIDs))
	acByUser := make(map[int64]int64, len(memberIDs))
	var prevTotal int64

	// 拉个人热力（提交 + AC + 上期）：errgroup 有限并发，缩短总时长且不打爆 core
	var hmMu sync.Mutex
	g := new(errgroup.Group)
	g.SetLimit(4)
	for _, uid := range memberIDs {
		uid := uid
		g.Go(func() error {
			hm, err := uc.fetchHeatmapUser(ctx, uid, start, end, false)
			if err != nil {
				log.Warnf("training report heatmap user=%d: %v", uid, err)
				return nil // 单人失败不阻断整体
			}
			acHm, acErr := uc.fetchHeatmapUser(ctx, uid, start, end, true)
			prevHm, prevErr := uc.fetchHeatmapUser(ctx, uid, prevStart, prevEnd, false)

			hmMu.Lock()
			defer hmMu.Unlock()
			var uSum int64
			for i, d := range hm {
				if i < len(dayTotals) {
					dayTotals[i].Count += d.Count
				}
				uSum += d.Count
			}
			submitByUser[uid] = uSum
			if acErr == nil {
				var acSum int64
				for i, d := range acHm {
					if i < len(dayACTotals) {
						dayACTotals[i].Count += d.Count
					}
					acSum += d.Count
				}
				acByUser[uid] = acSum
			}
			if prevErr == nil {
				prevTotal += sumDayCounts(prevHm)
			}
			return nil
		})
	}
	_ = g.Wait()

	idMap := uc.fetchIdentities(ctx, memberIDs, orgID)
	nameMap := make(map[int64]string, len(idMap))
	for id, idn := range idMap {
		if idn.Name != "" {
			nameMap[id] = idn.Name
		} else {
			nameMap[id] = idn.Username
		}
	}

	// 全量活跃榜 + 兼容旧 Top 字段
	activeRanking := buildActiveRanking(submitByUser, acByUser, idMap)
	topSubmit := rankFromMap(submitByUser, nameMap, 0) // 0=全部有分
	topAC := rankFromMap(acByUser, nameMap, 0)

	// 不活跃：区间内 0 提交，或最后提交早于 end-2 天
	threshold := end.AddDate(0, 0, -2)
	lastMap, err := uc.fetchLastSubmitMap(ctx, memberIDs)
	if err != nil {
		log.Warnf("training report lastSubmit: %v", err)
		lastMap = map[int64]int64{}
	}
	inactive := make([]string, 0)
	active := 0
	type pair struct {
		id   int64
		name string
		ts   int64
	}
	cands := make([]pair, 0)
	for _, id := range memberIDs {
		if submitByUser[id] > 0 {
			active++
			continue
		}
		ts := lastMap[id]
		if ts > 0 && !time.Unix(ts, 0).Before(threshold) {
			// 区间外最近仍活跃但本区间 0 —— 仍算本区间不活跃
		}
		n := nameMap[id]
		if n == "" {
			n = fmt.Sprintf("用户%d", id)
		}
		cands = append(cands, pair{id: id, name: n, ts: ts})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ts < cands[j].ts })
	for _, c := range cands {
		inactive = append(inactive, c.name)
		if len(inactive) >= 50 {
			break
		}
	}

	scopeLabel := "整组织"
	if squadID > 0 {
		scopeLabel = fmt.Sprintf("分队#%d", squadID)
	} else if groupID > 0 {
		scopeLabel = fmt.Sprintf("分组#%d", groupID)
	}

	initName := ""
	initEmail := ""
	if initiatorID > 0 {
		initEmail = uc.userContactEmail(initiatorID)
		if p := uc.userProfile(initiatorID); p != nil {
			initName = p.Name
		}
		if initName == "" {
			initName = fmt.Sprintf("用户%d", initiatorID)
		}
	}

	data := &TrainingReportData{
		OrgID:            orgID,
		GroupID:          groupID,
		SquadID:          squadID,
		ScopeLabel:       scopeLabel,
		StartDate:        start.Format(dateLayout),
		EndDate:          end.Format(dateLayout),
		PrevStartDate:    prevStart.Format(dateLayout),
		PrevEndDate:      prevEnd.Format(dateLayout),
		MemberCount:      len(memberIDs),
		MemberIDs:        memberIDs,
		TotalSubmits:     sumDayCounts(dayTotals),
		PrevTotalSubmits: prevTotal,
		TotalAC:          sumMap(acByUser),
		DailyTrend:       dayTotals,
		DailyACTrend:     dayACTotals,
		TopSubmit:        topSubmit,
		TopAC:            topAC,
		ActiveRanking:    activeRanking,
		InactiveMembers:  inactive,
		ActiveMembers:    active,
		InitiatorUserID:  initiatorID,
		InitiatorName:    initName,
		InitiatorEmail:   initEmail,
	}

	// 多维度预取（失败不阻断）
	data.OrgSubmitSample = uc.fetchOrgSubmitSample(elevated, start, end, 60)
	// 动态里若混入教练提交，剔除（复用开头拉取的教练名单）
	if len(coachSet) > 0 {
		feed := data.OrgSubmitSample[:0]
		for _, f := range data.OrgSubmitSample {
			if _, isCoach := coachSet[f.UserID]; !isCoach {
				feed = append(feed, f)
			}
		}
		data.OrgSubmitSample = feed
	}
	data.TeamTags = aggregateTeamTags(data.OrgSubmitSample, 24)
	data.ProblemOverview = aggregateProblemOverview(data.OrgSubmitSample, 25)
	data.Contests = uc.fetchOrgContests(elevated, start, end, 20, memberIDs)
	data.ContestRankings = uc.fetchContestRankSnaps(elevated, data.Contests, 4, 15)
	data.RecentBlogs = uc.fetchOrgBlogBriefs(elevated, orgID, 12)

	return data, nil
}

// buildActiveRanking 仅有提交成员，按提交降序
func buildActiveRanking(submit, ac map[int64]int64, idMap map[int64]userIdentity) []MemberStat {
	type pair struct {
		id  int64
		sub int64
		ac  int64
	}
	arr := make([]pair, 0, len(submit))
	var totalSub int64
	for id, s := range submit {
		if s <= 0 {
			continue
		}
		arr = append(arr, pair{id: id, sub: s, ac: ac[id]})
		totalSub += s
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].sub == arr[j].sub {
			if arr[i].ac == arr[j].ac {
				return arr[i].id < arr[j].id
			}
			return arr[i].ac > arr[j].ac
		}
		return arr[i].sub > arr[j].sub
	})
	out := make([]MemberStat, 0, len(arr))
	for i, p := range arr {
		idn := idMap[p.id]
		n := idn.Name
		if n == "" {
			n = idn.Username
		}
		if n == "" {
			n = fmt.Sprintf("用户%d", p.id)
		}
		rate := 0.0
		if p.sub > 0 {
			rate = float64(p.ac) / float64(p.sub) * 100
		}
		share := 0.0
		if totalSub > 0 {
			share = float64(p.sub) / float64(totalSub) * 100
		}
		out = append(out, MemberStat{
			Rank: int64(i + 1), UserID: p.id, Name: n, Username: idn.Username,
			Submits: p.sub, AC: p.ac, ACRate: rate, Share: share,
			ProfileURL: profileURL(idn.Username, p.id),
		})
	}
	return out
}

func aggregateTeamTags(feed []SubmitFeedItem, limit int) []TagHit {
	m := map[string]int{}
	for _, f := range feed {
		for _, t := range f.Tags {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			m[t]++
		}
	}
	arr := make([]TagHit, 0, len(m))
	for k, v := range m {
		arr = append(arr, TagHit{Tag: k, Count: v})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].Count == arr[j].Count {
			return arr[i].Tag < arr[j].Tag
		}
		return arr[i].Count > arr[j].Count
	})
	if limit > 0 && len(arr) > limit {
		arr = arr[:limit]
	}
	return arr
}

func aggregateProblemOverview(feed []SubmitFeedItem, limit int) []ProblemTouch {
	type agg struct {
		title    string
		platform string
		tags     []string
		trySet   map[string]struct{}
		acSet    map[string]struct{}
		acN      int
	}
	m := map[string]*agg{}
	for _, f := range feed {
		key := f.Platform + "|" + f.Problem
		if f.Title != "" {
			key = f.Platform + "|" + f.Title
		}
		a, ok := m[key]
		if !ok {
			a = &agg{
				title: f.Title, platform: f.Platform,
				trySet: map[string]struct{}{}, acSet: map[string]struct{}{},
			}
			if a.title == "" {
				a.title = f.Problem
			}
			if len(f.Tags) > 0 {
				a.tags = append([]string(nil), f.Tags...)
			}
			m[key] = a
		}
		name := f.UserName
		if name == "" {
			name = fmt.Sprintf("用户%d", f.UserID)
		}
		a.trySet[name] = struct{}{}
		us := strings.ToUpper(f.Status)
		if strings.Contains(us, "AC") || us == "OK" || us == "ACCEPT" || us == "ACCEPTED" {
			if _, seen := a.acSet[name]; !seen {
				a.acSet[name] = struct{}{}
				a.acN++
			}
		}
	}
	out := make([]ProblemTouch, 0, len(m))
	for k, a := range m {
		parts := strings.SplitN(k, "|", 2)
		prob := a.title
		if len(parts) == 2 && parts[1] != "" {
			prob = parts[1]
		}
		tryUsers := make([]string, 0, len(a.trySet))
		for n := range a.trySet {
			tryUsers = append(tryUsers, n)
		}
		sort.Strings(tryUsers)
		acUsers := make([]string, 0, len(a.acSet))
		for n := range a.acSet {
			acUsers = append(acUsers, n)
		}
		sort.Strings(acUsers)
		// 名单过长截断
		if len(tryUsers) > 8 {
			tryUsers = append(tryUsers[:8], "…")
		}
		if len(acUsers) > 8 {
			acUsers = append(acUsers[:8], "…")
		}
		out = append(out, ProblemTouch{
			Problem: prob, Title: a.title, Tags: a.tags, Platform: a.platform,
			Submitters: len(a.trySet), ACCount: a.acN,
			ACUsers: acUsers, TryUsers: tryUsers,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Submitters == out[j].Submitters {
			return out[i].ACCount > out[j].ACCount
		}
		return out[i].Submitters > out[j].Submitters
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (uc *SummaryUseCase) fetchOrgSubmitSample(ctx context.Context, start, end time.Time, limit int) []SubmitFeedItem {
	if limit <= 0 {
		limit = 20
	}
	conn, err := uc.dialCoreCtx(ctx)
	if err != nil {
		return nil
	}
	cli := submit_log.NewSubmitClient(conn)
	cursor := end.AddDate(0, 0, 1).Unix()
	res, err := cli.GetSubmitLog(ctx, &submit_log.GetSubmitLogReq{
		UserId: -1,
		Limit:  int64(limit * 2),
		Cursor: cursor,
	})
	if err != nil || res == nil {
		log.Warnf("fetchOrgSubmitSample: %v", err)
		return nil
	}
	dayStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	dayEnd := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, end.Location()).AddDate(0, 0, 1)
	out := make([]SubmitFeedItem, 0, limit)
	for _, v := range res.Data {
		if v == nil {
			continue
		}
		t := time.Unix(v.Time, 0)
		if t.Before(dayStart) || !t.Before(dayEnd) {
			continue
		}
		out = append(out, SubmitFeedItem{
			UserID: v.UserId, UserName: v.UserName, Platform: v.Platform,
			Problem: v.Problem, Title: v.ProblemTitle, Status: v.Status, Lang: v.Lang,
			Time: t.Format("01-02 15:04"), Tags: append([]string(nil), v.ProblemTags...),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// fetchOrgContests 组织比赛：先按区间筛，空则放宽无时间窗再本地过滤；再按成员历史补全。
func (uc *SummaryUseCase) fetchOrgContests(ctx context.Context, start, end time.Time, limit int, memberIDs []int64) []ContestBrief {
	if limit <= 0 {
		limit = 20
	}
	conn, err := uc.dialCoreCtx(ctx)
	if err != nil {
		return nil
	}
	cli := contest_log.NewContestClient(conn)

	timeFrom := start.Unix()
	timeTo := end.AddDate(0, 0, 1).Unix() - 1
	// 策略 1：组织流 + 时间窗
	res, err := cli.GetContestList(ctx, &contest_log.GetContestListReq{
		UserId: -1, Limit: int64(limit), Offset: 0,
		TimeFrom: timeFrom, TimeTo: timeTo,
	})
	if err != nil {
		log.Warnf("fetchOrgContests timed: %v", err)
	}
	out := contestLogsToBriefs(res)
	// 策略 2：无时间窗拉最近组织比赛，本地按 time/start/end 过滤
	if len(out) == 0 {
		res2, err2 := cli.GetContestList(ctx, &contest_log.GetContestListReq{
			UserId: -1, Limit: 40, Offset: 0,
		})
		if err2 != nil {
			log.Warnf("fetchOrgContests untimed: %v", err2)
		}
		out = filterContestsInRange(contestLogsToBriefs(res2), start, end)
	}
	// 策略 3：抽样成员个人比赛史合并（解决 time 字段异常 / 列表去重丢场次）
	if len(out) < limit && len(memberIDs) > 0 {
		seen := map[string]struct{}{}
		for _, c := range out {
			seen[c.Platform+"|"+c.ContestID] = struct{}{}
		}
		sample := memberIDs
		if len(sample) > 12 {
			sample = sample[:12]
		}
		for _, uid := range sample {
			hres, herr := cli.GetUserContestHistory(ctx, &contest_log.GetUserContestHistoryReq{
				UserId: uid, Limit: 30, Cursor: 0,
			})
			if herr != nil || hres == nil {
				continue
			}
			for _, v := range hres.GetData() {
				if v == nil {
					continue
				}
				if !contestInRange(v.GetTime(), v.GetStartTime(), v.GetEndTime(), start, end) {
					continue
				}
				key := v.Platform + "|" + v.ContestId
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				ts := ""
				if v.Time > 0 {
					ts = time.Unix(v.Time, 0).Format(dateLayout)
				} else if v.StartTime > 0 {
					ts = time.Unix(v.StartTime, 0).Format(dateLayout)
				}
				out = append(out, ContestBrief{
					ID: v.Id, Platform: v.Platform, ContestID: v.ContestId, ContestName: v.ContestName,
					Rank: v.Rank, ACCount: v.AcCount, TotalCount: v.TotalCount, Time: ts,
					UserID: v.UserId, UserName: v.UserName,
				})
				if len(out) >= limit {
					break
				}
			}
			if len(out) >= limit {
				break
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func contestLogsToBriefs(res *contest_log.GetContestListRes) []ContestBrief {
	if res == nil {
		return nil
	}
	out := make([]ContestBrief, 0, len(res.Data))
	for _, v := range res.Data {
		if v == nil {
			continue
		}
		ts := ""
		if v.Time > 0 {
			ts = time.Unix(v.Time, 0).Format(dateLayout)
		} else if v.StartTime > 0 {
			ts = time.Unix(v.StartTime, 0).Format(dateLayout)
		}
		out = append(out, ContestBrief{
			ID: v.Id, Platform: v.Platform, ContestID: v.ContestId, ContestName: v.ContestName,
			Rank: v.Rank, ACCount: v.AcCount, TotalCount: v.TotalCount, Time: ts,
			UserID: v.UserId, UserName: v.UserName,
		})
	}
	return out
}

func contestInRange(timeU, startU, endU int64, start, end time.Time) bool {
	dayStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	dayEnd := time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, end.Location())
	// 任一时间戳落入区间即算
	for _, u := range []int64{timeU, startU, endU} {
		if u <= 0 {
			continue
		}
		t := time.Unix(u, 0)
		if !t.Before(dayStart) && !t.After(dayEnd) {
			return true
		}
	}
	// 开赛在区间前、结束在区间后（跨区间比赛）
	if startU > 0 && endU > 0 {
		st, en := time.Unix(startU, 0), time.Unix(endU, 0)
		if !en.Before(dayStart) && !st.After(dayEnd) {
			return true
		}
	}
	return false
}

func filterContestsInRange(in []ContestBrief, start, end time.Time) []ContestBrief {
	if len(in) == 0 {
		return nil
	}
	out := make([]ContestBrief, 0, len(in))
	for _, c := range in {
		var tu int64
		if c.Time != "" {
			if t, err := time.ParseInLocation(dateLayout, c.Time, time.Local); err == nil {
				tu = t.Unix()
			}
		}
		// ContestBrief 只有 date 字符串时用 time 字段
		if contestInRange(tu, 0, 0, start, end) || c.Time == "" {
			// 无时间的也保留（策略 2 兜底时再靠策略 3）
			if contestInRange(tu, 0, 0, start, end) {
				out = append(out, c)
			}
		}
	}
	// 若本地过滤全空，退回最近若干条（至少给用户看到组织有比赛）
	if len(out) == 0 && len(in) > 0 {
		n := 8
		if len(in) < n {
			n = len(in)
		}
		return in[:n]
	}
	return out
}

func (uc *SummaryUseCase) fetchContestRankSnaps(ctx context.Context, contests []ContestBrief, maxContests, topN int) []ContestRankSnap {
	if maxContests <= 0 {
		maxContests = 3
	}
	if topN <= 0 {
		topN = 8
	}
	seen := map[string]struct{}{}
	out := make([]ContestRankSnap, 0, maxContests)
	conn, err := uc.dialCoreCtx(ctx)
	if err != nil {
		return nil
	}
	cli := contest_log.NewContestClient(conn)
	for _, c := range contests {
		if c.ContestID == "" {
			continue
		}
		key := c.Platform + ":" + c.ContestID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		res, err := cli.GetContestRanking(ctx, &contest_log.GetContestRankingReq{
			ContestId: c.ContestID,
			Limit:     int64(topN),
			Offset:    0,
		})
		if err != nil || res == nil {
			continue
		}
		snap := ContestRankSnap{
			ContestID:   c.ContestID,
			ContestName: c.ContestName,
			Platform:    c.Platform,
			Total:       res.GetTotal(),
		}
		if ct := res.GetContest(); ct != nil {
			if ct.ContestName != "" {
				snap.ContestName = ct.ContestName
			}
			if ct.Platform != "" {
				snap.Platform = ct.Platform
			}
		}
		for _, r := range res.GetData() {
			if r == nil {
				continue
			}
			snap.Top = append(snap.Top, ContestRankRow{
				Rank: r.Rank, UserID: r.UserId, Name: r.Name,
				Score: r.Score, ACCount: r.AcCount, TotalCount: r.TotalCount,
			})
		}
		out = append(out, snap)
		if len(out) >= maxContests {
			break
		}
	}
	return out
}

func (uc *SummaryUseCase) fetchOrgBlogBriefs(ctx context.Context, orgID int64, limit int) []BlogBrief {
	if uc == nil || uc.reg == nil || orgID <= 0 {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 15 {
		limit = 15
	}
	// 复用 agent tool 的 discovery HTTP
	path := fmt.Sprintf("/v1/user/blog/recommend?orgId=%d&page=1&pageSize=%d", orgID, limit)
	body, code, err := httpDiscoveryGet(ctx, uc.reg, "user", path)
	if err != nil || code >= 400 {
		log.Warnf("fetchOrgBlogBriefs org=%d: code=%d err=%v", orgID, code, err)
		return nil
	}
	var raw map[string]interface{}
	if err := jsonUnmarshal(body, &raw); err != nil {
		return nil
	}
	data, _ := raw["data"].(map[string]interface{})
	list, _ := data["list"].([]interface{})
	out := make([]BlogBrief, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		b := BlogBrief{
			Title:   fmt.Sprint(m["title"]),
			Summary: fmt.Sprint(m["summary"]),
		}
		if b.Title == "<nil>" {
			b.Title = ""
		}
		if b.Summary == "<nil>" {
			b.Summary = ""
		}
		if len(b.Summary) > 120 {
			b.Summary = b.Summary[:120] + "…"
		}
		if id, ok := m["id"].(float64); ok {
			b.ID = int64(id)
		}
		if a, ok := m["author"].(map[string]interface{}); ok {
			if n, ok := a["name"].(string); ok && n != "" {
				b.Author = n
			} else if n, ok := a["username"].(string); ok {
				b.Author = n
			}
		}
		if t, ok := m["publishedAt"].(string); ok && t != "" {
			b.Time = t
		} else if t, ok := m["createdAt"].(string); ok {
			b.Time = t
		}
		if b.Title != "" {
			out = append(out, b)
		}
	}
	return out
}

func sumMap(m map[int64]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

// rankFromMap topN<=0 表示返回全部有分成员
func rankFromMap(scores map[int64]int64, names map[int64]string, topN int) []RankEntry {
	type pair struct {
		id    int64
		score int64
	}
	arr := make([]pair, 0, len(scores))
	for id, sc := range scores {
		if sc <= 0 {
			continue
		}
		arr = append(arr, pair{id: id, score: sc})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].score == arr[j].score {
			return arr[i].id < arr[j].id
		}
		return arr[i].score > arr[j].score
	})
	if topN > 0 && len(arr) > topN {
		arr = arr[:topN]
	}
	out := make([]RankEntry, 0, len(arr))
	for i, p := range arr {
		n := names[p.id]
		if n == "" {
			n = fmt.Sprintf("用户%d", p.id)
		}
		out = append(out, RankEntry{
			Rank:   int64(i + 1),
			UserID: p.id,
			Name:   n,
			Score:  p.score,
		})
	}
	return out
}
