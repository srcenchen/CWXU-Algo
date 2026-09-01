package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spider/problem_fetch"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EnsureContestProblemsOnce 每场比赛只执行一次：主动发现题目 → 入库（external_id 与提交一致）→ 强制爬题面。
// AI 分析不强制：爬完后走标准闸门（有资格用户提交才分析）。
// 调用方可同步等待列表写入；爬取异步。
func (uc *ProblemUseCase) EnsureContestProblemsOnce(platform, contestID string) (status string, err error) {
	st, err := uc.ensureContestProblems(platform, contestID, false)
	return st, err
}

// EnsureContestProblemsForce 忽略 done 节流，强制重跑目录 + 无题面强制爬（牛客走比赛路径）。
func (uc *ProblemUseCase) EnsureContestProblemsForce(platform, contestID string) (status string, err error) {
	st, err := uc.ensureContestProblems(platform, contestID, true)
	return st, err
}

func (uc *ProblemUseCase) ensureContestProblems(platform, contestID string, force bool) (status string, err error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return "", fmt.Errorf("usecase not ready")
	}
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	if platform == "" || contestID == "" {
		return "", fmt.Errorf("empty platform/contestId")
	}

	// 已 done：默认直接返回（每场成功只跑一次，不因多用户重复打 OJ）
	// force=true：重置为 running 再跑
	// running 超过 5 分钟视为僵尸，允许抢占
	// failed：距上次完成不足 10 分钟则节流，避免前端/爬虫打爆 OJ
	const (
		ensureRunningTimeout = 5 * time.Minute
		ensureFailedCooldown = 10 * time.Minute
	)
	var existing model.ContestProblemEnsure
	err = uc.data.DB.Where("platform = ? AND contest_id = ?", platform, contestID).First(&existing).Error
	claimed := false
	newContest := err == gorm.ErrRecordNotFound
	if err == nil {
		if force {
			// 管理员强制：无论 done/failed/running 一律抢占重跑
			res := uc.data.DB.Model(&model.ContestProblemEnsure{}).
				Where("id = ?", existing.ID).
				Updates(map[string]interface{}{
					"status":     model.ContestEnsureRunning,
					"error_msg":  "",
					"ensured_at": nil,
					"updated_at": time.Now(),
				})
			if res.Error != nil {
				return "", res.Error
			}
			claimed = true
		} else if existing.Status == model.ContestEnsureDone {
			return existing.Status, nil
		} else if existing.Status == model.ContestEnsureRunning {
			// 僵尸 running：updated_at 过久才允许抢占
			if time.Since(existing.UpdatedAt) < ensureRunningTimeout {
				return existing.Status, nil
			}
			res := uc.data.DB.Model(&model.ContestProblemEnsure{}).
				Where("id = ? AND status = ? AND updated_at < ?", existing.ID, model.ContestEnsureRunning, time.Now().Add(-ensureRunningTimeout)).
				Updates(map[string]interface{}{
					"status":     model.ContestEnsureRunning,
					"error_msg":  "",
					"updated_at": time.Now(),
				})
			if res.Error != nil {
				return "", res.Error
			}
			if res.RowsAffected == 0 {
				return model.ContestEnsureRunning, nil
			}
			claimed = true
		} else if existing.Status == model.ContestEnsureFailed {
			if existing.EnsuredAt != nil && time.Since(*existing.EnsuredAt) < ensureFailedCooldown {
				return existing.Status, nil
			}
			res := uc.data.DB.Model(&model.ContestProblemEnsure{}).
				Where("id = ? AND status = ?", existing.ID, model.ContestEnsureFailed).
				Updates(map[string]interface{}{
					"status":    model.ContestEnsureRunning,
					"error_msg": "",
				})
			if res.Error != nil {
				return "", res.Error
			}
			if res.RowsAffected == 0 {
				_ = uc.data.DB.Where("id = ?", existing.ID).First(&existing).Error
				return existing.Status, nil
			}
			claimed = true
		}
	} else if err != gorm.ErrRecordNotFound {
		return "", err
	}

	if !claimed {
		// 抢占：插入 running；冲突则别人在做
		row := model.ContestProblemEnsure{
			Platform:  platform,
			ContestID: contestID,
			Status:    model.ContestEnsureRunning,
		}
		res := uc.data.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			return "", res.Error
		}
		if res.RowsAffected == 0 {
			if force {
				// 并发下强制：再抢一次
				_ = uc.data.DB.Model(&model.ContestProblemEnsure{}).
					Where("platform = ? AND contest_id = ?", platform, contestID).
					Updates(map[string]interface{}{
						"status":     model.ContestEnsureRunning,
						"error_msg":  "",
						"ensured_at": nil,
						"updated_at": time.Now(),
					}).Error
			} else {
				_ = uc.data.DB.Where("platform = ? AND contest_id = ?", platform, contestID).First(&existing).Error
				return existing.Status, nil
			}
		}
	}

	// 本 goroutine 执行发现
	if err := uc.runContestEnsure(platform, contestID, newContest); err != nil {
		log.Warnf("ensureContestProblems force=%v %s/%s: %v", force, platform, contestID, err)
		_ = uc.data.DB.Model(&model.ContestProblemEnsure{}).
			Where("platform = ? AND contest_id = ?", platform, contestID).
			Updates(map[string]interface{}{
				"status":     model.ContestEnsureFailed,
				"error_msg":  truncateStr(err.Error(), 500),
				"ensured_at": time.Now(),
			}).Error
		return model.ContestEnsureFailed, err
	}
	now := time.Now()
	_ = uc.data.DB.Model(&model.ContestProblemEnsure{}).
		Where("platform = ? AND contest_id = ?", platform, contestID).
		Updates(map[string]interface{}{
			"status":     model.ContestEnsureDone,
			"error_msg":  "",
			"ensured_at": now,
		}).Error
	return model.ContestEnsureDone, nil
}

