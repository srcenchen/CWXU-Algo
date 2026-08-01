package platform

import (
	"bytes"
	"context"
	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NewLeetCode 力扣（leetcode.cn）爬虫
//
// 策略（submit_id 前缀约定，见 model.CountsTowardSubmitStat）：
//  1. lc-cal-*  日历每日提交次数 → 提交热力 / 提交数
//  2. lc-pad-*  生涯 totalSubmissions 与日历差量 → 补齐生涯提交（修 AC 率）
//  3. lc-prob-* 最近通过明细（有 titleSlug）→ 题库 / AI / 动态与提交历史（默认 AC，无代码）
//  4. lc-ac-*   合成 AC（acTotal 条）→ 生涯做题数；与 lc-prob 并存时总数可能略高
//
// 活动流：仅展示 lc-prob-*；合成行（lc-cal / lc-pad / lc-ac）仍过滤。
type NewLeetCode struct{}

const (
	lcGraphQL    = "https://leetcode.cn/graphql/"
	lcGraphQLNoj = "https://leetcode.cn/graphql/noj-go/"
	lcCalAPI     = "https://leetcode.cn/api/user_submission_calendar/%s/"
	// 历史 AC / 提交补齐锚到很早的日期，避免「今日」被刷成全量
	lcACBaselineDay = "2000-01-01"
)

type lcProfileResp struct {
	Data struct {
		UserProfilePublicProfile *struct {
			SubmissionProgress *struct {
				AcTotal          int `json:"acTotal"`
				TotalSubmissions int `json:"totalSubmissions"`
			} `json:"submissionProgress"`
		} `json:"userProfilePublicProfile"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type lcRecentACResp struct {
	Data struct {
		RecentACSubmissions []struct {
			SubmissionID int64  `json:"submissionId"`
			SubmitTime   int64  `json:"submitTime"`
			Lang         string `json:"lang"` // 公开 recentAC 有语言（如 C++ / Java / Python3）
			Question     *struct {
				TitleSlug          string `json:"titleSlug"`
				Title              string `json:"title"`
				TranslatedTitle    string `json:"translatedTitle"`
				QuestionFrontendID string `json:"questionFrontendId"`
			} `json:"question"`
		} `json:"recentACSubmissions"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type lcProgress struct {
	AcTotal          int
	TotalSubmissions int
}

type lcRecentAC struct {
	SubmissionID int64
	SubmitTime   time.Time
	TitleSlug    string
	Title        string // 展示用（中文优先；含 frontendId 前缀）
	Lang         string
	FrontendID   string
}

func (p NewLeetCode) Name() string {
	return spider.LeetCode
}

func (p NewLeetCode) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if username == "" {
		return nil, fmt.Errorf("leetcode username 为空")
	}

	cal, err := fetchLeetCodeCalendar(ctx, username)
	if err != nil {
		return nil, err
	}
	prog, err := fetchLeetCodeProgress(ctx, username)
	if err != nil {
		return nil, err
	}
	// 最近通过失败不阻断热力/总数；题库侧只是少几题
	recent, recentErr := fetchLeetCodeRecentAC(ctx, username)
	if recentErr != nil {
		recent = nil
	}

	now := time.Now()
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
	baselineDay, _ := time.ParseInLocation("2006-01-02", lcACBaselineDay, loc)
	baselineDay = time.Date(baselineDay.Year(), baselineDay.Month(), baselineDay.Day(), 12, 0, 0, 0, loc)

	// 增量：只关心最近几天日历；更早的日历记录若已入库会因 submit_id 冲突被忽略
	cutoff := time.Time{}
	if !needAll {
		cutoff = today.AddDate(0, 0, -14)
	}

	res := make([]model.SubmitLog, 0, 512+len(recent))
	calSum := 0

	// 1) 日历 → 每日提交次数（status 不含 AC，只进「提交热力图 / 提交次数」）
	for dayUnix, cnt := range cal {
		if cnt <= 0 {
			continue
		}
		day := time.Unix(dayUnix, 0).In(time.UTC)
		dayLocal := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc)
		calSum += cnt
		if !needAll && dayLocal.Before(cutoff) {
			// 增量不产出旧日历行，但仍计入 calSum 以便 pad 正确
			// 注意：增量时 pad 依赖全量 calSum，所以仍要扫完全部日历计数
			continue
		}
		for i := 0; i < cnt; i++ {
			res = append(res, model.SubmitLog{
				UserID:   userId,
				Platform: spider.LeetCode,
				SubmitID: fmt.Sprintf("lc-cal-%d-%s-%d", userId, dayLocal.Format("20060102"), i),
				Contest:  "leetcode",
				Problem:  "leetcode-submit",
				Lang:     "LeetCode",
				Status:   "SUBMIT",
				Time:     dayLocal.Add(time.Duration(i) * time.Second),
			})
		}
	}

	// 增量路径：上面 continue 了旧日，calSum 仍是全量；但 pad 需要全量 cal 之和
	// 若 needAll=false 时我们 continue 前已 calSum+=cnt，OK

	// 2) 生涯提交补齐：totalSubmissions - 日历合计 → baseline 日 SUBMIT
	//    修 AC 率：分子生涯 AC 题数 / 分母生涯提交（不再只剩近一年日历）
	pad := prog.TotalSubmissions - calSum
	if pad < 0 {
		pad = 0
	}
	for i := 0; i < pad; i++ {
		res = append(res, model.SubmitLog{
			UserID:   userId,
			Platform: spider.LeetCode,
			SubmitID: fmt.Sprintf("lc-pad-%d-%d", userId, i),
			Contest:  "leetcode",
			Problem:  "leetcode-submit",
			Lang:     "LeetCode",
			Status:   "SUBMIT",
			Time:     baselineDay.Add(time.Duration(i%60) * time.Second),
		})
	}

	// 3) 最近通过 → 真实题级 AC（进题库 / 动态 / 提交历史；不计提交数以免与日历双计）
	//    公开接口常对同一题返回多次 AC → 先按 submissionId / titleSlug 去重（保留最新）。
	//    无源码 → 状态固定 AC；语言取公开字段 lang（旧实现误写死 "-"）。
	//    Problem 形如 "{titleSlug} {frontendId}. {title}"，首段供 parseLeetCode 取 slug
	//   （LCR 题 titleSlug 为 iIQa4I 等短码，须保留；展示优先靠 frontendId / 题库回填）。
	for _, r := range dedupeLeetCodeRecentAC(recent) {
		problem, lang := formatLeetCodeRecentProblem(r)
		res = append(res, model.SubmitLog{
			UserID:     userId,
			Platform:   spider.LeetCode,
			SubmitID:   fmt.Sprintf("lc-prob-%d", r.SubmissionID),
			Contest:    "leetcode",
			Problem:    problem,
			ExternalID: r.TitleSlug,
			Lang:       lang,
			Status:     "AC",
			Time:       r.SubmitTime,
		})
	}

	// 4) 累计 AC 题数 → 合成 AC（仅对齐生涯 acTotal / user_ac_problems）
	//    不进日 AC 热力、不进 user_ac_problem_days、不进动态（见 CountsTowardDailyAC）。
	//    力扣接口只给已去重的 acTotal；稳定 submit_id；时间锚 baseline（增量也不标今天，避免虚增「今日 AC」）。
	for i := 0; i < prog.AcTotal; i++ {
		t := baselineDay
		ext := fmt.Sprintf("ac-%d", i)
		res = append(res, model.SubmitLog{
			UserID:     userId,
			Platform:   spider.LeetCode,
			SubmitID:   fmt.Sprintf("lc-ac-%d-%d", userId, i),
			Contest:    "leetcode",
			Problem:    fmt.Sprintf("lc-ac-problem-%d", i),
			ExternalID: ext,
			Lang:       "LeetCode",
			Status:     "AC",
			Time:       t.Add(time.Duration(i%60) * time.Second),
		})
	}

	return res, nil
}

func fetchLeetCodeCalendar(ctx context.Context, username string) (map[int64]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	url := fmt.Sprintf(lcCalAPI, username)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setLCHeaders(req, username)

	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, fmt.Errorf("leetcode calendar 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("leetcode calendar 读 body 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("leetcode calendar 状态码 %d: %s", resp.StatusCode, string(body))
	}

	// 接口有时直接返回 object，有时返回 JSON 字符串
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("leetcode calendar 解析失败: %w", err)
	}
	var obj map[string]int
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("leetcode calendar 字符串解包失败: %w", err)
		}
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil, fmt.Errorf("leetcode calendar 内层解析失败: %w", err)
		}
	} else {
		var tmp map[string]int
		if err := json.Unmarshal(raw, &tmp); err != nil {
			return nil, fmt.Errorf("leetcode calendar object 解析失败: %w", err)
		}
		obj = tmp
	}

	out := make(map[int64]int, len(obj))
	for k, v := range obj {
		ts, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue
		}
		out[ts] = v
	}
	return out, nil
}

