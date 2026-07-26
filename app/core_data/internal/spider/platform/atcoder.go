package platform

import (
	"context"
	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type NewAtCoder struct{}
type atcJson struct {
	ID            int    `json:"id"`
	EpochSecond   int64  `json:"epoch_second"` // Unix 时间戳（秒）
	ProblemID     string `json:"problem_id"`
	ContestID     string `json:"contest_id"`
	UserID        string `json:"user_id"`
	Language      string `json:"language"`
	Result        string `json:"result"`         // 如 "AC", "WA" 等
	ExecutionTime int    `json:"execution_time"` // 执行时间（毫秒）
}

func fetchLog(ctx context.Context, url string) ([]atcJson, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 发起 Get 请求（ctx 透传，调用方超时可中断）
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发起http请求失败: %s", err.Error())
	}
	defer resp.Body.Close()
	// 校验状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("请求响应码错误 %d, %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析body错误: %s", err.Error())
	}
	var atc []atcJson
	if err := json.Unmarshal(body, &atc); err != nil {
		return nil, fmt.Errorf("解析json错误：%s", err.Error())
	}
	return atc, nil
}

func (p NewAtCoder) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) (res []model.SubmitLog, err error) {
	// needAll=true：from_second=0 全量；false：最近 60 小时
	t := time.Unix(0, 0)
	if needAll == false {
		t = time.Now().Add(-60 * (1 * time.Hour))
	}
	url := fmt.Sprintf(
		"https://atc.luckysan.top/atcoder/atcoder-api/v3/user/submissions?user=%s&from_second=%d",
		username, int(t.Unix()),
	)
	atc, err := fetchLog(ctx, url)
	if err != nil {
		return nil, err
	}
	// 构建res（external_id = problem_id，供站内榜 cell-submits / 题库绑定）
	for _, v := range atc {
		pid := strings.TrimSpace(v.ProblemID)
		tmp := model.SubmitLog{
			UserID:     userId,
			Platform:   "AtCoder",
			SubmitID:   strconv.Itoa(v.ID),
			Contest:    v.ContestID,
			Problem:    pid,
			ExternalID: pid,
			Lang:       v.Language,
			Status:     v.Result,
			Time:       time.Unix(v.EpochSecond, 0),
		}
		res = append(res, tmp)
	}
	// 分页：每页最多 500；用 last epoch 推进，并防重复页空转
	const maxPages = 200
	seenLast := map[int64]struct{}{}
	for page := 0; len(atc) == 500 && page < maxPages; page++ {
		lastEpoch := atc[len(atc)-1].EpochSecond
		if _, dup := seenLast[lastEpoch]; dup {
			break
		}
		seenLast[lastEpoch] = struct{}{}
		url := fmt.Sprintf(
			"https://atc.luckysan.top/atcoder/atcoder-api/v3/user/submissions?user=%s&from_second=%d",
			username, lastEpoch,
		)
		atc, err = fetchLog(ctx, url)
		if err != nil {
			return nil, err
		}
		// 跳过与上一页末条同 epoch 的首条重复（API 常 inclusive）
		start := 0
		if len(atc) > 0 && atc[0].EpochSecond == lastEpoch && len(res) > 0 {
			start = 1
		}
		for _, v := range atc[start:] {
			pid := strings.TrimSpace(v.ProblemID)
			tmp := model.SubmitLog{
				UserID:     userId,
				Platform:   "AtCoder",
				SubmitID:   strconv.Itoa(v.ID),
				Contest:    v.ContestID,
				Problem:    pid,
				ExternalID: pid,
				Lang:       v.Language,
				Status:     v.Result,
				Time:       time.Unix(v.EpochSecond, 0),
			}
			res = append(res, tmp)
		}
	}
	return res, nil
}
func (p NewAtCoder) Name() string {
	return spider.AtCoder
}

// atcoderHistoryEntry AtCoder 官方 /users/{handle}/history/json 单条
type atcoderHistoryEntry struct {
	IsRated           bool   `json:"IsRated"`
	Place             int    `json:"Place"`
	OldRating         int    `json:"OldRating"`
	NewRating         int    `json:"NewRating"`
	ContestScreenName string `json:"ContestScreenName"`
	ContestName       string `json:"ContestName"`
	ContestNameEn     string `json:"ContestNameEn"`
	EndTime           string `json:"EndTime"`
}

const atcoderContestScreenSuffix = ".contest.atcoder.jp"

// normalizeAtCoderContestID 将 ContestScreenName 规范为稳定 contest id
// 旧式如 agc004.contest.atcoder.jp → agc004；新式短名原样保留。
func normalizeAtCoderContestID(screenName string) string {
	screenName = strings.TrimSpace(screenName)
	if strings.HasSuffix(screenName, atcoderContestScreenSuffix) {
		return strings.TrimSuffix(screenName, atcoderContestScreenSuffix)
	}
	return screenName
}

func parseAtCoderHistoryJSON(body []byte) ([]atcoderHistoryEntry, error) {
	var hist []atcoderHistoryEntry
	if err := json.Unmarshal(body, &hist); err != nil {
		return nil, fmt.Errorf("atcoder history 解析失败: %w", err)
	}
	return hist, nil
}

