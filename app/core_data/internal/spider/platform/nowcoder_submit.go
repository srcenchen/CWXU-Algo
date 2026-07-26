package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"github.com/PuerkitoBio/goquery"
)

// 提交双源：
//  1. AC 竞赛站 practice-coding HTML
//  2. 主站训练 submission-history JSON
//
// needAll=false：practice 第 1 页 + training 最多 150 条
// needAll=true：practice 最多 100 页 + training 最多 1 万条

const (
	nowcoderPracticeCodingURL = "https://ac.nowcoder.com/acm/contest/profile/%s/practice-coding?pageSize=100&page=%d"
	nowcoderTrainingHistoryURL = "https://gw-c.nowcoder.com/api/sparta/user/question-training/submission-history"

	nowcoderPracticePageSize   = 100
	nowcoderPracticeMaxPages   = 100 // needAll 硬顶 1 万条
	nowcoderTrainingPageSize   = 50
	nowcoderTrainingLimitIncr  = 150
	nowcoderTrainingLimitAll   = 10000
	nowcoderPageSleep          = 200 * time.Millisecond
	nowcoderSubmitTimeLayout   = "2006-01-02 15:04:05"
)

// practiceSubmission practice-coding 表格一行。
type practiceSubmission struct {
	RunID      string
	Problem    string // 展示："数字题号 标题"
	ProblemID  string // /acm/problem/{id}
	Result     string
	Score      string
	TimeMS     string
	MemoryKB   string
	CodeLen    string
	Language   string
	SubmitTime string
}

// trainingHistoryRecord 主站训练 submission-history 单条。
type trainingHistoryRecord struct {
	Problem struct {
		// id = AC 站数字题号；questionUuid = 主站 32 位 hex
		// questionNum 可能是 "309177" 或 "ACM413"（展示号不可当 external_id）
		ID           int64  `json:"id"`
		QuestionID   int64  `json:"questionId"`
		QuestionNum  string `json:"questionNum"`
		QuestionUUID string `json:"questionUuid"`
		Title        string `json:"title"`
	} `json:"problem"`
	Submission struct {
		ID          int64 `json:"id"`
		CreatedDate int64 `json:"createdDate"` // 毫秒
	} `json:"submission"`
	Language string `json:"language"`
	Status   struct {
		Desc string `json:"desc"`
	} `json:"status"`
}

type trainingHistoryResp struct {
	Success bool `json:"success"`
	Data    struct {
		TotalPage int                     `json:"totalPage"`
		Records   []trainingHistoryRecord `json:"records"`
	} `json:"data"`
}

func getSubLogResp(url string) (*goquery.Document, error) {
	return getSubLogRespCtx(context.Background(), url)
}

// getSubLogRespCtx 同 getSubLogResp，ctx 透传到 HTTP 请求（爬虫超时可中断）
func getSubLogRespCtx(ctx context.Context, url string) (*goquery.Document, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发起http请求失败: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("请求响应码错误 %d, %s", resp.StatusCode, string(body))
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析html失败")
	}
	return doc, nil
}

func parsePracticeCodingTable(doc *goquery.Document) []practiceSubmission {
	var subs []practiceSubmission
	doc.Find("table.table-hover tbody tr").Each(func(_ int, tr *goquery.Selection) {
		tds := tr.Find("td")
		if tds.Length() < 9 {
			return
		}
		probCell := tds.Eq(1)
		title := strings.TrimSpace(probCell.Text())
		problemID := extractPracticeProblemID(probCell)
		problem := title
		if problemID != "" && !strings.HasPrefix(title, problemID) {
			problem = problemID + " " + title
		}
		subs = append(subs, practiceSubmission{
			RunID:      strings.TrimSpace(tds.Eq(0).Text()),
			Problem:    problem,
			ProblemID:  problemID,
			Result:     strings.TrimSpace(tds.Eq(2).Text()),
			Score:      strings.TrimSpace(tds.Eq(3).Text()),
			TimeMS:     strings.TrimSpace(tds.Eq(4).Text()),
			MemoryKB:   strings.TrimSpace(tds.Eq(5).Text()),
			CodeLen:    strings.TrimSpace(tds.Eq(6).Text()),
			Language:   strings.TrimSpace(tds.Eq(7).Text()),
			SubmitTime: strings.TrimSpace(tds.Eq(8).Text()),
		})
	})
	return subs
}

// extractPracticeProblemID 从链接提取 /acm/problem/{数字id}。
func extractPracticeProblemID(probCell *goquery.Selection) string {
	var problemID string
	probCell.Find("a").Each(func(_ int, a *goquery.Selection) {
		if problemID != "" {
			return
		}
		href, _ := a.Attr("href")
		i := strings.LastIndex(href, "/problem/")
		if i < 0 {
			return
		}
		id := strings.Trim(href[i+len("/problem/"):], "/")
		if j := strings.IndexAny(id, "?#"); j >= 0 {
			id = id[:j]
		}
		if id != "" && isDigits(id) {
			problemID = id
		}
	})
	return problemID
}