func (uc *ProblemUseCase) runContestEnsure(platform, contestID string, newContest bool) error {
	// 牛客：ensure 题目时同步官方赛时（校赛常不在 cpolar，默认 3h 会错）
	if NormalizeCalendarPlatform(platform) == spider.NowCoder && uc.data != nil && uc.data.DB != nil {
		var hintStart, hintEnd time.Time
		var name, url string
		var cl model.ContestLog
		if uc.data.DB.Where("platform IN ? AND contest_id = ?", calendarPlatformAliases(spider.NowCoder), contestID).
			Order("time DESC").First(&cl).Error == nil {
			hintStart, hintEnd = cl.Time, cl.EndTime
			name, url = cl.ContestName, cl.ContestUrl
		}
		if _, _, ok := EnsureNowCoderContestCalendar(uc.data.DB, contestID, name, url, hintStart, hintEnd); ok {
			log.Infof("contest ensure %s/%s: nowcoder calendar window ready", platform, contestID)
		}
	}

	specs, err := ListContestProblemSpecs(platform, contestID)
	if err != nil || len(specs) == 0 {
		// OJ 拉列表失败（CF 机房 400/Cloudflare 等）：用站内提交反推题目目录
		// external_id 仍走 ParseProblemIdentity，与用户提交一致
		if fromSub, subErr := uc.listContestProblemsFromSubmits(platform, contestID); subErr == nil && len(fromSub) > 0 {
			log.Infof("contest ensure %s/%s: OJ list failed (%v), fallback submits n=%d",
				platform, contestID, err, len(fromSub))
			specs = fromSub
			err = nil
		}
	}
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return fmt.Errorf("empty problem list")
	}
	// 稳定排序
	sort.SliceStable(specs, func(i, j int) bool {
		return labelSortKey(specs[i].Label) < labelSortKey(specs[j].Label)
	})

	for i, sp := range specs {
		if sp.ExternalID == "" || sp.Platform == "" {
			continue
		}
		// problems 表 URL 用题库规范形态；比赛页仅作抓取回退
		bankURL := sp.URL
		contestURL := ""
		if strings.EqualFold(platform, "NowCoder") {
			if u := problem_fetch.NowCoderBankProblemURL(sp.ExternalID); u != "" {
				bankURL = u
			}
			contestURL = problem_fetch.NowCoderContestProblemURL(contestID, firstNonEmpty(sp.Label, ""))
		}
		parsed := &ParsedProblem{
			Platform:   sp.Platform,
			ExternalID: sp.ExternalID,
			Title:      sp.Title,
			URL:        firstNonEmpty(bankURL, sp.URL),
		}
		p, err := uc.UpsertProblemFromParsedNoAI(parsed)
		if err != nil {
			log.Warnf("contest ensure upsert %s/%s: %v", sp.Platform, sp.ExternalID, err)
			continue
		}
		// 先写 contest_problems，再入队（避免消费者早于目录落库、拿不到比赛页回退）
		item := model.ContestProblem{
			Platform:   platform,
			ContestID:  contestID,
			Label:      firstNonEmpty(sp.Label, sp.ExternalID),
			SortOrder:  i,
			ExternalID: sp.ExternalID,
			Title:      firstNonEmpty(sp.Title, p.Title),
			// NowCoder：存比赛页，供题库无权限时回退抓取
			URL:       firstNonEmpty(contestURL, firstNonEmpty(sp.URL, p.URL)),
			ProblemID: p.ID,
		}
		_ = uc.data.DB.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "platform"}, {Name: "contest_id"}, {Name: "label"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"sort_order", "external_id", "title", "url", "problem_id", "updated_at",
			}),
		}).Create(&item).Error
		// 仅首次创建比赛目录时补全题面。已有比赛的重新同步只更新目录，
		// 不得因历史题面缺失再次唤起整场题目。
		if shouldRequeueMissingContestContent(newContest) {
			if err := uc.ForceEnqueueFetchContest(p.ID, contestURL); err != nil {
				log.Warnf("contest ensure fetch %d: %v", p.ID, err)
			}
		}
	}

	// 回写 contest_logs.total_count（同场所有用户行）
	n := int64(0)
	_ = uc.data.DB.Model(&model.ContestProblem{}).
		Where("platform = ? AND contest_id = ?", platform, contestID).
		Count(&n).Error
	if n > 0 {
		_ = uc.data.DB.Model(&model.ContestLog{}).
			Where("platform = ? AND contest_id = ?", platform, contestID).
			Update("total_count", n).Error
	}
	return nil
}