func fetchLeetCodeProgress(ctx context.Context, username string) (lcProgress, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload := map[string]interface{}{
		"query": `query userPublicProfile($userSlug: String!) {
			userProfilePublicProfile(userSlug: $userSlug) {
				submissionProgress { acTotal totalSubmissions }
			}
		}`,
		"variables": map[string]string{"userSlug": username},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lcGraphQL, bytes.NewReader(b))
	if err != nil {
		return lcProgress{}, err
	}
	setLCHeaders(req, username)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return lcProgress{}, fmt.Errorf("leetcode profile 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return lcProgress{}, fmt.Errorf("leetcode profile 读 body 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return lcProgress{}, fmt.Errorf("leetcode profile 状态码 %d: %s", resp.StatusCode, string(body))
	}

	var pr lcProfileResp
	if err := json.Unmarshal(body, &pr); err != nil {
		return lcProgress{}, fmt.Errorf("leetcode profile 解析失败: %w", err)
	}
	if len(pr.Errors) > 0 {
		return lcProgress{}, fmt.Errorf("leetcode profile graphql 错误: %s", pr.Errors[0].Message)
	}
	if pr.Data.UserProfilePublicProfile == nil || pr.Data.UserProfilePublicProfile.SubmissionProgress == nil {
		return lcProgress{}, fmt.Errorf("leetcode 用户不存在或资料不可见: %s", username)
	}
	sp := pr.Data.UserProfilePublicProfile.SubmissionProgress
	return lcProgress{
		AcTotal:          sp.AcTotal,
		TotalSubmissions: sp.TotalSubmissions,
	}, nil
}

