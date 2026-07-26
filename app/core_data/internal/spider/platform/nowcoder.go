package platform

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"github.com/PuerkitoBio/goquery"
)

// NewNowCoder 牛客竞赛站爬虫。
// 绑定 username = 竞赛 UID（数字）。
//
// 能力拆分：
//   - FetchSubmitLog → nowcoder_submit.go（practice-coding + 训练 history）
//   - FetchContestLog → nowcoder_contest.go（参赛历史，真实 start/end）
//   - 比赛页时间 → nowcoder_contest_time.go
//   - 题号身份 → nowcoder_identity.go
type NewNowCoder struct{}

func (nc NewNowCoder) Name() string {
	return spider.NowCoder
}

// FetchSubmitLog 合并竞赛练习提交与主站训练提交。
// username 为牛客竞赛 uid（数字），与绑定 platforms.username 一致。
func (nc NewNowCoder) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	practice, err := fetchPracticeCodingLogs(ctx, userId, username, needAll)
	if err != nil {
		return nil, err
	}
	training := fetchTrainingHistoryLogs(ctx, userId, username, needAll)
	out := make([]model.SubmitLog, 0, len(practice)+len(training))
	out = append(out, practice...)
	out = append(out, training...)
	return out, nil
}

// FetchContestLog 见 nowcoder_contest.go。

// FetchRating 从竞赛主页 HTML 解析 Rating（未参赛显示「暂无」→ hasRating=false）。
func (nc NewNowCoder) FetchRating(username string) (int, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, false, fmt.Errorf("nowcoder uid 为空")
	}
	url := fmt.Sprintf("https://ac.nowcoder.com/acm/contest/profile/%s", username)
	doc, err := getSubLogResp(url)
	if err != nil {
		return 0, false, err
	}
	ratingText := extractProfileStateNum(doc, "Rating")
	if ratingText == "" || ratingText == "暂无" || ratingText == "-" {
		return 0, false, nil
	}
	// 可能带小数（如 727.0）
	ratingText = strings.TrimSpace(strings.Split(ratingText, ".")[0])
	r, err := strconv.Atoi(ratingText)
	if err != nil {
		return 0, false, nil
	}
	return r, true, nil
}

// extractProfileStateNum 读 .my-state-item 中 label 对应的 .state-num。
func extractProfileStateNum(doc *goquery.Document, labelWant string) string {
	var out string
	doc.Find(".my-state-item").Each(func(_ int, s *goquery.Selection) {
		if out != "" {
			return
		}
		label := strings.TrimSpace(s.Find("span").First().Text())
		if label != labelWant {
			return
		}
		out = strings.TrimSpace(s.Find(".state-num").First().Text())
	})
	return out
}

func init() {
	spider.Register(NewNowCoder{})
}
