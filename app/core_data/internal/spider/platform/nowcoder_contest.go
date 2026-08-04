package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
)

// 牛客参赛历史（按用户主页 uid，非全站赛程）：
//
//	GET {nowcoderContestJoinedHistoryURL}?uid={uid}&page=N&onlyJoinedFilter=true&...
//	个人主页：https://ac.nowcoder.com/acm/contest/profile/{uid}
//
// 每场自带真实 startTime / endTime / contestDuration（毫秒），赛长不固定，禁止默认 3h。

const (
	nowcoderContestJoinedHistoryURL = "https://ac.nowcoder.com/acm-heavy/acm/contest/profile/contest-joined-history"
	nowcoderContestPageURLFmt       = "https://ac.nowcoder.com/acm/contest/"
	nowcoderContestHistoryPageSleep = 200 * time.Millisecond
)

var reNowCoderProfileUID = regexp.MustCompile(`(?i)/acm/contest/profile/(\d+)`)

// ContestHistoryItem 参赛历史单场（字段与线上 JSON 对齐）。
type ContestHistoryItem struct {
	ContestId       json.Number `json:"contestId"`
	ContestName     string      `json:"contestName"`
	Rank            int         `json:"rank"`
	TotalCount      int         `json:"problemCount"`
	AcCount         int         `json:"acceptedCount"`
	Rating          json.Number `json:"rating"`
	ChangeValue     json.Number `json:"changeValue"`
	StartTime       json.Number `json:"startTime"`       // 毫秒
	EndTime         json.Number `json:"endTime"`         // 毫秒
	ContestDuration json.Number `json:"contestDuration"` // 毫秒 = end-start，官方赛长
	ColorLevel      json.Number `json:"colorLevel"`
}

// ContestHistoryPageInfo 分页。
type ContestHistoryPageInfo struct {
	PageCount    int `json:"pageCount"`
	PageSize     int `json:"pageSize"`
	ElementCount int `json:"elementCount"`
	TotalCount   int `json:"totalCount"`
	PageCurrent  int `json:"pageCurrent"`
}

// ContestHistoryData 响应 data。
type ContestHistoryData struct {
	DataList  []ContestHistoryItem   `json:"dataList"`
	PageInfo  ContestHistoryPageInfo `json:"pageInfo"`
	BasicInfo map[string]interface{} `json:"basicInfo"`
}

// ResponseContest 参赛历史外层。
type ResponseContest struct {
	Msg  string             `json:"msg"`
	Code int                `json:"code"`
	Data ContestHistoryData `json:"data"`
}

// ParseNowCoderProfileUID 从主页 URL 或纯数字 uid 提取牛客竞赛 uid。
// 例：https://ac.nowcoder.com/acm/contest/profile/156260548 → "156260548"
func ParseNowCoderProfileUID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if isDigits(raw) {
		return raw
	}
	if m := reNowCoderProfileUID.FindStringSubmatch(raw); len(m) >= 2 {
		return m[1]
	}
	// 兼容 query ?uid=
	if u, err := url.Parse(raw); err == nil {
		if id := strings.TrimSpace(u.Query().Get("uid")); isDigits(id) {
			return id
		}
		// path 末段数字
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if n := len(parts); n > 0 && isDigits(parts[n-1]) {
			return parts[n-1]
		}
	}
	return ""
}

// ParseNowCoderContestHistoryJSON 解析参赛历史 API 响应。
func ParseNowCoderContestHistoryJSON(body []byte) (*ResponseContest, error) {
	var contestResp ResponseContest
	if err := json.Unmarshal(body, &contestResp); err != nil {
		return nil, fmt.Errorf("nowcoder contest history json: %w", err)
	}
	if contestResp.Code != 0 {
		return nil, fmt.Errorf("nowcoder contest history api: code=%d msg=%s", contestResp.Code, contestResp.Msg)
	}
	return &contestResp, nil
}