// formatLeetCodeRecentProblem 组装 lc-prob 的 Problem / Lang。
// Problem 必须以 titleSlug 开头，供 parseLeetCode 解析 external_id；
// 有 questionFrontendId 时写入「LCR 038. 每日温度」式可读标题，避免动态里只剩 iIQa4I。
func formatLeetCodeRecentProblem(r lcRecentAC) (problem, lang string) {
	title := r.Title
	if title == "" {
		title = r.TitleSlug
	}
	if r.FrontendID != "" {
		title = r.FrontendID + ". " + title
	}
	lang = strings.TrimSpace(r.Lang)
	if lang == "" {
		lang = "-"
	}
	return fmt.Sprintf("%s %s", r.TitleSlug, title), lang
}

// dedupeLeetCodeRecentAC 同一批最近通过：submissionId 去重 + 同一 titleSlug 只留最新一条
// API 列表通常已按时间倒序；同 slug 保留先出现的（更新）。
func dedupeLeetCodeRecentAC(in []lcRecentAC) []lcRecentAC {
	if len(in) == 0 {
		return nil
	}
	seenID := make(map[int64]struct{}, len(in))
	seenSlug := make(map[string]struct{}, len(in))
	out := make([]lcRecentAC, 0, len(in))
	for _, r := range in {
		if r.TitleSlug == "" || r.SubmissionID == 0 {
			continue
		}
		if _, ok := seenID[r.SubmissionID]; ok {
			continue
		}
		if _, ok := seenSlug[r.TitleSlug]; ok {
			continue
		}
		seenID[r.SubmissionID] = struct{}{}
		seenSlug[r.TitleSlug] = struct{}{}
		out = append(out, r)
	}
	return out
}

