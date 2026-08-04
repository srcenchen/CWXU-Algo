package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
)

// POJ（北大 OJ，poj.org）公开 status 列表爬虫，无需登录。
//
// 列表：http://poj.org/status?user_id={handle}
// 翻页：Next Page 使用 top={本页最小 Run ID} 取更旧记录。
// needAll=false：仅最新一页（约 20 条）；needAll=true：沿 top 翻页直至空页。
const (
	pojStatusBase  = "http://poj.org/status"
	pojPageDelay   = 400 * time.Millisecond
	pojMaxPagesAll = 500 // 20 条/页 → 最多约 1 万条，防无界
)

// 时区：POJ 展示时间为北京时间
var pojLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// 行：Run ID | User | Problem | Result | Memory | Time | Language | Code Length | Submit Time
var (
	rePOJRow = regexp.MustCompile(`(?is)<tr\s+align=center>(.*?)</tr>`)
	rePOJTD  = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	// 题目列可能是 <a href=problem?id=1000>1000</a>
	rePOJProblemID = regexp.MustCompile(`(?i)problem\?id=(\d+)`)
	rePOJTags      = regexp.MustCompile(`(?s)<[^>]*>`)
)

// NewPOJ Peking University Online Judge 提交爬虫。
type NewPOJ struct{}

func (p NewPOJ) Name() string { return spider.POJ }

func mapPOJStatus(raw string) string {
	s := strings.TrimSpace(raw)
	// 去掉可能残留的空白/大小写差异
	switch strings.ToLower(s) {
	case "accepted":
		return "AC"
	case "presentation error":
		return "PE"
	case "time limit exceeded":
		return "TLE"
	case "memory limit exceeded":
		return "MLE"
	case "wrong answer":
		return "WA"
	case "runtime error":
		return "RE"
	case "output limit exceeded":
		return "OLE"
	case "compile error", "compilation error":
		return "CE"
	case "waiting", "running", "compiling", "queuing":
		return "Judging"
	default:
		if s == "" {
			return "WA"
		}
		return s
	}
}

func stripPOJHTML(s string) string {
	s = rePOJTags.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func parsePOJTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, pojLoc); err == nil {
		return t
	}
	return time.Time{}
}

// parsePOJStatusPage 解析一页 status HTML，返回本页记录与最小 Run ID（用于翻页 top=）。
func parsePOJStatusPage(html string, userId int64) (logs []model.SubmitLog, minRunID string, ok bool) {
	rows := rePOJRow.FindAllStringSubmatch(html, -1)
	if len(rows) == 0 {
		return nil, "", false
	}
	var minID int64 = -1
	for _, row := range rows {
		cells := rePOJTD.FindAllStringSubmatch(row[1], -1)
		if len(cells) < 9 {
			continue
		}
		runID := stripPOJHTML(cells[0][1])
		if runID == "" || !isAllDigits(runID) {
			continue
		}
		// Problem
		ext := ""
		if m := rePOJProblemID.FindStringSubmatch(cells[2][1]); len(m) > 1 {
			ext = m[1]
		} else {
			ext = stripPOJHTML(cells[2][1])
		}
		problem := ext
		if ext != "" {
			problem = "#" + ext
		}
		status := mapPOJStatus(stripPOJHTML(cells[3][1]))
		lang := stripPOJHTML(cells[6][1])
		t := parsePOJTime(stripPOJHTML(cells[8][1]))

		logs = append(logs, model.SubmitLog{
			UserID:     userId,
			Platform:   spider.POJ,
			SubmitID:   runID,
			Problem:    problem,
			ExternalID: ext,
			Lang:       lang,
			Status:     status,
			Time:       t,
		})
		if id, err := parseInt64(runID); err == nil {
			if minID < 0 || id < minID {
				minID = id
			}
		}
	}
	if len(logs) == 0 {
		return nil, "", false
	}
	if minID >= 0 {
		minRunID = fmt.Sprintf("%d", minID)
	}
	return logs, minRunID, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not digits")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

func pojFetchPage(ctx context.Context, username, top string) (string, error) {
	q := url.Values{}
	q.Set("user_id", username)
	if top != "" {
		q.Set("top", top)
	}
	reqURL := pojStatusBase + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ojhttp.DefaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8")
	req.Header.Set("Referer", "http://poj.org/")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return "", fmt.Errorf("poj status: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("poj status read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("poj status status %d", resp.StatusCode)
	}
	return string(rb), nil
}

// FetchSubmitLog 拉取 POJ 公开提交。
// needAll=false：最新一页；needAll=true：top 向旧翻页直至空页或硬顶。
func (p NewPOJ) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("poj username 为空")
	}

	var (
		res    []model.SubmitLog
		top    string
		pages  int
		seenID = map[string]struct{}{}
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pages++
		if pages > pojMaxPagesAll {
			break
		}
		html, err := pojFetchPage(ctx, username, top)
		if err != nil {
			return nil, err
		}
		pageLogs, minRun, ok := parsePOJStatusPage(html, userId)
		if !ok {
			break
		}
		added := 0
		for _, l := range pageLogs {
			if _, dup := seenID[l.SubmitID]; dup {
				continue
			}
			seenID[l.SubmitID] = struct{}{}
			res = append(res, l)
			added++
		}
		if !needAll {
			break
		}
		// 无新记录或无法翻页
		if added == 0 || minRun == "" || minRun == top {
			break
		}
		top = minRun
		time.Sleep(pojPageDelay)
	}
	return res, nil
}

func init() {
	spider.Register(&NewPOJ{})
}
