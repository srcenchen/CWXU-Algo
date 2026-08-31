package platform

import (
	"context"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type NewCodeforces struct{}

const cfGymContestIDMin = 100000

func isCFGymContestID(id int) bool {
	return id >= cfGymContestIDMin
}

func canonicalCFContestID(id int) int {
	if isCFGymContestID(id) {
		return -id
	}
	return id
}

func cfContestURL(id int) string {
	if id < 0 {
		return "https://codeforces.com/gym/" + strconv.Itoa(-id)
	}
	return "https://codeforces.com/contest/" + strconv.Itoa(id)
}

func cfPositiveContestID(id int) int {
	if id < 0 {
		return -id
	}
	return id
}

type CFResponse struct {
	Status string   `json:"status"`
	Result []cfJson `json:"result"`
}

type cfJson struct {
	ID        int `json:"id"`
	ContestID int `json:"contestId"`
	Problem   struct {
		Index string `json:"index"`
		Name  string `json:"name"`
	} `json:"problem"`
	ProgrammingLanguage string `json:"programmingLanguage"`
	Verdict             string `json:"verdict"`
	CreationTimeSeconds int64  `json:"creationTimeSeconds"`
	Author              struct {
		ParticipantType string `json:"participantType"`
	} `json:"author"`
}

const (
	cfStatusPageSize    = 1000
	cfStatusMaxPagesAll = 100 // needAll 硬顶 10 万条，避免 1e6 单包
	cfStatusPageGap     = 200 * time.Millisecond
)

// 短缓存：同用户一次 LoadData 内 submit+contest 复用同一份 user.status
type cfStatusCacheEntry struct {
	at   time.Time
	subs []cfJson
}

var (
	cfStatusCacheMu sync.Mutex
	cfStatusCache   = map[string]cfStatusCacheEntry{}
)

func cfStatusCacheKey(username string, needAll bool) string {
	if needAll {
		return strings.ToLower(strings.TrimSpace(username)) + ":all"
	}
	return strings.ToLower(strings.TrimSpace(username)) + ":incr"
}

type cfRatingEntry struct {
	ContestID               int    `json:"contestId"`
	ContestName             string `json:"contestName"`
	Handle                  string `json:"handle"`
	Rank                    int    `json:"rank"`
	RatingUpdateTimeSeconds int64  `json:"ratingUpdateTimeSeconds"`
	OldRating               int    `json:"oldRating"`
	NewRating               int    `json:"newRating"`
}

type cfContestListEntry struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	Phase            string `json:"phase"`
	DurationSeconds  int64  `json:"durationSeconds"`
	StartTimeSeconds int64  `json:"startTimeSeconds"`
}

func (p NewCodeforces) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) (res []model.SubmitLog, err error) {
	subs, err := fetchCFUserStatusPaged(ctx, username, needAll)
	if err != nil {
		return nil, err
	}
	res = make([]model.SubmitLog, 0, len(subs))
	for _, sub := range subs {
		res = append(res, model.SubmitLog{
			UserID:   userId,
			Platform: spider.CodeForces,
			SubmitID: strconv.Itoa(sub.ID),
			Contest:  strconv.Itoa(canonicalCFContestID(sub.ContestID)),
			Problem:  fmt.Sprintf("%s-%s", sub.Problem.Index, sub.Problem.Name),
			Lang:     sub.ProgrammingLanguage,
			// CF 评测中可能省略 verdict → 空串；归一化后写入，避免 UI 显示空白
			Status: NormalizeCodeforcesVerdict(sub.Verdict),
			Time:   time.Unix(sub.CreationTimeSeconds, 0),
		})
	}
	return res, nil
}

// fetchCFUserStatusPaged 分页拉取 user.status；短缓存供 submit/contest 复用。
// needAll=false：仅第 1 页（最多 1000）；needAll=true：分页直至不足一页或达硬顶。
func fetchCFUserStatusPaged(ctx context.Context, username string, needAll bool) ([]cfJson, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("codeforces handle 为空")
	}
	key := cfStatusCacheKey(username, needAll)
	cfStatusCacheMu.Lock()
	if e, ok := cfStatusCache[key]; ok && time.Since(e.at) < 2*time.Minute {
		out := e.subs
		cfStatusCacheMu.Unlock()
		return out, nil
	}
	cfStatusCacheMu.Unlock()

	maxPages := 1
	if needAll {
		maxPages = cfStatusMaxPagesAll
	}
	all := make([]cfJson, 0, cfStatusPageSize)
	handleQ := url.QueryEscape(username)
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		from := page*cfStatusPageSize + 1
		path := fmt.Sprintf(
			"/api/user.status?handle=%s&from=%d&count=%d",
			handleQ, from, cfStatusPageSize,
		)
		body, _, err := cfGetJSON(ctx, path)
		if err != nil {
			return nil, err
		}
		var cfResp CFResponse
		if err := json.Unmarshal(body, &cfResp); err != nil {
			return nil, fmt.Errorf("解析json错误：%s", err.Error())
		}
		if cfResp.Status != "OK" {
			return nil, fmt.Errorf("API status error: %s", cfResp.Status)
		}
		all = append(all, cfResp.Result...)
		if len(cfResp.Result) < cfStatusPageSize {
			break
		}
		if page+1 < maxPages {
			time.Sleep(cfStatusPageGap)
		}
	}

	cfStatusCacheMu.Lock()
	cfStatusCache[key] = cfStatusCacheEntry{at: time.Now(), subs: all}
	// 简单防膨胀：超过 64 条清掉过期项
	if len(cfStatusCache) > 64 {
		for k, e := range cfStatusCache {
			if time.Since(e.at) > 2*time.Minute {
				delete(cfStatusCache, k)
			}
		}
	}
	cfStatusCacheMu.Unlock()
	return all, nil
}