func fetchLeetCodeRecentAC(ctx context.Context, username string) ([]lcRecentAC, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload := map[string]interface{}{
		"query": `query recentACSubmissions($userSlug: String!) {
			recentACSubmissions(userSlug: $userSlug) {
				submissionId
				submitTime
				lang
				question {
					titleSlug
					title
					translatedTitle
					questionFrontendId
				}
			}
		}`,
		"variables": map[string]string{"userSlug": username},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lcGraphQLNoj, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	setLCHeaders(req, username)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, fmt.Errorf("leetcode recentAC 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("leetcode recentAC 读 body 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("leetcode recentAC 状态码 %d: %s", resp.StatusCode, string(body))
	}

	var pr lcRecentACResp
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("leetcode recentAC 解析失败: %w", err)
	}
	if len(pr.Errors) > 0 {
		return nil, fmt.Errorf("leetcode recentAC graphql 错误: %s", pr.Errors[0].Message)
	}

	out := make([]lcRecentAC, 0, len(pr.Data.RecentACSubmissions))
	for _, item := range pr.Data.RecentACSubmissions {
		if item.Question == nil || item.Question.TitleSlug == "" {
			continue
		}
		title := item.Question.TranslatedTitle
		if title == "" {
			title = item.Question.Title
		}
		// submitTime 为 Unix 秒
		t := time.Unix(item.SubmitTime, 0)
		if item.SubmitTime <= 0 {
			t = time.Now()
		}
		out = append(out, lcRecentAC{
			SubmissionID: item.SubmissionID,
			SubmitTime:   t,
			TitleSlug:    item.Question.TitleSlug,
			Title:        title,
			Lang:         strings.TrimSpace(item.Lang),
			FrontendID:   strings.TrimSpace(item.Question.QuestionFrontendID),
		})
	}
	return out, nil
}

func setLCHeaders(req *http.Request, username string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Referer", fmt.Sprintf("https://leetcode.cn/u/%s/", username))
	req.Header.Set("Origin", "https://leetcode.cn")
	req.Header.Set("Accept", "application/json, text/plain, */*")
}

