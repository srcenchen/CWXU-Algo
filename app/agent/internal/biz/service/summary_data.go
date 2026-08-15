package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/api/core/v1/problem"
	"cwxu-algo/api/core/v1/statistic"
	"cwxu-algo/api/core/v1/submit_log"
	profile2 "cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/agent/internal/agent/svcdata"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/common/utils/auth"

	"github.com/go-kratos/kratos/v2/log"
	grpc2 "google.golang.org/grpc"
)

const dateLayout = "2006-01-02"

type DayCount struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

type SubmitItem struct {
	Platform   string   `json:"platform"`
	Problem    string   `json:"problem"`
	Status     string   `json:"status"`
	Lang       string   `json:"lang"`
	Time       string   `json:"time"`
	ProblemID  uint32   `json:"problemId,omitempty"`
	Title      string   `json:"title,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Difficulty string   `json:"difficulty,omitempty"`
}

// TagACBrief 用户标签 AC 摘要（日报/周报给 AI）
type TagACBrief struct {
	Tag     string  `json:"tag"`
	Score   float64 `json:"score"`
	ACCount int64   `json:"acCount"`
}

type RankEntry struct {
	Rank   int64  `json:"rank"`
	UserID int64  `json:"userId"`
	Name   string `json:"name"`
	Score  int64  `json:"score"`
}

type DailyReportData struct {
	UserID           int64        `json:"userId"`
	Name             string       `json:"name"`
	Email            string       `json:"email"`
	Yesterday        string       `json:"yesterday"`
	YesterdayCount   int64        `json:"yesterdayCount"`
	ConsecutiveZeros int          `json:"consecutiveZeroDays"`
	Last7Days        []DayCount   `json:"last7Days"`
	Last7DaysAC      []DayCount   `json:"last7DaysAc,omitempty"`
	YesterdayLogs    []SubmitItem `json:"yesterdayLogs"`
	// 用户标签画像（预取；亦可 function call problem_tags）
	TagRadar []TagACBrief `json:"tagRadar,omitempty"`
	// 昨日提交涉及的标签聚合 count
	YesterdayTagHits map[string]int `json:"yesterdayTagHits,omitempty"`
	// 近期个人比赛（含 rank、过题数）
	RecentContests []ContestBrief `json:"recentContests,omitempty"`
}

type WeeklyReportData struct {
	CoachUserID     int64       `json:"coachUserId"`
	CoachName       string      `json:"coachName"`
	CoachEmail      string      `json:"coachEmail"`
	WeekStart       string      `json:"weekStart"`
	WeekEnd         string      `json:"weekEnd"`
	PrevWeekStart   string      `json:"prevWeekStart"`
	PrevWeekEnd     string      `json:"prevWeekEnd"`
	ThisWeekTotal   int64       `json:"thisWeekTotal"`
	PrevWeekTotal   int64       `json:"prevWeekTotal"`
	DailyTrend      []DayCount  `json:"dailyTrend"`
	TopSubmit       []RankEntry `json:"topSubmit"`
	TopAC           []RankEntry `json:"topAC"`
	InactiveMembers []string    `json:"inactiveMembers"`
}

// dialUser / dialCoreData 返回按服务名缓存的共享长连接（调用方不得 Close）。
// 鉴权 metadata 由每次 RPC 的 ctx 携带，与连接无关。
func (uc *SummaryUseCase) dialUser(_ context.Context) (*grpc2.ClientConn, error) {
	return svcdata.SharedGRPCConn(uc.reg, "user")
}

func (uc *SummaryUseCase) dialCoreData(_ context.Context) (*grpc2.ClientConn, error) {
	return svcdata.SharedGRPCConn(uc.reg, "core-data")
}

func (uc *SummaryUseCase) userRPC() (*grpc2.ClientConn, error) {
	return uc.dialUser(context.Background())
}

func (uc *SummaryUseCase) userProfile(userId int64) *profile2.GetByIdRes {
	conn, err := uc.userRPC()
	if err != nil {
		log.Warnf("userProfile dial user=%d: %v", userId, err)
		return nil
	}
	p := profile2.NewProfileClient(conn)
	// 注意：无 JWT 时 GetById 会剥离 email / emailEnabled 等私有字段，仅可取公开 name 等。
	res, err := p.GetById(context.Background(), &profile2.GetByIdReq{UserId: userId})
	if err != nil {
		log.Warnf("userProfile GetById user=%d: %v", userId, err)
		return nil
	}
	return res
}

// userSyncPolicy 服务间取定时/邮件策略（含个人开关与组织授权，不做隐私剥离）
func (uc *SummaryUseCase) userSyncPolicy(userId int64) *profile2.UserSyncPolicy {
	if userId <= 0 {
		return nil
	}
	conn, err := uc.userRPC()
	if err != nil {
		return nil
	}
	cli := profile2.NewProfileClient(conn)
	res, err := cli.GetSyncPolicies(context.Background(), &profile2.GetSyncPoliciesReq{
		UserIds: []int64{userId},
	})
	if err != nil || res == nil {
		return nil
	}
	for _, p := range res.GetPolicies() {
		if p != nil && p.GetUserId() == userId {
			return p
		}
	}
	return nil
}

// userContactEmail 服务间取联系邮箱（不做隐私剥离）
func (uc *SummaryUseCase) userContactEmail(userId int64) string {
	if userId <= 0 {
		return ""
	}
	conn, err := uc.userRPC()
	if err != nil {
		return ""
	}
	cli := profile2.NewProfileClient(conn)
	res, err := cli.GetContactEmail(context.Background(), &profile2.GetContactEmailReq{UserId: userId})
	if err != nil || res == nil {
		return ""
	}
	return strings.TrimSpace(res.GetEmail())
}

func (uc *SummaryUseCase) checkRoleId(userId int64) int {
	p := uc.userProfile(userId)
	if p == nil {
		return 0
	}
	return int(p.RoleId)
}

// canSendDailyEmail 个人日报开关开即允许（日报不再要求组织授权）
// 必须用 GetSyncPolicies，不能用无鉴权 GetById（后者会把 emailEnabled/授权剥成 false）。
func (uc *SummaryUseCase) canSendDailyEmail(userId int64) bool {
	p := uc.userSyncPolicy(userId)
	if p == nil {
		return false
	}
	return p.GetEmailEnabled()
}

// canSendWeeklyEmail 个人周报开 AND 组织 staff 周报授权
func (uc *SummaryUseCase) canSendWeeklyEmail(userId int64) bool {
	p := uc.userSyncPolicy(userId)
	if p == nil {
		return false
	}
	return p.GetEmailWeeklyEnabled() && p.GetEnableAiWeeklyEmail()
}

func (uc *SummaryUseCase) checkEmailEnabled(userId int64) bool {
	return uc.canSendDailyEmail(userId)
}

func (uc *SummaryUseCase) getUserIds() []int64 {
	userRpc, err := uc.userRPC()
	if err != nil {
		log.Warnf("getUserIds dial user 服务失败: %v", err)
		return make([]int64, 0)
	}
	profile := profile2.NewProfileClient(userRpc)
	getUsers := func(pageNum int) (*profile2.GetListRes, error) {
		return profile.GetList(context.Background(), &profile2.GetListReq{
			PageSize: 100,
			PageNum:  int64(pageNum),
		})
	}
	res, err := getUsers(1)
	if err != nil {
		log.Warnf("getUserIds GetList page=1: %v", err)
		return make([]int64, 0)
	}
	rList := []*profile2.GetListRes{res}
	totalPage := (res.Total + 99) / 100
	for i := 2; i <= int(totalPage); i++ {
		r, err := getUsers(i)
		if err != nil {
			log.Warnf("getUserIds GetList page=%d: %v", i, err)
			continue
		}
		rList = append(rList, r)
	}
	var userIds []int64
	for _, v := range rList {
		for _, u := range v.List {
			userIds = append(userIds, int64(u.UserId))
		}
	}
	return userIds
}

func fillMissingDays(start, end time.Time, items []*statistic.HeatmapResp_HeatmapItem) []DayCount {
	m := make(map[string]int64, len(items))
	for _, it := range items {
		// Heatmap 返回 2006-01-02
		m[it.Date] = it.Count
	}
	out := make([]DayCount, 0)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format(dateLayout)
		out = append(out, DayCount{Date: key, Count: m[key]})
	}
	return out
}

func sumDayCounts(days []DayCount) int64 {
	var s int64
	for _, d := range days {
		s += d.Count
	}
	return s
}

func consecutiveZeroFromEnd(days []DayCount) int {
	n := 0
	for i := len(days) - 1; i >= 0; i-- {
		if days[i].Count == 0 {
			n++
		} else {
			break
		}
	}
	return n
}

func dailyACTrend(days, acDays []DayCount, err error) []DayCount {
	if err == nil {
		return acDays
	}
	fallback := make([]DayCount, len(days))
	for i, day := range days {
		fallback[i] = DayCount{Date: day.Date}
	}
	return fallback
}

func (uc *SummaryUseCase) fetchHeatmap(ctx context.Context, userId int64, start, end time.Time) ([]DayCount, error) {
	conn, err := uc.dialCoreData(ctx)
	if err != nil {
		return nil, err
	}
	cli := statistic.NewStatisticClient(conn)
	res, err := cli.Heatmap(ctx, &statistic.HeatmapReq{
		UserId:    userId,
		StartDate: start.Format(dateLayout),
		EndDate:   end.Format(dateLayout),
		IsAc:      false,
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

func (uc *SummaryUseCase) fetchSubmitLogs(ctx context.Context, userId int64, endDate time.Time, limit int64) ([]SubmitItem, error) {
	if limit <= 0 {
		return []SubmitItem{}, nil
	}
	if limit > 50 {
		limit = 50
	}
	conn, err := uc.dialCoreData(ctx)
	if err != nil {
		return nil, err
	}
	cli := submit_log.NewSubmitClient(conn)
	// cursor = 次日 0 点，向前取 limit 条
	cursor := endDate.AddDate(0, 0, 1).Unix()
	res, err := cli.GetSubmitLog(ctx, &submit_log.GetSubmitLogReq{
		UserId: userId,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return nil, err
	}
	dayStart := time.Date(endDate.Year(), endDate.Month(), endDate.Day(), 0, 0, 0, 0, endDate.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	out := make([]SubmitItem, 0)
	for _, v := range res.Data {
		t := time.Unix(v.Time, 0)
		if t.Before(dayStart) || !t.Before(dayEnd) {
			continue
		}
		item := SubmitItem{
			Platform:   v.Platform,
			Problem:    v.Problem,
			Status:     v.Status,
			Lang:       v.Lang,
			Time:       t.Format("15:04"),
			ProblemID:  v.GetProblemId(),
			Title:      v.GetProblemTitle(),
			Tags:       append([]string(nil), v.GetProblemTags()...),
			Difficulty: v.GetProblemDifficulty(),
		}
		out = append(out, item)
	}
	return out, nil
}

func (uc *SummaryUseCase) fetchUserTagRadar(ctx context.Context, userId int64) []TagACBrief {
	if userId <= 0 {
		return nil
	}
	conn, err := uc.dialCoreData(ctx)
	if err != nil {
		return nil
	}
	cli := problem.NewProblemClient(conn)
	res, err := cli.UserProfile(ctx, &problem.UserProfileReq{UserId: userId})
	if err != nil || res == nil {
		log.Warnf("fetchUserTagRadar user=%d: %v", userId, err)
		return nil
	}
	radar := res.GetRadar()
	out := make([]TagACBrief, 0, len(radar))
	for _, t := range radar {
		if t == nil || strings.TrimSpace(t.GetTag()) == "" {
			continue
		}
		out = append(out, TagACBrief{
			Tag:     t.GetTag(),
			Score:   t.GetScore(),
			ACCount: t.GetAcCount(),
		})
	}
	// 按 ac 降序，日报只带前 20
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ACCount != out[j].ACCount {
			return out[i].ACCount > out[j].ACCount
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func aggregateTagHits(logs []SubmitItem) map[string]int {
	m := map[string]int{}
	for _, l := range logs {
		for _, tag := range l.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			m[tag]++
		}
	}
	return m
}

func (uc *SummaryUseCase) fetchRank(ctx context.Context, start, end time.Time, scoreType string, pageSize int64) ([]RankEntry, error) {
	conn, err := uc.dialCoreData(ctx)
	if err != nil {
		return nil, err
	}
	cli := statistic.NewStatisticClient(conn)
	res, err := cli.Rank(ctx, &statistic.RankReq{
		StartDate: start.Format(dateLayout),
		EndDate:   end.Format(dateLayout),
		ScoreType: scoreType,
		Page:      1,
		PageSize:  pageSize,
		GroupId:   -1,
	})
	if err != nil {
		return nil, err
	}
	out := make([]RankEntry, 0, len(res.Data))
	for _, v := range res.Data {
		out = append(out, RankEntry{
			Rank:   v.Rank,
			UserID: v.UserId,
			Name:   v.Name,
			Score:  v.Score,
		})
	}
	return out, nil
}

func (uc *SummaryUseCase) fetchLastSubmitMap(ctx context.Context, userIds []int64) (map[int64]int64, error) {
	if len(userIds) == 0 {
		return map[int64]int64{}, nil
	}
	conn, err := uc.dialCoreData(ctx)
	if err != nil {
		return nil, err
	}
	cli := submit_log.NewSubmitClient(conn)
	// 分批，避免一次过大
	result := make(map[int64]int64, len(userIds))
	const batch = 200
	for i := 0; i < len(userIds); i += batch {
		j := i + batch
		if j > len(userIds) {
			j = len(userIds)
		}
		res, err := cli.LastSubmitTime(ctx, &submit_log.LastSubmitTimeReq{UserIds: userIds[i:j]})
		if err != nil {
			return nil, err
		}
		var m map[int64]int64
		if err := utils.GobDecoder(res.TimeMap, &m); err != nil {
			return nil, err
		}
		for k, v := range m {
			result[k] = v
		}
	}
	return result, nil
}

func (uc *SummaryUseCase) loadDailyReportData(ctx context.Context, userId int64) (*DailyReportData, error) {
	email := uc.userContactEmail(userId)
	if email == "" {
		return nil, fmt.Errorf("用户 %d 未绑定邮箱", userId)
	}
	name := ""
	if profile := uc.userProfile(userId); profile != nil {
		name = profile.Name
	}
	if name == "" {
		name = fmt.Sprintf("用户%d", userId)
	}

	now := time.Now()
	yesterday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -1)
	start7 := yesterday.AddDate(0, 0, -6)

	days, err := uc.fetchHeatmap(ctx, userId, start7, yesterday)
	if err != nil {
		return nil, fmt.Errorf("拉取热力图失败: %w", err)
	}
	acDays, acErr := uc.fetchHeatmapUser(ctx, userId, start7, yesterday, true)
	if acErr != nil {
		log.Warnf("拉取近 7 日 AC 热力图失败 user=%d: %v", userId, acErr)
	}
	acDays = dailyACTrend(days, acDays, acErr)
	yCount := int64(0)
	if len(days) > 0 {
		yCount = days[len(days)-1].Count
	}
	logs, err := uc.fetchSubmitLogs(ctx, userId, yesterday, yCount)
	if err != nil {
		log.Warnf("拉取昨日提交明细失败 user=%d: %v", userId, err)
		logs = []SubmitItem{}
	}
	radar := uc.fetchUserTagRadar(ctx, userId)
	// 日报只显示昨日比赛：[昨日0点, 今日0点)
	contests := uc.fetchUserRecentContests(ctx, userId, 15, yesterday.Unix(), yesterday.AddDate(0, 0, 1).Unix())

	return &DailyReportData{
		UserID:           userId,
		Name:             name,
		Email:            email,
		Yesterday:        yesterday.Format(dateLayout),
		YesterdayCount:   yCount,
		ConsecutiveZeros: consecutiveZeroFromEnd(days),
		Last7Days:        days,
		Last7DaysAC:      acDays,
		YesterdayLogs:    logs,
		TagRadar:         radar,
		YesterdayTagHits: aggregateTagHits(logs),
		RecentContests:   contests,
	}, nil
}

// fetchUserRecentContests 用户比赛历史。since/until 为 unix 秒（0=不限）：
// 日报传 [昨日0点, 今日0点) 只显示昨日比赛。
func (uc *SummaryUseCase) fetchUserRecentContests(ctx context.Context, userId int64, limit int64, since, until int64) []ContestBrief {
	if userId <= 0 {
		return nil
	}
	if limit <= 0 {
		limit = 15
	}
	if limit > 30 {
		limit = 30
	}
	conn, err := uc.dialCoreData(ctx)
	if err != nil {
		return nil
	}
	cli := contest_log.NewContestClient(conn)
	res, err := cli.GetUserContestHistory(ctx, &contest_log.GetUserContestHistoryReq{
		UserId: userId,
		Limit:  limit,
		Cursor: 0,
	})
	if err != nil || res == nil {
		log.Warnf("fetchUserRecentContests user=%d: %v", userId, err)
		return nil
	}
	out := make([]ContestBrief, 0, len(res.Data))
	for _, v := range res.Data {
		if v == nil {
			continue
		}
		t := v.Time
		if t <= 0 {
			t = v.StartTime
		}
		if since > 0 && t < since {
			continue
		}
		if until > 0 && t >= until {
			continue
		}
		ts := ""
		if v.Time > 0 {
			ts = time.Unix(v.Time, 0).Format(dateLayout)
		}
		out = append(out, ContestBrief{
			ID: v.Id, Platform: v.Platform, ContestID: v.ContestId, ContestName: v.ContestName,
			Rank: v.Rank, ACCount: v.AcCount, TotalCount: v.TotalCount, Time: ts,
			UserID: v.UserId, UserName: v.UserName,
		})
	}
	return out
}

func (uc *SummaryUseCase) loadWeeklyReportData(ctx context.Context, coachUserId int64) (*WeeklyReportData, error) {
	email := uc.userContactEmail(coachUserId)
	if email == "" {
		return nil, fmt.Errorf("教练 %d 未绑定邮箱", coachUserId)
	}
	name := ""
	if profile := uc.userProfile(coachUserId); profile != nil {
		name = profile.Name
	}
	if name == "" {
		name = fmt.Sprintf("用户%d", coachUserId)
	}

	now := time.Now()
	// 上周：周一到周日（相对今天往前推）
	// 今天若为周一发上周报，则 end=昨天(周日)，start=昨天-6
	weekEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -1)
	weekStart := weekEnd.AddDate(0, 0, -6)
	prevEnd := weekStart.AddDate(0, 0, -1)
	prevStart := prevEnd.AddDate(0, 0, -6)

	// 全局趋势
	thisDays, err := uc.fetchHeatmap(ctx, 0, weekStart, weekEnd)
	if err != nil {
		return nil, fmt.Errorf("拉取本周全局热力图失败: %w", err)
	}
	prevDays, err := uc.fetchHeatmap(ctx, 0, prevStart, prevEnd)
	if err != nil {
		return nil, fmt.Errorf("拉取上周全局热力图失败: %w", err)
	}

	topSubmit, err := uc.fetchRank(ctx, weekStart, weekEnd, "submit", 5)
	if err != nil {
		return nil, fmt.Errorf("拉取提交排行失败: %w", err)
	}
	topAC, err := uc.fetchRank(ctx, weekStart, weekEnd, "ac", 5)
	if err != nil {
		return nil, fmt.Errorf("拉取AC排行失败: %w", err)
	}

	// 连续 3 天以上未提交：lastSubmit < weekEnd-2 天 0 点
	userIds := uc.getUserIds()
	inactive := make([]string, 0)
	if len(userIds) > 0 {
		timeMap, err := uc.fetchLastSubmitMap(ctx, userIds)
		if err != nil {
			log.Warnf("拉取最后提交时间失败: %v", err)
		} else {
			// 取用户名
			nameMap := make(map[int64]string, len(userIds))
			conn, err := uc.dialUser(ctx)
			if err == nil {
				cli := profile2.NewProfileClient(conn)
				// 分批 GetByIds
				const batch = 100
				for i := 0; i < len(userIds); i += batch {
					j := i + batch
					if j > len(userIds) {
						j = len(userIds)
					}
					var orgID int64
					if pd := auth.GetCurrentUser(ctx); pd != nil {
						orgID = int64(pd.OrgID)
					}
					res, err := cli.GetByIds(ctx, &profile2.GetByIdsReq{UserIds: userIds[i:j], OrgId: orgID})
					if err == nil {
						for _, p := range res.Profiles {
							nameMap[p.UserId] = p.Name
						}
					}
				}
			}
			threshold := weekEnd.AddDate(0, 0, -2) // 连续3天：最后提交早于 end-2 日
			type pair struct {
				id   int64
				name string
				ts   int64
			}
			cands := make([]pair, 0)
			for _, id := range userIds {
				ts, ok := timeMap[id]
				if !ok || ts == 0 {
					cands = append(cands, pair{id: id, name: nameMap[id], ts: 0})
					continue
				}
				if time.Unix(ts, 0).Before(threshold) {
					cands = append(cands, pair{id: id, name: nameMap[id], ts: ts})
				}
			}
			sort.Slice(cands, func(i, j int) bool { return cands[i].ts < cands[j].ts })
			for _, c := range cands {
				n := c.name
				if n == "" {
					n = fmt.Sprintf("用户%d", c.id)
				}
				inactive = append(inactive, n)
				if len(inactive) >= 30 {
					break
				}
			}
		}
	}

	return &WeeklyReportData{
		CoachUserID:     coachUserId,
		CoachName:       name,
		CoachEmail:      email,
		WeekStart:       weekStart.Format(dateLayout),
		WeekEnd:         weekEnd.Format(dateLayout),
		PrevWeekStart:   prevStart.Format(dateLayout),
		PrevWeekEnd:     prevEnd.Format(dateLayout),
		ThisWeekTotal:   sumDayCounts(thisDays),
		PrevWeekTotal:   sumDayCounts(prevDays),
		DailyTrend:      thisDays,
		TopSubmit:       topSubmit,
		TopAC:           topAC,
		InactiveMembers: inactive,
	}, nil
}