func shouldRequeueMissingContestContent(newContest bool) bool {
	return newContest
}

// listContestProblemsFromSubmits 从 submit_logs 反推比赛题目（OJ 列表不可用时兜底）。
func (uc *ProblemUseCase) listContestProblemsFromSubmits(platform, contestID string) ([]ContestProblemSpec, error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return nil, fmt.Errorf("no db")
	}
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	if platform == "" || contestID == "" {
		return nil, fmt.Errorf("empty")
	}
	// CF 提交 contest 字段为数字 id；problem 形如 "A-Title"
	type row struct {
		Contest string `gorm:"column:contest"`
		Problem string `gorm:"column:problem"`
	}
	var rows []row
	q := uc.data.DB.Model(&model.SubmitLog{}).
		Select("contest, problem").
		Where("platform = ?", platform).
		Where("problem <> '' AND problem IS NOT NULL")
	// contest 精确或 gym 前缀
	if platform == "CodeForces" || platform == "Codeforces" {
		q = q.Where("contest = ? OR contest = ? OR contest = ?", contestID, "-"+contestID, "gym"+contestID)
	} else {
		q = q.Where("contest = ?", contestID)
	}
	if err := q.Group("contest, problem").Limit(80).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// 部分平台 contest 为空，problem 自带 id：再试 contest 模糊
		_ = uc.data.DB.Model(&model.SubmitLog{}).
			Select("contest, problem").
			Where("platform = ? AND (contest = ? OR contest LIKE ?)", platform, contestID, "%"+contestID+"%").
			Where("problem <> '' AND problem IS NOT NULL").
			Group("contest, problem").
			Limit(80).Find(&rows).Error
	}
	seen := map[string]struct{}{}
	var specs []ContestProblemSpec
	for _, r := range rows {
		parsed, err := ParseProblemIdentity(platform, r.Contest, r.Problem)
		if err != nil || parsed == nil || parsed.ExternalID == "" {
			continue
		}
		if _, ok := seen[parsed.ExternalID]; ok {
			continue
		}
		seen[parsed.ExternalID] = struct{}{}
		label := strings.TrimSpace(parsed.ExternalID)
		// CF external 2247A → A；gym102861A → A
		if platform == "CodeForces" || platform == "Codeforces" {
			ext := parsed.ExternalID
			if strings.HasPrefix(strings.ToLower(ext), "gym") {
				ext = ext[3:]
			}
			for i := 0; i < len(ext); i++ {
				if (ext[i] >= 'A' && ext[i] <= 'Z') || (ext[i] >= 'a' && ext[i] <= 'z') {
					label = ext[i:]
					break
				}
			}
		}
		specs = append(specs, ContestProblemSpec{
			Label:      label,
			ExternalID: parsed.ExternalID,
			Title:      firstNonEmpty(parsed.Title, label),
			URL:        parsed.URL,
			Platform:   parsed.Platform,
		})
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no submits for contest")
	}
	sort.SliceStable(specs, func(i, j int) bool {
		return labelSortKey(specs[i].Label) < labelSortKey(specs[j].Label)
	})
	return specs, nil
}