// nowcoderContestWindowFromItem 从单场历史解析官方 [start,end] Unix 秒。
// 优先 endTime；缺失时用 startTime+contestDuration（均为毫秒）。
func nowcoderContestWindowFromItem(item ContestHistoryItem) (startSec, endSec int64, ok bool) {
	startRaw, _ := item.StartTime.Int64()
	endRaw, _ := item.EndTime.Int64()
	durRaw, _ := item.ContestDuration.Int64()

	startSec = nowcoderMsToUnixSec(startRaw)
	endSec = nowcoderMsToUnixSec(endRaw)
	if startSec <= 0 {
		return 0, 0, false
	}
	if endSec > startSec {
		return startSec, endSec, true
	}
	// end 缺失/非法：用官方 contestDuration（毫秒）
	if durRaw > 0 {
		durSec := durRaw
		if durRaw >= 1_000_000 { // 毫秒
			durSec = durRaw / 1000
		}
		if durSec >= 15*60 && durSec <= 7*24*3600 {
			return startSec, startSec + durSec, true
		}
	}
	return startSec, 0, false
}

// ContestHistoryItemToLog 单场 → ContestLog（EndTime 为 gorm:"-" 内存字段，供写日历）。
func ContestHistoryItemToLog(userId int64, item ContestHistoryItem) (model.ContestLog, bool) {
	contestId, _ := item.ContestId.Int64()
	if contestId <= 0 {
		return model.ContestLog{}, false
	}
	startSec, endSec, okWin := nowcoderContestWindowFromItem(item)
	if startSec <= 0 {
		return model.ContestLog{}, false
	}
	var endAt time.Time
	if okWin && endSec > startSec {
		endAt = time.Unix(endSec, 0)
	}
	cid := strconv.FormatInt(contestId, 10)
	return model.ContestLog{
		Platform:    spider.NowCoder,
		UserID:      userId,
		Rank:        item.Rank,
		TotalCount:  item.TotalCount,
		AcCount:     item.AcCount,
		ContestId:   cid,
		ContestName: strings.TrimSpace(item.ContestName),
		ContestUrl:  nowcoderContestPageURLFmt + cid,
		Time:        time.Unix(startSec, 0),
		EndTime:     endAt,
	}, true
}

// ParseNowCoderContestLogsFromHistory 将一整页/整包 history JSON 转为 ContestLog 列表。
func ParseNowCoderContestLogsFromHistory(userId int64, body []byte) ([]model.ContestLog, *ContestHistoryPageInfo, error) {
	resp, err := ParseNowCoderContestHistoryJSON(body)
	if err != nil {
		return nil, nil, err
	}
	out := make([]model.ContestLog, 0, len(resp.Data.DataList))
	for _, item := range resp.Data.DataList {
		if log, ok := ContestHistoryItemToLog(userId, item); ok {
			out = append(out, log)
		}
	}
	pi := resp.Data.PageInfo
	return out, &pi, nil
}

// FetchNowCoderContestJoinedHistoryPage 拉取参赛历史一页原始 JSON。
func FetchNowCoderContestJoinedHistoryPage(uid string, page int) ([]byte, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" || !isDigits(uid) {
		return nil, fmt.Errorf("nowcoder uid 无效: %q", uid)
	}
	if page < 1 {
		page = 1
	}
	u := fmt.Sprintf("%s?token=&uid=%s&page=%d&onlyJoinedFilter=true&searchContestName=&onlyRatingFilter=false&contestEndFilter=true",
		nowcoderContestJoinedHistoryURL, url.QueryEscape(uid), page)
	resp, err := nowcoderGet(context.Background(), u, false)
	if err != nil {
		return nil, err
	}
	return nowcoderReadOK(resp, 4<<20)
}

// FetchContestLog 从参赛历史 API 拉取比赛记录；每场带真实 EndTime（非固定 3h）。
// username 为牛客竞赛 uid（数字），也可传 profile URL。
func (nc NewNowCoder) FetchContestLog(userId int64, username string, needAll bool) ([]model.ContestLog, error) {
	uid := ParseNowCoderProfileUID(username)
	if uid == "" {
		return nil, fmt.Errorf("nowcoder uid 为空或无法从 %q 解析", username)
	}

	result := make([]model.ContestLog, 0)
	page := 1
	for {
		body, err := FetchNowCoderContestJoinedHistoryPage(uid, page)
		if err != nil {
			return nil, err
		}
		logs, pageInfo, err := ParseNowCoderContestLogsFromHistory(userId, body)
		if err != nil {
			return nil, err
		}
		result = append(result, logs...)

		if !needAll || pageInfo == nil || page >= pageInfo.PageCount {
			break
		}
		page++
		time.Sleep(nowcoderContestHistoryPageSleep)
	}
	return result, nil
}
