package platform

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 牛客比赛页内嵌 JSON：startTime/endTime 多为毫秒时间戳
var (
	reNowCoderStartTime = regexp.MustCompile(`"startTime"\s*:\s*(\d{10,16})`)
	reNowCoderEndTime   = regexp.MustCompile(`"endTime"\s*:\s*(\d{10,16})`)
	reNowCoderPageName  = regexp.MustCompile(`"name"\s*:\s*"([^"\\]{1,200})"`)
	reNowCoderTitle     = regexp.MustCompile(`(?i)<title>([^<]+)</title>`)
)

// ParseNowCoderContestTimesHTML 从比赛页 HTML 解析官方起止时间（Unix 秒）与名称。
// 正确示例：河南萌新联赛 13:00–17:00（4h），而非默认 3h 推断。
func ParseNowCoderContestTimesHTML(html string) (startSec, endSec int64, name string, err error) {
	html = strings.TrimSpace(html)
	if html == "" {
		return 0, 0, "", fmt.Errorf("empty html")
	}
	sm := reNowCoderStartTime.FindStringSubmatch(html)
	em := reNowCoderEndTime.FindStringSubmatch(html)
	if len(sm) < 2 || len(em) < 2 {
		return 0, 0, "", fmt.Errorf("startTime/endTime not found")
	}
	startRaw, _ := strconv.ParseInt(sm[1], 10, 64)
	endRaw, _ := strconv.ParseInt(em[1], 10, 64)
	startSec = nowcoderMsToUnixSec(startRaw)
	endSec = nowcoderMsToUnixSec(endRaw)
	// 爬多少用多少：仅要求 end>start，不做赛长上限否决
	if startSec <= 0 || endSec <= startSec {
		return 0, 0, "", fmt.Errorf("invalid window start=%d end=%d", startSec, endSec)
	}
	name = parseNowCoderContestName(html)
	return startSec, endSec, name, nil
}

func parseNowCoderContestName(html string) string {
	if m := reNowCoderPageName.FindStringSubmatch(html); len(m) >= 2 {
		n := strings.TrimSpace(m[1])
		if n != "" {
			return n
		}
	}
	if m := reNowCoderTitle.FindStringSubmatch(html); len(m) >= 2 {
		t := strings.TrimSpace(m[1])
		// 「xxx_ACM/NOI..._牛客竞赛OJ」→ 取赛名段
		if i := strings.Index(t, "_ACM"); i > 0 {
			t = t[:i]
		}
		if i := strings.Index(t, "_牛客"); i > 0 {
			t = t[:i]
		}
		return strings.TrimSpace(t)
	}
	return ""
}

// nowcoderMsToUnixSec 毫秒→秒；已是秒级则原样返回。
func nowcoderMsToUnixSec(v int64) int64 {
	if v <= 0 {
		return 0
	}
	// 1e12 ≈ 2001-09 毫秒；大于此视为毫秒
	if v >= 1_000_000_000_000 {
		return v / 1000
	}
	return v
}

// FetchNowCoderContestTimes 拉取比赛页并解析官方 start/end（Unix 秒）。
func FetchNowCoderContestTimes(contestID string) (startSec, endSec int64, name string, err error) {
	contestID = strings.TrimSpace(contestID)
	if contestID == "" {
		return 0, 0, "", fmt.Errorf("empty contest id")
	}
	rawURL := nowcoderContestPageURLFmt + contestID
	resp, err := nowcoderGet(context.Background(), rawURL, true)
	if err != nil {
		return 0, 0, "", err
	}
	body, err := nowcoderReadOK(resp, 2<<20)
	if err != nil {
		return 0, 0, "", fmt.Errorf("nowcoder contest %s: %w", contestID, err)
	}
	return ParseNowCoderContestTimesHTML(string(body))
}

// NowCoderContestWindow 官方时间窗（展示/日历用，无赛后缓冲）。
type NowCoderContestWindow struct {
	Start time.Time
	End   time.Time
	Name  string
}

// FetchNowCoderContestWindow 同 FetchNowCoderContestTimes，返回 time.Time。
func FetchNowCoderContestWindow(contestID string) (NowCoderContestWindow, error) {
	s, e, name, err := FetchNowCoderContestTimes(contestID)
	if err != nil {
		return NowCoderContestWindow{}, err
	}
	return NowCoderContestWindow{
		Start: time.Unix(s, 0),
		End:   time.Unix(e, 0),
		Name:  name,
	}, nil
}