// FetchRating 力扣竞赛 rating（noj-go GraphQL；浮点四舍五入为整数）
func (p NewLeetCode) FetchRating(username string) (int, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, false, fmt.Errorf("leetcode username 为空")
	}
	payload := map[string]interface{}{
		"query": `query userContestRanking($userSlug: String!) {
			userContestRanking(userSlug: $userSlug) { rating }
		}`,
		"variables": map[string]string{"userSlug": username},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, lcGraphQLNoj, bytes.NewReader(b))
	if err != nil {
		return 0, false, err
	}
	setLCHeaders(req, username)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("leetcode rating 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("leetcode rating 状态码 %d: %s", resp.StatusCode, string(body))
	}
	var pr struct {
		Data struct {
			UserContestRanking *struct {
				Rating float64 `json:"rating"`
			} `json:"userContestRanking"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, false, fmt.Errorf("leetcode rating 解析失败: %w", err)
	}
	if len(pr.Errors) > 0 {
		return 0, false, fmt.Errorf("leetcode rating graphql: %s", pr.Errors[0].Message)
	}
	// 未参加过竞赛时 userContestRanking 为 null
	if pr.Data.UserContestRanking == nil {
		return 0, false, nil
	}
	return int(pr.Data.UserContestRanking.Rating + 0.5), true, nil
}

// lcContestHistoryItem 公开 GraphQL userContestRankingHistory 单条（仅映射用字段）
type lcContestHistoryItem struct {
	Attended      bool `json:"attended"`
	Ranking       int  `json:"ranking"`
	Score         int  `json:"score"`
	TotalProblems int  `json:"totalProblems"`
	Contest       *struct {
		Title     string `json:"title"`
		TitleCn   string `json:"titleCn"`
		StartTime int64  `json:"startTime"`
	} `json:"contest"`
}

type lcContestHistoryResp struct {
	Data struct {
		// null：用户不存在 / 从未参赛
		UserContestRankingHistory []lcContestHistoryItem `json:"userContestRankingHistory"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// leetCodeContestSlug 从中文/英文周赛标题推导 titleSlug（ContestNode 无 titleSlug 字段）
// 例：第 365 场周赛 → weekly-contest-365；第 160 场双周赛 → biweekly-contest-160
func leetCodeContestSlug(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	// 双周赛须先匹配（含「周赛」子串）
	reBiCN := regexp.MustCompile(`第\s*(\d+)\s*场双周赛`)
	if m := reBiCN.FindStringSubmatch(title); len(m) == 2 {
		return "biweekly-contest-" + m[1]
	}
	reWkCN := regexp.MustCompile(`第\s*(\d+)\s*场周赛`)
	if m := reWkCN.FindStringSubmatch(title); len(m) == 2 {
		return "weekly-contest-" + m[1]
	}
	reBiEN := regexp.MustCompile(`(?i)Biweekly\s+Contest\s+(\d+)`)
	if m := reBiEN.FindStringSubmatch(title); len(m) == 2 {
		return "biweekly-contest-" + m[1]
	}
	reWkEN := regexp.MustCompile(`(?i)Weekly\s+Contest\s+(\d+)`)
	if m := reWkEN.FindStringSubmatch(title); len(m) == 2 {
		return "weekly-contest-" + m[1]
	}
	return ""
}

// mapLeetCodeContestHistory 将 GraphQL history 映射为 ContestLog（纯函数，便于 fixture 单测）
// 仅保留 attended=true；needAll=false 时只保留 startTime 最新的 lcContestRecentLimit 条。
func mapLeetCodeContestHistory(userId int64, items []lcContestHistoryItem, needAll bool) []model.ContestLog {
	const lcContestRecentLimit = 10
	out := make([]model.ContestLog, 0, len(items))
	for _, it := range items {
		if !it.Attended || it.Contest == nil {
			continue
		}
		title := strings.TrimSpace(it.Contest.TitleCn)
		if title == "" {
			title = strings.TrimSpace(it.Contest.Title)
		}
		if title == "" {
			continue
		}
		slug := leetCodeContestSlug(it.Contest.Title)
		if slug == "" {
			slug = leetCodeContestSlug(it.Contest.TitleCn)
		}
		contestID := slug
		contestURL := ""
		if contestID == "" {
			// 特殊赛名无法推导 slug：用 startTime 保证 (user_id, contest_id) 稳定
			if it.Contest.StartTime <= 0 {
				continue
			}
			contestID = fmt.Sprintf("lc-%d", it.Contest.StartTime)
		} else {
			contestURL = "https://leetcode.cn/contest/" + contestID + "/"
		}
		var t time.Time
		if it.Contest.StartTime > 0 {
			t = time.Unix(it.Contest.StartTime, 0)
		}
		out = append(out, model.ContestLog{
			Platform:    spider.LeetCode,
			UserID:      userId,
			ContestId:   contestID,
			ContestName: title,
			ContestUrl:  contestURL,
			Rank:        it.Ranking,
			// totalProblems 为场次题数；力扣无 AC 数，用 score 写入 AcCount 供站内榜展示
			TotalCount: it.TotalProblems,
			AcCount:    it.Score,
			Time:       t,
		})
	}
	// 按开始时间降序，便于增量只取最近
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Time.After(out[j].Time)
	})
	if !needAll && len(out) > lcContestRecentLimit {
		out = out[:lcContestRecentLimit]
	}
	return out
}