func fetchAtCoderHistory(username string) ([]atcoderHistoryEntry, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("atcoder username 为空")
	}
	url := fmt.Sprintf("https://atcoder.jp/users/%s/history/json", username)
	resp, err := ojhttp.Get(url)
	if err != nil {
		return nil, fmt.Errorf("atcoder history 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("atcoder history 状态码 %d: %s", resp.StatusCode, string(body))
	}
	return parseAtCoderHistoryJSON(body)
}

// contestLogsFromAtCoderHistory 将 history JSON 映射为 ContestLog。
// needAll=false 时只保留时间最新的一场（API 多为时间升序，取末条）。
// acByContest 为各 contest_id 的正式参赛过题数（可 nil）。
func contestLogsFromAtCoderHistory(userId int64, hist []atcoderHistoryEntry, needAll bool, acByContest map[string]int) []model.ContestLog {
	if len(hist) == 0 {
		return nil
	}
	entries := hist
	if !needAll {
		entries = hist[len(hist)-1:]
	}
	out := make([]model.ContestLog, 0, len(entries))
	for _, h := range entries {
		id := normalizeAtCoderContestID(h.ContestScreenName)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(h.ContestName)
		if name == "" {
			name = strings.TrimSpace(h.ContestNameEn)
		}
		var t time.Time
		if h.EndTime != "" {
			if parsed, err := time.Parse(time.RFC3339, h.EndTime); err == nil {
				t = parsed
			}
		}
		ac := 0
		if acByContest != nil {
			ac = acByContest[id]
		}
		out = append(out, model.ContestLog{
			Platform:    spider.AtCoder,
			UserID:      userId,
			ContestId:   id,
			ContestName: name,
			ContestUrl:  "https://atcoder.jp/contests/" + id,
			Rank:        h.Place,
			// history/json 无过题数；AC 由提交 API 统计（见 fetchAtCoderContestAC）
			TotalCount: 0,
			AcCount:    ac,
			Time:       t,
		})
	}
	return out
}

// atcoderHistoryEndUnix 从 history 解析各 contest 结束时间（unix 秒），用于过滤赛后练习提交。
func atcoderHistoryEndUnix(hist []atcoderHistoryEntry) map[string]int64 {
	out := make(map[string]int64, len(hist))
	for _, h := range hist {
		id := normalizeAtCoderContestID(h.ContestScreenName)
		if id == "" || h.EndTime == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, h.EndTime)
		if err != nil {
			continue
		}
		out[id] = parsed.Unix()
	}
	return out
}

// fetchAtCoderSubmissions 拉取用户提交（kenkoooo 兼容 API，经代理）；needAll=false 时仅最近 60h。
func fetchAtCoderSubmissions(username string, needAll bool) ([]atcJson, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("atcoder username 为空")
	}
	from := int64(0)
	if !needAll {
		from = time.Now().Add(-60 * time.Hour).Unix()
	}
	var all []atcJson
	url := fmt.Sprintf(
		"https://atc.luckysan.top/atcoder/atcoder-api/v3/user/submissions?user=%s&from_second=%d",
		username, from,
	)
	for {
		page, err := fetchLog(context.Background(), url)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			break
		}
		// 下一页从末条 epoch 继续（API 按时间升序，同秒可能重复，调用方按 problem 去重）
		nextFrom := page[len(page)-1].EpochSecond
		url = fmt.Sprintf(
			"https://atc.luckysan.top/atcoder/atcoder-api/v3/user/submissions?user=%s&from_second=%d",
			username, nextFrom,
		)
	}
	return all, nil
}

// fetchAtCoderContestAC 按 contest_id 统计正式参赛 unique AC。
// endByContest 有结束时间时只计 epoch_second <= 结束时间的提交，排除赛后练习/虚拟。
func fetchAtCoderContestAC(subs []atcJson, endByContest map[string]int64) map[string]int {
	acProblems := map[string]map[string]struct{}{}
	for _, s := range subs {
		cid := strings.TrimSpace(s.ContestID)
		pid := strings.TrimSpace(s.ProblemID)
		if cid == "" || pid == "" {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(s.Result), "AC") {
			continue
		}
		if end, ok := endByContest[cid]; ok && end > 0 && s.EpochSecond > end {
			continue
		}
		set := acProblems[cid]
		if set == nil {
			set = map[string]struct{}{}
			acProblems[cid] = set
		}
		set[pid] = struct{}{}
	}
	out := make(map[string]int, len(acProblems))
	for cid, set := range acProblems {
		out[cid] = len(set)
	}
	return out
}