func practiceCodingTotalSubmits(doc *goquery.Document) int {
	var totalSubmit string
	doc.Find(".my-state-item").Each(func(_ int, s *goquery.Selection) {
		label := strings.TrimSpace(s.Find("span").Text())
		if label == "次提交" {
			totalSubmit = strings.TrimSpace(s.Find(".state-num").Text())
		}
	})
	n, _ := strconv.Atoi(totalSubmit)
	return n
}

func practiceSubmissionsToLogs(userId int64, subs []practiceSubmission) []model.SubmitLog {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	out := make([]model.SubmitLog, 0, len(subs))
	for _, v := range subs {
		t, _ := time.ParseInLocation(nowcoderSubmitTimeLayout, v.SubmitTime, loc)
		out = append(out, model.SubmitLog{
			UserID:   userId,
			Platform: spider.NowCoder,
			SubmitID: v.RunID,
			Contest:  "",
			Problem:  v.Problem,
			Lang:     v.Language,
			Status:   v.Result,
			Time:     t,
		})
	}
	return out
}

// fetchPracticeCodingLogs 拉 AC 站 practice-coding HTML 提交。
func fetchPracticeCodingLogs(ctx context.Context, userId int64, uid string, needAll bool) ([]model.SubmitLog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	url := fmt.Sprintf(nowcoderPracticeCodingURL, uid, 1)
	doc, err := getSubLogRespCtx(ctx, url)
	if err != nil {
		return nil, err
	}
	subs := parsePracticeCodingTable(doc)
	if needAll {
		totalS := practiceCodingTotalSubmits(doc)
		totPage := (totalS + nowcoderPracticePageSize - 1) / nowcoderPracticePageSize
		if totPage > nowcoderPracticeMaxPages {
			totPage = nowcoderPracticeMaxPages
		}
		for i := 2; i <= totPage; i++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			time.Sleep(nowcoderPageSleep)
			pageURL := fmt.Sprintf(nowcoderPracticeCodingURL, uid, i)
			pageDoc, err := getSubLogRespCtx(ctx, pageURL)
			if err != nil {
				return nil, err
			}
			subs = append(subs, parsePracticeCodingTable(pageDoc)...)
		}
	}
	return practiceSubmissionsToLogs(userId, subs), nil
}

func fetchTrainingHistoryPage(ctx context.Context, uid string, page, pageSize int) (*trainingHistoryResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body := fmt.Sprintf(`{"pageNo":%d,"pageSize":%d,"userId":%s}`, page, pageSize, uid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nowcoderTrainingHistoryURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ojhttp.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r trainingHistoryResp
	if err := json.Unmarshal(bs, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func trainingRecordsToLogs(userId int64, records []trainingHistoryRecord) []model.SubmitLog {
	out := make([]model.SubmitLog, 0, len(records))
	for _, it := range records {
		// 优先 problem.id（AC 站数字题号），勿优先 UUID，否则与 practice-coding 双计
		problem := nowcoderProblemLabel(
			it.Problem.ID,
			it.Problem.QuestionUUID,
			it.Problem.QuestionNum,
			it.Problem.Title,
		)
		out = append(out, model.SubmitLog{
			UserID:   userId,
			Platform: spider.NowCoder,
			SubmitID: strconv.FormatInt(it.Submission.ID, 10),
			Contest:  "", // 练习题不写 contest，避免 parse 用 main|uid 污染 external_id
			Problem:  problem,
			Lang:     it.Language,
			Status:   it.Status.Desc,
			Time:     time.Unix(it.Submission.CreatedDate/1000, 0),
		})
	}
	return out
}

// fetchTrainingHistoryLogs 拉主站训练提交历史；失败返回空切片（与原逻辑一致）。
func fetchTrainingHistoryLogs(ctx context.Context, userId int64, uid string, needAll bool) []model.SubmitLog {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := nowcoderTrainingLimitIncr
	if needAll {
		limit = nowcoderTrainingLimitAll
	}
	result := make([]model.SubmitLog, 0)
	appendPage := func(records []trainingHistoryRecord) bool {
		for _, log := range trainingRecordsToLogs(userId, records) {
			result = append(result, log)
			if len(result) >= limit {
				return false
			}
		}
		return true
	}

	first, err := fetchTrainingHistoryPage(ctx, uid, 1, nowcoderTrainingPageSize)
	if err != nil {
		return result
	}
	if !appendPage(first.Data.Records) {
		return result
	}
	for page := 2; page <= first.Data.TotalPage; page++ {
		if ctx.Err() != nil {
			break
		}
		time.Sleep(nowcoderPageSleep)
		r, err := fetchTrainingHistoryPage(ctx, uid, page, nowcoderTrainingPageSize)
		if err != nil {
			break
		}
		if len(r.Data.Records) == 0 {
			break
		}
		if !appendPage(r.Data.Records) {
			break
		}
	}
	return result
}