// FetchContestLog 力扣竞赛参赛历史 → contest_logs（公开 noj-go GraphQL）
// 未参赛 / history 为 null → 空切片、无错误。
func (p NewLeetCode) FetchContestLog(userId int64, username string, needAll bool) ([]model.ContestLog, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("leetcode username 为空")
	}
	payload := map[string]interface{}{
		"query": `query userContestRankingHistory($userSlug: String!) {
			userContestRankingHistory(userSlug: $userSlug) {
				attended
				ranking
				score
				totalProblems
				contest {
					title
					titleCn
					startTime
				}
			}
		}`,
		"variables": map[string]string{"userSlug": username},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, lcGraphQLNoj, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	setLCHeaders(req, username)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, fmt.Errorf("leetcode contest history 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("leetcode contest history 读 body 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("leetcode contest history 状态码 %d: %s", resp.StatusCode, string(body))
	}

	var pr lcContestHistoryResp
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("leetcode contest history 解析失败: %w", err)
	}
	if len(pr.Errors) > 0 {
		return nil, fmt.Errorf("leetcode contest history graphql: %s", pr.Errors[0].Message)
	}
	// null / 空：从未参赛或用户不可见 → 成功空结果
	if pr.Data.UserContestRankingHistory == nil {
		return []model.ContestLog{}, nil
	}
	return mapLeetCodeContestHistory(userId, pr.Data.UserContestRankingHistory, needAll), nil
}

// FetchContestDetails 用公开 ranking API（按 history.rank 定位页）拉每题 AC 时间与 fail_count。
func (p NewLeetCode) FetchContestDetails(userId int64, username string, needAll bool) ([]spider.ContestProblemCell, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("leetcode username 为空")
	}
	// 先取 history（含 ranking / startTime / slug）
	payload := map[string]interface{}{
		"query": `query userContestRankingHistory($userSlug: String!) {
			userContestRankingHistory(userSlug: $userSlug) {
				attended
				ranking
				score
				totalProblems
				contest { title titleCn startTime }
			}
		}`,
		"variables": map[string]string{"userSlug": username},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, lcGraphQLNoj, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	setLCHeaders(req, username)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var pr lcContestHistoryResp
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, err
	}
	if pr.Data.UserContestRankingHistory == nil {
		return nil, nil
	}
	items := pr.Data.UserContestRankingHistory
	// 与 map 一致：只 attended；needAll=false 最近 10 场
	type attended struct {
		slug    string
		rank    int
		start   int64
		title   string
	}
	var list []attended
	for _, it := range items {
		if !it.Attended || it.Contest == nil {
			continue
		}
		title := strings.TrimSpace(it.Contest.TitleCn)
		if title == "" {
			title = strings.TrimSpace(it.Contest.Title)
		}
		slug := leetCodeContestSlug(it.Contest.Title)
		if slug == "" {
			slug = leetCodeContestSlug(it.Contest.TitleCn)
		}
		if slug == "" {
			continue
		}
		list = append(list, attended{slug: slug, rank: it.Ranking, start: it.Contest.StartTime, title: title})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].start > list[j].start })
	if !needAll && len(list) > 10 {
		list = list[:10]
	}

	out := make([]spider.ContestProblemCell, 0, len(list)*4)
	for i, a := range list {
		if i > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		cells, err := fetchLeetCodeUserContestCells(a.slug, username, a.rank, a.start)
		if err != nil {
			// 单场失败不阻断
			continue
		}
		out = append(out, cells...)
	}
	_ = userId
	return out, nil
}

// fetchLeetCodeUserContestCells 按 rank 估算页码，从 ranking API 取该用户 submissions。
func fetchLeetCodeUserContestCells(slug, userSlug string, rank int, contestStart int64) ([]spider.ContestProblemCell, error) {
	slug = strings.TrimSpace(slug)
	userSlug = strings.TrimSpace(userSlug)
	if slug == "" || userSlug == "" {
		return nil, fmt.Errorf("empty slug/user")
	}
	const pageSize = 25
	// rank 可能是 0-based 或 1-based；多试相邻页
	pages := []int{1}
	if rank > 0 {
		// rank_v2 多为从 1 开始
		p := (rank-1)/pageSize + 1
		if p < 1 {
			p = 1
		}
		pages = []int{p, p - 1, p + 1, 1}
	}
	seenPage := map[int]struct{}{}
	for _, page := range pages {
		if page < 1 {
			continue
		}
		if _, ok := seenPage[page]; ok {
			continue
		}
		seenPage[page] = struct{}{}
		cells, found, err := fetchLeetCodeRankingPage(slug, userSlug, page, contestStart)
		if err != nil {
			continue
		}
		if found {
			return cells, nil
		}
	}
	return nil, fmt.Errorf("leetcode ranking: user %s not found in nearby pages for %s", userSlug, slug)
}