// FetchContestLog 拉取 Codeforces 比赛记录。
//
// HTML 页面 /contests/with/{handle} 会被 Cloudflare 拦截，改走官方 API：
//  1. user.rating → 官方排名 / 比赛名 / 结算时间（仅 rated 且已出分）
//  2. user.status → 按 contestId 统计正式参赛 (CONTESTANT/OUT_OF_COMPETITION) 的 unique OK 作为 AC
//  3. 刚结束尚未出分、或 unrated：rank=0，仍写入 AC；站内榜可按 AC 模拟排名
func (p NewCodeforces) FetchContestLog(userId int64, username string, needAll bool) ([]model.ContestLog, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("codeforces handle 为空")
	}

	ratings, err := fetchCFUserRating(username)
	if err != nil {
		return nil, err
	}
	acByContest, participateTime, err := fetchCFContestACFromStatus(username, needAll)
	if err != nil {
		return nil, err
	}

	// contestId → 合并后的日志
	type draft struct {
		rank     int
		name     string
		ac       int
		timeUnix int64
		fromRate bool
	}
	merged := map[int]*draft{}

	for _, r := range ratings {
		if r.ContestID <= 0 {
			continue
		}
		cid := canonicalCFContestID(r.ContestID)
		d := merged[cid]
		if d == nil {
			d = &draft{}
			merged[cid] = d
		}
		d.rank = r.Rank
		d.name = strings.TrimSpace(r.ContestName)
		d.timeUnix = r.RatingUpdateTimeSeconds
		d.fromRate = true
	}

	for cid, ac := range acByContest {
		d := merged[cid]
		if d == nil {
			d = &draft{}
			merged[cid] = d
		}
		d.ac = ac
		if d.timeUnix == 0 {
			d.timeUnix = participateTime[cid]
		}
	}

	// 仅有 rating、status 窗口未覆盖到的场次：AC 可能为 0（增量时常见）
	// 需要补比赛名的 id
	needMeta := make([]int, 0)
	for cid, d := range merged {
		if d.name == "" {
			needMeta = append(needMeta, cid)
		}
	}
	meta := map[int]cfContestListEntry{}
	if len(needMeta) > 0 {
		if m, mErr := fetchCFContestListMap(); mErr == nil {
			meta = m
		}
	}

	shZone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		shZone = time.FixedZone("CST", 8*3600)
	}

	ids := make([]int, 0, len(merged))
	for cid := range merged {
		ids = append(ids, cid)
	}
	// 新→旧，便于 needAll=false 截断
	sort.Slice(ids, func(i, j int) bool {
		ti, tj := merged[ids[i]].timeUnix, merged[ids[j]].timeUnix
		if ti != tj {
			return ti > tj
		}
		return ids[i] > ids[j]
	})

	limit := len(ids)
	if !needAll && limit > 15 {
		limit = 15
	}

	out := make([]model.ContestLog, 0, limit)
	for _, cid := range ids[:limit] {
		d := merged[cid]
		name := d.name
		var t time.Time
		if d.timeUnix > 0 {
			t = time.Unix(d.timeUnix, 0).In(shZone)
		}
		if name == "" {
			if m, ok := meta[cfPositiveContestID(cid)]; ok {
				name = strings.TrimSpace(m.Name)
				if t.IsZero() && m.StartTimeSeconds > 0 {
					t = time.Unix(m.StartTimeSeconds, 0).In(shZone)
				}
			}
		}
		if name == "" {
			name = fmt.Sprintf("Codeforces Contest %d", cid)
		}
		idStr := strconv.Itoa(cid)
		contestID := idStr
		if cid < 0 {
			contestID = strconv.Itoa(-cid)
		}
		out = append(out, model.ContestLog{
			Platform:    spider.CodeForces,
			UserID:      userId,
			ContestId:   contestID,
			ContestName: name,
			ContestUrl:  cfContestURL(cid),
			Rank:        d.rank,
			AcCount:     d.ac,
			TotalCount:  0,
			Time:        t,
		})
	}
	return out, nil
}