// UpsertProblemFromParsedNoAI 入库但不按 actor 强制 AI；题面强制爬取走 ForceEnqueueFetchContest。
func (uc *ProblemUseCase) UpsertProblemFromParsedNoAI(parsed *ParsedProblem) (*model.Problem, error) {
	if uc == nil || parsed == nil || parsed.Platform == "" || parsed.ExternalID == "" {
		return nil, fmt.Errorf("invalid parsed problem")
	}
	var existing model.Problem
	err := uc.data.DB.Where("platform = ? AND external_id = ?", parsed.Platform, parsed.ExternalID).
		First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		status := model.ProblemStatusPending
		if parsed.SkipFetch {
			status = model.ProblemStatusSkipped
		}
		p := model.Problem{
			Platform:   parsed.Platform,
			ExternalID: parsed.ExternalID,
			Title:      firstNonEmpty(parsed.Title, parsed.ExternalID),
			URL:        parsed.URL,
			Status:     status,
			Tags:       model.StringArray{},
		}
		if err := uc.data.DB.Create(&p).Error; err != nil {
			if err2 := uc.data.DB.Where("platform = ? AND external_id = ?", parsed.Platform, parsed.ExternalID).
				First(&existing).Error; err2 != nil {
				return nil, err
			}
		} else {
			existing = p
		}
	} else if err != nil {
		return nil, err
	} else {
		updates := map[string]interface{}{}
		if existing.Title == "" && parsed.Title != "" {
			updates["title"] = parsed.Title
			existing.Title = parsed.Title
		}
		if existing.URL == "" && parsed.URL != "" {
			updates["url"] = parsed.URL
			existing.URL = parsed.URL
		}
		if len(updates) > 0 {
			_ = uc.data.DB.Model(&existing).Updates(updates).Error
		}
	}
	return &existing, nil
}

// RequeueMissingContestProblemFetches 对本场已入库但无题面的题强制再爬（优先比赛路径）。
// 用于：ensure 已 done 后新挂上 contest 映射、或曾被标 FAILED_PERM。
func (uc *ProblemUseCase) RequeueMissingContestProblemFetches(platform, contestID string) int {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return 0
	}
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	if platform == "" || contestID == "" {
		return 0
	}
	var cps []model.ContestProblem
	_ = uc.data.DB.Where("platform = ? AND contest_id = ?", platform, contestID).Find(&cps).Error
	n := 0
	for _, cp := range cps {
		var p model.Problem
		if cp.ProblemID > 0 {
			if uc.data.DB.First(&p, cp.ProblemID).Error != nil {
				continue
			}
		} else if cp.ExternalID != "" {
			if uc.data.DB.Where("platform = ? AND external_id = ?", platform, cp.ExternalID).
				First(&p).Error != nil {
				continue
			}
		} else {
			continue
		}
		if strings.TrimSpace(p.ContentMD) != "" {
			continue
		}
		contestURL := strings.TrimSpace(cp.URL)
		if !problem_fetch.IsNowCoderContestURL(contestURL) {
			contestURL = problem_fetch.NowCoderContestProblemURL(contestID, cp.Label)
		}
		if e := uc.ForceEnqueueFetchContest(p.ID, contestURL); e == nil {
			n++
		}
	}
	return n
}