func fetchLeetCodeRankingPage(slug, userSlug string, page int, contestStart int64) (cells []spider.ContestProblemCell, found bool, err error) {
	url := fmt.Sprintf(
		"https://leetcode.cn/contest/api/ranking/%s/?pagination=%d&region=local",
		slug, page,
	)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoAlgo/1.0)")
	req.Header.Set("Referer", "https://leetcode.cn/contest/"+slug+"/")
	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		Questions []struct {
			TitleSlug string `json:"title_slug"`
			Credit    int    `json:"credit"`
			// question_id 与 submissions 的 key 可能是 question_id
			QuestionID int `json:"question_id"`
			ID         int `json:"id"`
		} `json:"questions"`
		TotalRank []struct {
			UserSlug string `json:"user_slug"`
			Username string `json:"username"`
			Score    int    `json:"score"`
			RankV2   int    `json:"rank_v2"`
		} `json:"total_rank"`
		Submissions []map[string]struct {
			Date       int64 `json:"date"`
			FailCount  int   `json:"fail_count"`
			Status     int   `json:"status"`
			QuestionID int   `json:"question_id"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false, err
	}
	// question_id / id → title_slug + credit + label
	type qmeta struct {
		slug   string
		credit int
		label  string
	}
	qidMap := map[string]qmeta{}
	for i, q := range out.Questions {
		label := string(rune('A' + i))
		if i >= 26 {
			label = strconv.Itoa(i + 1)
		}
		ts := strings.TrimSpace(q.TitleSlug)
		meta := qmeta{slug: ts, credit: q.Credit, label: label}
		if q.QuestionID > 0 {
			qidMap[strconv.Itoa(q.QuestionID)] = meta
		}
		if q.ID > 0 {
			qidMap[strconv.Itoa(q.ID)] = meta
		}
		// 也用 title_slug 作 key（部分响应）
		if ts != "" {
			qidMap[ts] = meta
		}
	}

	userSlugLower := strings.ToLower(userSlug)
	for i, r := range out.TotalRank {
		if strings.ToLower(strings.TrimSpace(r.UserSlug)) != userSlugLower &&
			strings.ToLower(strings.TrimSpace(r.Username)) != userSlugLower {
			continue
		}
		if i >= len(out.Submissions) {
			return nil, true, nil
		}
		subMap := out.Submissions[i]
		cells = make([]spider.ContestProblemCell, 0, len(subMap))
		for key, sub := range subMap {
			meta, ok := qidMap[key]
			if !ok {
				meta, ok = qidMap[strconv.Itoa(sub.QuestionID)]
			}
			if !ok || meta.slug == "" {
				// 仍写入，external 用 key
				meta = qmeta{slug: key, label: key}
			}
			// status 10 = AC
			if sub.Status != 10 && sub.Date == 0 {
				continue
			}
			cell := spider.ContestProblemCell{
				ContestID:  slug,
				Label:      meta.label,
				ExternalID: meta.slug,
				Attempts:   sub.FailCount,
				ScoreDelta: meta.credit,
				Status:     model.ContestCellAC,
			}
			if sub.Date > 0 {
				t := time.Unix(sub.Date, 0)
				cell.FirstACAt = &t
				if contestStart > 0 && sub.Date >= contestStart {
					rel := int(sub.Date - contestStart)
					cell.RelativeSec = &rel
				}
			}
			cells = append(cells, cell)
		}
		return cells, true, nil
	}
	return nil, false, nil
}

func init() {
	spider.Register(NewLeetCode{})
}