func fetchCFUserRating(username string) ([]cfRatingEntry, error) {
	path := fmt.Sprintf("/api/user.rating?handle=%s", url.QueryEscape(username))
	body, _, err := cfGetJSON(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("codeforces user.rating 请求失败: %w", err)
	}
	var out struct {
		Status  string          `json:"status"`
		Comment string          `json:"comment"`
		Result  []cfRatingEntry `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("codeforces user.rating 解析失败: %w", err)
	}
	if out.Status != "OK" {
		// 未参赛用户 comment 类似 "handles: User with handle ... not found" 或空 result
		if strings.Contains(strings.ToLower(out.Comment), "not found") {
			return nil, fmt.Errorf("codeforces user.rating: %s", out.Comment)
		}
		// 无 rating 历史：空列表
		if len(out.Result) == 0 {
			return []cfRatingEntry{}, nil
		}
		return nil, fmt.Errorf("codeforces user.rating API: %s %s", out.Status, out.Comment)
	}
	return out.Result, nil
}

// fetchCFContestACFromStatus 从 user.status 统计正式参赛过题数与最早提交时间。
// 复用 fetchCFUserStatusPaged 缓存，避免与 FetchSubmitLog 重复拉 1e6。
// 返回：acByContest[contestId]=unique OK 数；participateTime[contestId]=最早提交 unix。
func fetchCFContestACFromStatus(username string, needAll bool) (map[int]int, map[int]int64, error) {
	subs, err := fetchCFUserStatusPaged(context.Background(), username, needAll)
	if err != nil {
		return nil, nil, err
	}

	acProblems := map[int]map[string]struct{}{}
	participateTime := map[int]int64{}
	for _, s := range subs {
		if s.ContestID <= 0 {
			continue
		}
		pt := strings.ToUpper(strings.TrimSpace(s.Author.ParticipantType))
		// 正式参赛 / 非官方分区；练习与虚拟赛不计入比赛榜
		if pt != "CONTESTANT" && pt != "OUT_OF_COMPETITION" {
			continue
		}
		cid := canonicalCFContestID(s.ContestID)
		if t, ok := participateTime[cid]; !ok || (s.CreationTimeSeconds > 0 && s.CreationTimeSeconds < t) {
			if s.CreationTimeSeconds > 0 {
				participateTime[cid] = s.CreationTimeSeconds
			}
		}
		if !strings.EqualFold(strings.TrimSpace(s.Verdict), "OK") {
			continue
		}
		idx := strings.TrimSpace(s.Problem.Index)
		if idx == "" {
			continue
		}
		set := acProblems[cid]
		if set == nil {
			set = map[string]struct{}{}
			acProblems[cid] = set
		}
		set[idx] = struct{}{}
	}
	acByContest := make(map[int]int, len(acProblems))
	for cid, set := range acProblems {
		acByContest[cid] = len(set)
	}
	// 参赛但 0 AC 也要有记录（rank 可能来自 rating）
	for cid := range participateTime {
		if _, ok := acByContest[cid]; !ok {
			acByContest[cid] = 0
		}
	}
	return acByContest, participateTime, nil
}

// FetchContestDetails 正式参赛提交 → 每题 AC/TRIED + 尝试次数 + 首次 AC 相对开赛时间。
func (p NewCodeforces) FetchContestDetails(userId int64, username string, needAll bool) ([]spider.ContestProblemCell, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("codeforces handle 为空")
	}
	subs, err := fetchCFUserStatusPaged(context.Background(), username, needAll)
	if err != nil {
		return nil, err
	}
	startBy := map[int]int64{}
	if meta, mErr := fetchCFContestListMap(); mErr == nil {
		for id, e := range meta {
			if e.StartTimeSeconds > 0 {
				startBy[id] = e.StartTimeSeconds
			}
		}
	}

	// contestId -> index -> agg
	type agg struct {
		attempts int
		ac       bool
		firstAC  int64
		label    string
	}
	byContest := map[int]map[string]*agg{}

	// 按时间升序，便于统计 AC 前尝试
	sorted := make([]cfJson, len(subs))
	copy(sorted, subs)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreationTimeSeconds < sorted[j].CreationTimeSeconds
	})

	for _, s := range sorted {
		if s.ContestID <= 0 {
			continue
		}
		pt := strings.ToUpper(strings.TrimSpace(s.Author.ParticipantType))
		if pt != "CONTESTANT" && pt != "OUT_OF_COMPETITION" {
			continue
		}
		idx := strings.TrimSpace(s.Problem.Index)
		if idx == "" {
			continue
		}
		cid := canonicalCFContestID(s.ContestID)
		m := byContest[cid]
		if m == nil {
			m = map[string]*agg{}
			byContest[cid] = m
		}
		a := m[idx]
		if a == nil {
			a = &agg{label: idx}
			m[idx] = a
		}
		if a.ac {
			continue // AC 后提交不计
		}
		verdict := strings.ToUpper(strings.TrimSpace(s.Verdict))
		if verdict == "OK" {
			a.ac = true
			a.firstAC = s.CreationTimeSeconds
			// attempts = AC 前错误次数（不含本次 AC）
			continue
		}
		// 忽略排队中
		if verdict == "" || verdict == "TESTING" || verdict == "IN_QUEUE" {
			continue
		}
		// COMPILATION_ERROR 不计罚（ICPC 惯例）
		if verdict == "COMPILATION_ERROR" {
			continue
		}
		a.attempts++
	}

	out := make([]spider.ContestProblemCell, 0, 64)
	for cid, m := range byContest {
		cidStr := strconv.Itoa(cid)
		if cid < 0 {
			cidStr = strconv.Itoa(-cid)
		}
		start := startBy[cfPositiveContestID(cid)]
		for idx, a := range m {
			if a.attempts == 0 && !a.ac {
				continue
			}
			ext := cidStr + idx
			if cid < 0 {
				ext = "gym" + ext
			}
			cell := spider.ContestProblemCell{
				ContestID:  cidStr,
				Label:      a.label,
				ExternalID: ext,
				Attempts:   a.attempts,
			}
			if a.ac {
				cell.Status = model.ContestCellAC
				t := time.Unix(a.firstAC, 0)
				cell.FirstACAt = &t
				if start > 0 && a.firstAC >= start {
					rel := int(a.firstAC - start)
					cell.RelativeSec = &rel
				}
			} else {
				cell.Status = model.ContestCellTried
			}
			_ = userId // 写入层填 UserID
			out = append(out, cell)
		}
	}
	return out, nil
}

func fetchCFContestListMap() (map[int]cfContestListEntry, error) {
	body, _, err := cfGetJSON(context.Background(), "/api/contest.list?gym=false")
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string               `json:"status"`
		Result []cfContestListEntry `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if out.Status != "OK" {
		return nil, fmt.Errorf("codeforces contest.list status %s", out.Status)
	}
	m := make(map[int]cfContestListEntry, len(out.Result))
	for _, c := range out.Result {
		m[c.ID] = c
	}
	return m, nil
}

func (p NewCodeforces) Name() string {
	return spider.CodeForces
}

// FetchRating 通过 Codeforces API user.info 取当前 rating
func (p NewCodeforces) FetchRating(username string) (int, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, false, fmt.Errorf("codeforces handle 为空")
	}
	path := fmt.Sprintf("/api/user.info?handles=%s", url.QueryEscape(username))
	body, _, err := cfGetJSON(context.Background(), path)
	if err != nil {
		return 0, false, fmt.Errorf("codeforces rating 请求失败: %w", err)
	}
	var out struct {
		Status string `json:"status"`
		Result []struct {
			// 未参赛用户无 rating 字段
			Rating *int `json:"rating"`
		} `json:"result"`
		Comment string `json:"comment"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, false, fmt.Errorf("codeforces rating 解析失败: %w", err)
	}
	if out.Status != "OK" || len(out.Result) == 0 {
		return 0, false, fmt.Errorf("codeforces rating API: %s %s", out.Status, out.Comment)
	}
	if out.Result[0].Rating == nil {
		return 0, false, nil // 未参赛
	}
	return *out.Result[0].Rating, true, nil
}

// cfHTTPStatusErr 不把 HTML 墙/整页 body 塞进 error（会污染 lastError 与日志）。
func cfHTTPStatusErr(api string, status int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	low := strings.ToLower(snippet)
	if strings.Contains(low, "<html") || strings.Contains(low, "<!doctype") || strings.Contains(low, "nginx") {
		switch status {
		case http.StatusForbidden:
			return fmt.Errorf("请求响应码错误 %d (codeforces %s 被拒绝/拦截)", status, api)
		case http.StatusTooManyRequests:
			return fmt.Errorf("请求响应码错误 %d (codeforces %s 限流)", status, api)
		default:
			return fmt.Errorf("请求响应码错误 %d (codeforces %s 返回异常页面)", status, api)
		}
	}
	if utf8.RuneCountInString(snippet) > 120 {
		snippet = string([]rune(snippet)[:120]) + "…"
	}
	if snippet == "" {
		return fmt.Errorf("请求响应码错误 %d (codeforces %s)", status, api)
	}
	return fmt.Errorf("请求响应码错误 %d, %s", status, snippet)
}

func init() {
	// 注册到注册中心
	spider.Register(NewCodeforces{})
}