// FetchContestLog 从 AtCoder 官方参赛历史拉取比赛记录，并用提交 API 补过题数（SubmitContestFetcher）。
//
//  1. /users/{handle}/history/json → 排名 / 比赛名 / 结束时间
//  2. kenkoooo user/submissions → 按 contest_id 统计赛时 unique AC
func (p NewAtCoder) FetchContestLog(userId int64, username string, needAll bool) ([]model.ContestLog, error) {
	hist, err := fetchAtCoderHistory(username)
	if err != nil {
		return nil, err
	}
	var acByContest map[string]int
	// 提交拉取失败不阻断榜单（仍有 rank）；AC 保持 0，下次全量可 GREATEST 补上
	if subs, sErr := fetchAtCoderSubmissions(username, needAll); sErr == nil {
		acByContest = fetchAtCoderContestAC(subs, atcoderHistoryEndUnix(hist))
	}
	return contestLogsFromAtCoderHistory(userId, hist, needAll, acByContest), nil
}

// FetchContestDetails 赛时提交 → 每题 AC/TRIED（EndTime 后练习不计）。
func (p NewAtCoder) FetchContestDetails(userId int64, username string, needAll bool) ([]spider.ContestProblemCell, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("atcoder username 为空")
	}
	hist, err := fetchAtCoderHistory(username)
	if err != nil {
		return nil, err
	}
	endBy := atcoderHistoryEndUnix(hist)
	subs, err := fetchAtCoderSubmissions(username, needAll)
	if err != nil {
		return nil, err
	}
	_ = userId
	return aggregateAtCoderContestDetailCells(subs, endBy), nil
}

// aggregateAtCoderContestDetailCells 仅对 history 有 EndTime 的正式场次聚合赛时格子。
// 未参赛（无 EndTime）或 EndTime 之后的提交一律跳过，避免赛后补题被写成 AC。
func aggregateAtCoderContestDetailCells(subs []atcJson, endBy map[string]int64) []spider.ContestProblemCell {
	type agg struct {
		attempts int
		ac       bool
		firstAC  int64
		label    string
	}
	by := map[string]map[string]*agg{} // contest -> problem_id -> agg

	sort.SliceStable(subs, func(i, j int) bool {
		return subs[i].EpochSecond < subs[j].EpochSecond
	})
	for _, s := range subs {
		cid := strings.TrimSpace(s.ContestID)
		pid := strings.TrimSpace(s.ProblemID)
		if cid == "" || pid == "" {
			continue
		}
		// 仅正式参赛场次写赛时格子：history 无该场 EndTime → 未参赛/仅补题，
		// 禁止把赛后 AC 写成 ContestCellAC（否则榜上绿钩、罚时被污染）。
		// 补题由 InferContestUpsolves / ListContestPracticeCells 标 UPSOLVE。
		end, hasEnd := endBy[cid]
		if !hasEnd || end <= 0 {
			continue
		}
		if s.EpochSecond > end {
			continue
		}
		m := by[cid]
		if m == nil {
			m = map[string]*agg{}
			by[cid] = m
		}
		a := m[pid]
		if a == nil {
			label := pid
			if i := strings.LastIndex(pid, "_"); i >= 0 && i+1 < len(pid) {
				label = strings.ToUpper(pid[i+1:])
			}
			a = &agg{label: label}
			m[pid] = a
		}
		if a.ac {
			continue
		}
		res := strings.ToUpper(strings.TrimSpace(s.Result))
		if res == "AC" {
			a.ac = true
			a.firstAC = s.EpochSecond
			continue
		}
		if res == "" || res == "WJ" || res == "WR" {
			continue
		}
		if res == "CE" {
			continue
		}
		a.attempts++
	}

	out := make([]spider.ContestProblemCell, 0, 64)
	for cid, m := range by {
		for pid, a := range m {
			if a.attempts == 0 && !a.ac {
				continue
			}
			cell := spider.ContestProblemCell{
				ContestID:  cid,
				Label:      a.label,
				ExternalID: pid,
				Attempts:   a.attempts,
			}
			if a.ac {
				cell.Status = model.ContestCellAC
				t := time.Unix(a.firstAC, 0)
				cell.FirstACAt = &t
				if end, ok := endBy[cid]; ok && end > 0 {
					// history 仅有 EndTime：用平台默认 100min 反推开赛，写 relative_sec 供榜罚时
					startApprox := end - int64((100 * time.Minute).Seconds())
					if a.firstAC >= startApprox {
						rel := int(a.firstAC - startApprox)
						cell.RelativeSec = &rel
					}
				}
			} else {
				cell.Status = model.ContestCellTried
			}
			out = append(out, cell)
		}
	}
	return out
}

// FetchRating 从 AtCoder 官方 rating 历史取最新 NewRating
func (p NewAtCoder) FetchRating(username string) (int, bool, error) {
	hist, err := fetchAtCoderHistory(username)
	if err != nil {
		return 0, false, err
	}
	if len(hist) == 0 {
		return 0, false, nil
	}
	// 优先最后一场 rated 的 NewRating；若全是非 rated，取最后一条 NewRating
	lastRated := -1
	for i, h := range hist {
		if h.IsRated {
			lastRated = i
		}
	}
	if lastRated >= 0 {
		return hist[lastRated].NewRating, true, nil
	}
	// 有历史但无 rated：仍返回末条（通常等于当前展示 rating）
	return hist[len(hist)-1].NewRating, true, nil
}

func init() {
	// 注册到注册中心
	spider.Register(NewAtCoder{})
}
