package platform

import (
	"bytes"
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
)

const (
	lojAPIBase      = "https://api.loj.ac/api/"
	lojTakeCount    = 10 // 服务端实际上限约 10
	lojMaxPages     = 500
	lojPageDelay    = 300 * time.Millisecond
	lojSubmitPrefix = "loj-"
)

// NewLOJ LibreOJ（loj.ac）提交爬虫：公开 JSON API，无需登录。
type NewLOJ struct{}

func (p NewLOJ) Name() string { return spider.LOJ }

type lojQuerySubmissionReq struct {
	Locale    string `json:"locale"`
	Submitter string `json:"submitter"`
	TakeCount int    `json:"takeCount"`
	MaxID     *int64 `json:"maxId,omitempty"`
}

type lojQuerySubmissionResp struct {
	Error       string             `json:"error"`
	Submissions []lojSubmissionMeta `json:"submissions"`
	HasSmaller  bool               `json:"hasSmallerId"`
	HasLarger   bool               `json:"hasLargerId"`
}

type lojSubmissionMeta struct {
	ID           int64          `json:"id"`
	CodeLanguage string         `json:"codeLanguage"`
	Status       string         `json:"status"`
	SubmitTime   string         `json:"submitTime"`
	ProblemTitle string         `json:"problemTitle"`
	Problem      lojProblemMeta `json:"problem"`
}

type lojProblemMeta struct {
	ID        int64 `json:"id"`
	DisplayID int64 `json:"displayId"`
}

type lojUserMetaResp struct {
	Error string `json:"error"`
	Meta  *struct {
		Username string `json:"username"`
		Rating   int    `json:"rating"`
	} `json:"meta"`
}

func lojPostJSON(ctx context.Context, path string, body any, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lojAPIBase+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoAlgo-Spider/1.0)")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return fmt.Errorf("loj %s: %w", path, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("loj %s read: %w", path, err)
	}
	// NestJS：校验失败 400；业务成功多为 200/201
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("loj %s status %d: %s", path, resp.StatusCode, truncateStr(string(rb), 200))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("loj %s json: %w", path, err)
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func mapLOJStatus(raw string) string {
	switch strings.TrimSpace(raw) {
	case "Accepted":
		return "AC"
	case "WrongAnswer":
		return "WA"
	case "CompilationError":
		return "CE"
	case "TimeLimitExceeded":
		return "TLE"
	case "MemoryLimitExceeded":
		return "MLE"
	case "RuntimeError":
		return "RE"
	case "OutputLimitExceeded":
		return "OLE"
	case "PartiallyCorrect":
		return "PC"
	case "Pending":
		return "Judging"
	case "SystemError", "ConfigurationError", "JudgementFailed", "FileError", "Canceled":
		return raw
	default:
		if raw == "" {
			return "WA"
		}
		return raw
	}
}

func parseLOJTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// 部分实例无时区
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.000Z", s, time.UTC); err == nil {
		return t
	}
	return time.Time{}
}

func lojSubmissionToLog(userId int64, s lojSubmissionMeta) model.SubmitLog {
	displayID := s.Problem.DisplayID
	if displayID == 0 {
		displayID = s.Problem.ID
	}
	ext := ""
	if displayID > 0 {
		ext = strconv.FormatInt(displayID, 10)
	}
	title := strings.TrimSpace(s.ProblemTitle)
	problem := title
	if ext != "" {
		if title != "" {
			problem = "#" + ext + " " + title
		} else {
			problem = "#" + ext
		}
	}
	return model.SubmitLog{
		UserID:     userId,
		Platform:   spider.LOJ,
		SubmitID:   lojSubmitPrefix + strconv.FormatInt(s.ID, 10),
		Problem:    problem,
		ExternalID: ext,
		Lang:       strings.TrimSpace(s.CodeLanguage),
		Status:     mapLOJStatus(s.Status),
		Time:       parseLOJTime(s.SubmitTime),
	}
}

// FetchSubmitLog 拉取 LibreOJ 公开提交。
// needAll=false：仅最新一页；needAll=true：maxId 向前翻页直至结束。
func (p NewLOJ) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("loj username 为空")
	}

	var (
		res    []model.SubmitLog
		maxID  *int64
		pages  int
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pages++
		if pages > lojMaxPages {
			break
		}
		reqBody := lojQuerySubmissionReq{
			Locale:    "zh_CN",
			Submitter: username,
			TakeCount: lojTakeCount,
			MaxID:     maxID,
		}
		var resp lojQuerySubmissionResp
		if err := lojPostJSON(ctx, "submission/querySubmission", reqBody, &resp); err != nil {
			return nil, err
		}
		if resp.Error == "NO_SUCH_USER" {
			return nil, fmt.Errorf("loj 用户不存在: %s", username)
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("loj querySubmission: %s", resp.Error)
		}
		if len(resp.Submissions) == 0 {
			break
		}
		var minID int64
		for i, s := range resp.Submissions {
			res = append(res, lojSubmissionToLog(userId, s))
			if i == 0 || s.ID < minID {
				minID = s.ID
			}
		}
		if !needAll {
			break
		}
		if !resp.HasSmaller {
			break
		}
		next := minID - 1
		maxID = &next
		time.Sleep(lojPageDelay)
	}
	return res, nil
}

// FetchRating 用户 meta 中的 rating（未参赛可能为 0）。
func (p NewLOJ) FetchRating(username string) (int, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, false, fmt.Errorf("loj username 为空")
	}
	var resp lojUserMetaResp
	if err := lojPostJSON(context.Background(), "user/getUserMeta", map[string]string{"username": username}, &resp); err != nil {
		return 0, false, err
	}
	if resp.Error == "NO_SUCH_USER" {
		return 0, false, fmt.Errorf("loj 用户不存在: %s", username)
	}
	if resp.Error != "" {
		return 0, false, fmt.Errorf("loj getUserMeta: %s", resp.Error)
	}
	if resp.Meta == nil {
		return 0, false, nil
	}
	// rating=0 视为无有效 rating（与多数 OJ 一致）
	if resp.Meta.Rating <= 0 {
		return 0, false, nil
	}
	return resp.Meta.Rating, true, nil
}

func init() {
	spider.Register(&NewLOJ{})
}