// ForceEnqueueFetchContest 比赛 ensure 强制爬题面（忽略资格；爬完走标准 AI 闸门）。
// contestFallbackURL：NowCoder 比赛页如 /acm/contest/137561/A，题库页无权限时回退。
func (uc *ProblemUseCase) ForceEnqueueFetchContest(problemID uint, contestFallbackURL ...string) error {
	if uc == nil || problemID == 0 {
		return nil
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return err
	}
	// 已有题面：尝试标准 AI（有资格提交者）
	if strings.TrimSpace(p.ContentMD) != "" {
		if len(nonEmptyTags(p.Tags)) == 0 {
			return uc.enqueueAnalyzePrio(problemID, mqPriorityIncremental)
		}
		return nil
	}
	if p.Status == model.ProblemStatusSkipped {
		return nil
	}
	// 比赛同步是周期性触发的，不能把同一道历史失败题无限重灌 MQ。
	// 只有从未尝试过的 PENDING 题允许由目录发现首次入队；失败重试由
	// handleFetchError 的退避机制负责，管理员显式重试走单独接口。
	if !shouldForceEnqueueContestFetch(p) {
		return nil
	}
	claimed := uc.data.DB.Model(&model.Problem{}).
		Where("id = ? AND status = ? AND fetch_attempts = ?", p.ID, model.ProblemStatusPending, 0).
		Update("status", model.ProblemStatusFetching)
	if claimed.Error != nil {
		return claimed.Error
	}
	if claimed.RowsAffected == 0 {
		return nil
	}
	if uc.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	var fb []string
	for _, u := range contestFallbackURL {
		u = strings.TrimSpace(u)
		if u != "" {
			fb = append(fb, u)
		}
	}
	// 牛客：比赛页作主 URL（优先抓），题库页作 fallback
	primary := p.URL
	fallbacks := fb
	if len(fb) > 0 && problem_fetch.IsNowCoderContestURL(fb[0]) {
		primary = fb[0]
		fallbacks = nil
		if bank := problem_fetch.NowCoderBankProblemURL(p.ExternalID); bank != "" {
			fallbacks = []string{bank}
		}
		// 其余 fallback 附后
		for _, u := range fb[1:] {
			if u != "" && u != primary {
				fallbacks = append(fallbacks, u)
			}
		}
	}
	body, _ := json.Marshal(event.ProblemFetchEvent{
		ProblemID:    p.ID,
		Platform:     p.Platform,
		ExternalID:   p.ExternalID,
		URL:          primary,
		FallbackURLs: fallbacks,
		Force:        true,  // 忽略用户爬取资格
		SkipAnalyze:  false, // 爬完走 enqueueAnalyze（submitter AI 闸门）
		ActorUserID:  0,
	})
	if err := uc.declareProblemQueue("problem_fetch"); err != nil {
		_ = uc.data.DB.Model(&model.Problem{}).Where("id = ? AND status = ?", p.ID, model.ProblemStatusFetching).Update("status", model.ProblemStatusPending).Error
		return err
	}
	// 异步入队：比赛 ensure 批量加题时勿同步等 confirm（易拖死 HTTP）
	if err := uc.mq.Publish("", "problem_fetch", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Priority:     mqPriorityIncremental,
	}); err != nil {
		_ = uc.data.DB.Model(&model.Problem{}).Where("id = ? AND status = ?", p.ID, model.ProblemStatusFetching).Update("status", model.ProblemStatusPending).Error
		return err
	}
	return nil
}

func shouldForceEnqueueContestFetch(p model.Problem) bool {
	return p.Status == model.ProblemStatusPending && p.FetchAttempts == 0
}

// ListContestProblems 读目录 + 关联 problem 状态。返回 ensureStatus、ensureError。
func (uc *ProblemUseCase) ListContestProblems(platform, contestID string) (list []map[string]interface{}, status, ensureErr string, err error) {
	if uc == nil || uc.data == nil {
		return nil, "", "", fmt.Errorf("usecase not ready")
	}
	var ensure model.ContestProblemEnsure
	if uc.data.DB.Where("platform = ? AND contest_id = ?", platform, contestID).First(&ensure).Error == nil {
		status = ensure.Status
		ensureErr = ensure.ErrorMsg
	}
	var items []model.ContestProblem
	_ = uc.data.DB.Where("platform = ? AND contest_id = ?", platform, contestID).
		Order("sort_order asc, id asc").Find(&items).Error

	// 批量 problem
	ids := make([]uint, 0, len(items))
	for _, it := range items {
		if it.ProblemID > 0 {
			ids = append(ids, it.ProblemID)
		}
	}
	probMap := map[uint]model.Problem{}
	if len(ids) > 0 {
		var probs []model.Problem
		_ = uc.data.DB.Where("id IN ?", ids).Find(&probs).Error
		for _, p := range probs {
			probMap[p.ID] = p
		}
	}
	out := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		m := map[string]interface{}{
			"label":      it.Label,
			"externalId": it.ExternalID,
			"title":      it.Title,
			"url":        it.URL,
			"problemId":  it.ProblemID,
			"sortOrder":  it.SortOrder,
		}
		if p, ok := probMap[it.ProblemID]; ok {
			m["title"] = firstNonEmpty(p.Title, it.Title)
			m["status"] = p.Status
			m["hasContent"] = strings.TrimSpace(p.ContentMD) != ""
			m["difficulty"] = p.Difficulty
			m["tags"] = []string(p.Tags)
		} else {
			m["status"] = model.ProblemStatusPending
			m["hasContent"] = false
		}
		out = append(out, m)
	}
	return out, status, ensureErr, nil
}
