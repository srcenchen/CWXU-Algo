package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cwxu-algo/api/user/v1/subscription"
	data2 "cwxu-algo/app/common/data"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/common/utils/sqllike"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/loadgate"
	"cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spider/problem_fetch"
	"cwxu-algo/app/core_data/internal/userrpc"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errProblemFetchPaused = errors.New("fetch paused")
	errSkipProblemBank    = errors.New("submit does not belong in problem bank")
)

type ProblemUseCase struct {
	data   *data.Data
	mq     *event.RabbitMQ
	tagger *ProblemTagger
	reg    *registry.Registrar

	// profileTask 画像 MQ 入队（可选；nil 时只同步算小用户）
	profileTask  *task.UserProfileTask
	abilityStats task.AbilityStatsRefresher

	orgUsersMu         sync.Mutex
	orgUsersCache      map[int64]struct{} // 兼容旧缓存（= fetch 集合）
	orgUsersAt         time.Time
	pipelineUsersCache *pipelineUserSets
	pipelineUsersAt    time.Time

	// adminOp 防止补全/重置/重试并发互踩
	adminOpMu   sync.Mutex
	adminOpName string
}

func NewProblemUseCase(data *data.Data, mq *event.RabbitMQ, tagger *ProblemTagger, reg *discovery.Register, profileTask *task.UserProfileTask, abilityStats task.AbilityStatsRefresher) *ProblemUseCase {
	var r *registry.Registrar
	if reg != nil {
		r = &reg.Reg
	}
	uc := &ProblemUseCase{data: data, mq: mq, tagger: tagger, reg: r, profileTask: profileTask, abilityStats: abilityStats}
	if data != nil && data.DB != nil && data.DB.Dialector.Name() == "postgres" {
		go uc.runAbilityMaintenanceRecovery()
	}
	return uc
}

// MQ 优先级：队列需 x-max-priority
// bulk=回填/重置；incremental=爬虫增量；user=题单/用户主动加题（顶）
const (
	mqPriorityBulk        uint8 = 1
	mqPriorityIncremental uint8 = 9
	mqPriorityUser        uint8 = 10
	mqMaxPriority         int32 = 10
)

// BindSubmitsAfterSpider 爬虫写入提交后绑定/创建题库（增量）。普通增量
// 不触发画像重构；画像由每日任务或具体 OJ 换绑后的完成标记触发。
func (uc *ProblemUseCase) BindSubmitsAfterSpider(userId int64) error {
	return uc.bindSubmitsAfterSpider(userId, "")
}

// BindSubmitsAfterSpiderForPlatform is used when a platform-specific full
// crawl has just completed. Only the matching rebind marker may trigger a
// forced profile rebuild, so another OJ's incremental sync cannot consume it.
func (uc *ProblemUseCase) BindSubmitsAfterSpiderForPlatform(userID int64, platform string) error {
	return uc.bindSubmitsAfterSpider(userID, strings.TrimSpace(platform))
}

func (uc *ProblemUseCase) bindSubmitsAfterSpider(userId int64, platform string) error {
	ctx := context.Background()
	markedForRebuild := false
	if platform != "" && uc.data != nil && uc.data.RDB != nil {
		var err error
		markedForRebuild, err = task.HasProfileRebuildAfterBinding(ctx, uc.data.RDB, userId, platform)
		if err != nil {
			// Redis only carries the post-rebind trigger. Keep the marker for a
			// retry, but do not block durable submit/problem binding on a cache
			// outage.
			log.Warnf("BindSubmitsAfterSpider profile marker user=%d platform=%s: %v", userId, platform, err)
			markedForRebuild = false
		}
	}
	var highWatermark uint
	if err := uc.data.DB.WithContext(ctx).Model(&model.SubmitLog{}).
		Where("user_id = ?", userId).
		Select("COALESCE(MAX(id), 0)").Scan(&highWatermark).Error; err != nil {
		log.Errorf("BindSubmitsAfterSpider watermark: %v", err)
		return err
	}
	const batchSize = 500
	var cursor uint
	boundAC := make([]model.SubmitLog, 0, 32)
	var errs []error
	for cursor < highWatermark {
		var logs []model.SubmitLog
		if err := uc.data.DB.WithContext(ctx).
			Where("user_id = ? AND id > ? AND id <= ? AND (problem_id IS NULL OR problem_id = 0)", userId, cursor, highWatermark).
			Order("id ASC").Limit(batchSize).Find(&logs).Error; err != nil {
			log.Errorf("BindSubmitsAfterSpider query: %v", err)
			return errors.Join(append(errs, err)...)
		}
		if len(logs) == 0 {
			break
		}
		cursor = logs[len(logs)-1].ID
		// 每批预查已存在题；单批最多 500 条，避免无界内存。
		cache := uc.prefetchProblemsForLogs(logs)
		for i := range logs {
			// 系统过载时放缓逐条绑定，避免整点风暴雪上加霜
			if i%25 == 0 && loadgate.Global().Overloaded() {
				time.Sleep(200 * time.Millisecond)
			}
			if _, _, err := uc.resolveOneWithCache(&logs[i], true, cache); err != nil {
				if errors.Is(err, errSkipProblemBank) {
					continue
				}
				log.Debugf("resolve submit %d: %v", logs[i].ID, err)
				errs = append(errs, err)
				continue
			}
			if logs[i].IsAC && logs[i].ProblemID != nil && *logs[i].ProblemID > 0 {
				boundAC = append(boundAC, logs[i])
			}
		}
	}
	// 已绑定但预聚合仍停在 e:/n: 的存量键一并升级（画像 JOIN 也兼容 e:）
	if err := dal.PromoteUserACFromBoundSubmits(ctx, uc.data.DB, userId); err != nil {
		log.Warnf("PromoteUserACFromBoundSubmits user=%d: %v", userId, err)
		errs = append(errs, err)
	}
	// 绑题后补 user_problem_status / 首次 AC 增量（与全量 Rebuild 互补）
	if len(boundAC) > 0 {
		if err := dal.ApplyUserProblemStatusFromSubmits(ctx, uc.data.DB, boundAC); err != nil {
			log.Warnf("BindSubmits ApplyUserProblemStatus user=%d: %v", userId, err)
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if markedForRebuild {
		if uc.profileTask == nil {
			return fmt.Errorf("user profile publisher unavailable")
		}
		result := uc.profileTask.DoForce(userId)
		if !result.KeepClaim() {
			return fmt.Errorf("force publish user profile %d failed", userId)
		}
		if err := task.ClearProfileRebuildAfterBinding(ctx, uc.data.RDB, userId, platform); err != nil {
			return err
		}
	}
	return nil
}

// RebuildAllUserProfiles 全站「仅重建画像」：对有 AC 的用户强制入队画像重建
// （队列内 RebuildUserTagAC 重算后验、个人过程与标签置信度，不重新爬取 OJ 提交）。
// 用于能力雷达评分模型升级后的一次性回填，或站管手动触发。
func (uc *ProblemUseCase) RebuildAllUserProfiles(ctx context.Context) (candidates, published int, err error) {
	if uc.data == nil || uc.data.DB == nil || uc.profileTask == nil || uc.abilityStats == nil {
		return 0, 0, errors.New("RebuildAllUserProfiles: dependencies unavailable")
	}
	pending, _, err := ensureAbilityMaintenancePending(ctx, uc.data.DB, model.AbilityMaintenancePending{
		Scope: "global:rebuild", Operation: "rebuild", TagsChanged: true, DifficultyChanged: true,
	})
	if err != nil {
		return 0, 0, err
	}
	if pending.Phase == "fence_finalized" {
		var count int64
		if err := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&count).Error; err != nil {
			return 0, 0, err
		}
		if _, err := uc.relayAbilityMaintenanceTargets(ctx, pending); err != nil {
			return int(count), 0, err
		}
		return int(count), int(count), nil
	}
	profileToken, err := beginGlobalProfileInvalidationForIntent(ctx, uc.data.RDB, pending.OperationID)
	if err != nil {
		return 0, 0, err
	}
	owner := profileToken.Owner
	if err := claimAbilityMaintenancePending(ctx, uc.data.DB, pending, owner); err != nil {
		return 0, 0, errors.Join(err, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, profileToken))
	}
	workCtx := profileToken.Context()
	if err := validateProfileInvalidation(workCtx, uc.data.RDB, profileGlobalGenerationKey, profileToken); err != nil {
		return 0, 0, errors.Join(err, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, profileToken))
	}
	abandon := func(cause error) error {
		return errors.Join(cause, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, profileToken))
	}
	validate := func() error {
		return validateProfileInvalidation(workCtx, uc.data.RDB, profileGlobalGenerationKey, profileToken)
	}
	if pending.Phase == "derived_ready" {
		var count int64
		if err := uc.data.DB.WithContext(workCtx).Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&count).Error; err != nil {
			return 0, 0, abandon(err)
		}
		if err := FinishGlobalProfileInvalidation(workCtx, uc.data.RDB, profileToken); err != nil {
			return int(count), 0, abandon(err)
		}
		if err := advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "fence_finalized"); err != nil {
			return int(count), 0, err
		}
		if _, err := uc.relayAbilityMaintenanceTargets(ctx, pending); err != nil {
			return int(count), 0, err
		}
		return int(count), int(count), nil
	}
	if pending.Phase == "intent" {
		if err := markAbilityMaintenanceFacts(workCtx, uc.data.DB, pending, true, true); err != nil {
			return 0, 0, abandon(err)
		}
	}
	modelVersion := pending.TargetModelVersion
	if pending.Phase == "facts" {
		modelVersion, err = uc.refreshAbilityStatsForMaintenance(workCtx, pending)
		if err != nil {
			return 0, 0, abandon(err)
		}
		if err := validate(); err != nil {
			return 0, 0, abandon(err)
		}
	}
	if err := validate(); err != nil {
		return 0, 0, abandon(err)
	}
	if pending.Phase == "model_ready" {
		var userIDs []int64
		if err := uc.data.DB.WithContext(workCtx).Model(&model.UserACProblem{}).Distinct("user_id").Order("user_id ASC").Pluck("user_id", &userIDs).Error; err != nil {
			log.Errorf("RebuildAllUserProfiles: pluck users: %v", err)
			return 0, 0, abandon(err)
		}
		if err := prepareAbilityMaintenanceRebuildTargets(workCtx, uc.data.DB, pending, userIDs); err != nil {
			return len(userIDs), 0, abandon(err)
		}
	}
	var targetCount int64
	if err := uc.data.DB.WithContext(workCtx).Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error; err != nil {
		return 0, 0, abandon(err)
	}
	if pending.Phase == "targets_ready" {
		if err := rebuildPendingAbilityMaintenanceTargets(workCtx, uc.data.DB, pending, validate, func(userID int64) error {
			return dal.RebuildUserTagACForUser(workCtx, uc.data.DB, userID)
		}); err != nil {
			return int(targetCount), 0, abandon(err)
		}
		if err := validate(); err != nil {
			return int(targetCount), 0, abandon(err)
		}
		if err := stageRebuiltAbilityMaintenanceTargets(workCtx, uc.data.DB, pending); err != nil {
			return int(targetCount), 0, abandon(err)
		}
	}
	if err := FinishGlobalProfileInvalidation(workCtx, uc.data.RDB, profileToken); err != nil {
		return int(targetCount), published, abandon(err)
	}
	if err := advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "fence_finalized"); err != nil {
		return int(targetCount), published, err
	}
	if _, err := uc.relayAbilityMaintenanceTargets(ctx, pending); err != nil {
		return int(targetCount), published, err
	}
	published = int(targetCount)
	log.Infof("RebuildAllUserProfiles model_version=%d candidates=%d published=%d", modelVersion, targetCount, published)
	return int(targetCount), published, nil
}

// resolveOne 解析并绑定单条提交；返回 (problem, isNew, err)
// highPriority=true：增量爬虫路径，MQ 最高优先级
func (uc *ProblemUseCase) resolveOne(sl *model.SubmitLog, highPriority bool) (*model.Problem, bool, error) {
	return uc.resolveOneWithCache(sl, highPriority, nil)
}

// prefetchProblemsForLogs 批量预查 (platform, external_id) 已存在题，供批量绑定复用。
// 返回 nil 表示预查失败（调用方按无缓存逐条查询）。
func (uc *ProblemUseCase) prefetchProblemsForLogs(logs []model.SubmitLog) map[string]*model.Problem {
	if len(logs) == 0 {
		return map[string]*model.Problem{}
	}
	seen := map[string]struct{}{}
	pairs := make([][]interface{}, 0, len(logs))
	for i := range logs {
		parsed, err := ParseProblemIdentity(logs[i].Platform, logs[i].Contest, logs[i].Problem)
		if err != nil || parsed.SkipBank {
			continue
		}
		k := parsed.Platform + "\x00" + parsed.ExternalID
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		pairs = append(pairs, []interface{}{parsed.Platform, parsed.ExternalID})
	}
	if len(pairs) == 0 {
		return map[string]*model.Problem{}
	}
	var rows []model.Problem
	if err := uc.data.DB.Where("(platform, external_id) IN ?", pairs).Find(&rows).Error; err != nil {
		log.Warnf("prefetchProblemsForLogs: %v", err)
		return nil
	}
	out := make(map[string]*model.Problem, len(rows))
	for i := range rows {
		p := rows[i]
		out[p.Platform+"\x00"+p.ExternalID] = &p
	}
	return out
}

// resolveOneWithCache 同 resolveOne；cache 非 nil 时命中即免逐条 SELECT，写路径后回填缓存
func (uc *ProblemUseCase) resolveOneWithCache(sl *model.SubmitLog, highPriority bool, cache map[string]*model.Problem) (*model.Problem, bool, error) {
	parsed, err := ParseProblemIdentity(sl.Platform, sl.Contest, sl.Problem)
	if err != nil {
		return nil, false, err
	}
	// SkipBank：明确不进题库的平台/记录
	if parsed.SkipBank {
		return nil, false, fmt.Errorf("%w: %s", errSkipProblemBank, parsed.Platform)
	}

	cacheKey := parsed.Platform + "\x00" + parsed.ExternalID
	var existing model.Problem
	if cache != nil {
		if p, ok := cache[cacheKey]; ok && p != nil {
			existing = *p
			err = nil
		} else {
			err = gorm.ErrRecordNotFound
		}
	} else {
		err = uc.data.DB.Where("platform = ? AND external_id = ?", parsed.Platform, parsed.ExternalID).First(&existing).Error
	}
	isNew := false
	if err == gorm.ErrRecordNotFound {
		status := model.ProblemStatusPending
		if parsed.SkipFetch {
			status = model.ProblemStatusSkipped
		}
		p := model.Problem{
			Platform:   parsed.Platform,
			ExternalID: parsed.ExternalID,
			Title:      firstNonEmpty(parsed.Title, sl.Problem),
			URL:        parsed.URL,
			Status:     status,
			Tags:       model.StringArray{},
		}
		t := sl.Time
		p.LastSubmittedAt = &t
		if err := uc.data.DB.Create(&p).Error; err != nil {
			// 并发唯一冲突 → 再查
			if err2 := uc.data.DB.Where("platform = ? AND external_id = ?", parsed.Platform, parsed.ExternalID).
				First(&existing).Error; err2 != nil {
				return nil, false, err
			}
		} else {
			existing = p
			isNew = true
		}
	} else if err != nil {
		return nil, false, err
	} else {
		// 更新最近提交时间
		if existing.LastSubmittedAt == nil || sl.Time.After(*existing.LastSubmittedAt) {
			_ = uc.data.DB.Model(&existing).Update("last_submitted_at", sl.Time).Error
			existing.LastSubmittedAt = &sl.Time
		}
		if shouldReplaceProblemTitle(existing.Platform, existing.Title, parsed.Title) {
			_ = uc.data.DB.Model(&existing).Update("title", parsed.Title).Error
			existing.Title = parsed.Title
		}
		if existing.URL == "" && parsed.URL != "" {
			_ = uc.data.DB.Model(&existing).Update("url", parsed.URL).Error
			existing.URL = parsed.URL
		}
	}

	// 绑定 submit；条件更新防止并发 binder 覆盖已变化的归属。
	pid := existing.ID
	oldExternalID := sl.ExternalID
	if err := uc.data.DB.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.SubmitLog{}).
			Where("id = ? AND user_id = ? AND (problem_id IS NULL OR problem_id = 0)", sl.ID, sl.UserID).
			Updates(map[string]interface{}{
				"problem_id":  pid,
				"external_id": parsed.ExternalID,
			})
		if res.Error != nil {
			return fmt.Errorf("bind submit %d: %w", sl.ID, res.Error)
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("bind submit %d: expected one row, updated %d", sl.ID, res.RowsAffected)
		}
		// 画像预聚合：绑题后把 e:/n: 键升级为 p:{id}（写路径在绑题前多写 e:/n:）。
		if sl.IsAC {
			oldKeys := []string{
				model.ACProblemKey(sl.Platform, parsed.ExternalID, sl.Problem, nil),
				model.ACProblemKey(sl.Platform, oldExternalID, sl.Problem, nil),
				model.ACProblemKey(parsed.Platform, parsed.ExternalID, sl.Problem, nil),
			}
			if err := dal.PromoteUserACKeysToProblemID(context.Background(), tx, sl.UserID, oldKeys, pid); err != nil {
				return fmt.Errorf("promote user AC keys user=%d pid=%d: %w", sl.UserID, pid, err)
			}
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	sl.ProblemID = &pid
	sl.ExternalID = parsed.ExternalID

	prio := mqPriorityBulk
	if highPriority {
		prio = mqPriorityIncremental
	}

	// 题面爬取：仅近窗有爬取资格用户提交才入队（默认非公共域组织，可个人覆盖）
	// AI：enqueueAnalyzePrio 内统一闸门（独立资格）
	allowFetch := !parsed.SkipFetch && uc.shouldEnqueueFetch(existing.ID)

	// 新题且可爬
	if isNew && allowFetch && existing.Status == model.ProblemStatusPending {
		if err := uc.enqueueFetchPrio(existing.ID, existing.Platform, existing.ExternalID, existing.URL, prio); err != nil {
			log.Errorf("enqueue problem %d: %v", existing.ID, err)
		}
	}
	// 永久失败：升级标记后不再入队
	if existing.Status == model.ProblemStatusFailed && isPermanentFetchError(existing.ErrorMsg) {
		_ = uc.data.DB.Model(&existing).Update("status", model.ProblemStatusFailedPerm).Error
		existing.Status = model.ProblemStatusFailedPerm
	}

	// 已存在但题面未完成：补入队；FAILED_PERM 永不重试；已 COMPLETED：不入队
	if !isNew && !parsed.SkipFetch {
		switch existing.Status {
		case model.ProblemStatusPending, model.ProblemStatusFailed:
			if strings.TrimSpace(existing.ContentMD) == "" {
				if allowFetch {
					if err := uc.enqueueFetchPrio(existing.ID, existing.Platform, existing.ExternalID, existing.URL, prio); err != nil {
						log.Errorf("re-enqueue fetch problem %d: %v", existing.ID, err)
					}
				}
			} else {
				_ = uc.data.DB.Model(&existing).Update("status", model.ProblemStatusTagging).Error
				if err := uc.enqueueAnalyzePrio(existing.ID, prio); err != nil {
					log.Errorf("re-enqueue analyze problem %d: %v", existing.ID, err)
				}
			}
		case model.ProblemStatusTagging:
			if strings.TrimSpace(existing.ContentMD) != "" {
				if err := uc.enqueueAnalyzePrio(existing.ID, prio); err != nil {
					log.Errorf("re-enqueue analyze problem %d: %v", existing.ID, err)
				}
			} else if allowFetch {
				if err := uc.enqueueFetchPrio(existing.ID, existing.Platform, existing.ExternalID, existing.URL, prio); err != nil {
					log.Errorf("re-enqueue fetch problem %d: %v", existing.ID, err)
				}
			}
		case model.ProblemStatusCompleted, model.ProblemStatusFailedPerm, model.ProblemStatusSkipped:
			// 已分析完成 / 永久失败 / 跳过：不入队
		}
	}
	// 回填缓存：同批后续同题提交复用（含新建题），避免重复建题/查询
	if cache != nil {
		cp := existing
		cache[cacheKey] = &cp
	}
	return &existing, isNew, nil
}

func (uc *ProblemUseCase) declareProblemQueue(name string) error {
	if uc.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	// 队列已存在：直接成功。重复 QueueDeclare 且 args 不一致会 PRECONDITION 杀 channel，
	// 导致后续 Publish 失败且消费者永远注册不上。
	if _, err := uc.mq.QueueInspect(name); err == nil {
		return nil
	}
	args := amqp.Table{"x-max-priority": mqMaxPriority}
	if _, err := uc.mq.QueueDeclare(name, true, false, false, false, args); err != nil {
		// 已存在且无 max-priority 时 PRECONDITION_FAILED：降级声明
		if _, err2 := uc.mq.QueueDeclare(name, true, false, false, false, nil); err2 != nil {
			return err
		}
	}
	return nil
}

func (uc *ProblemUseCase) enqueueFetch(id uint, platform, externalID, url string) error {
	return uc.enqueueFetchPrio(id, platform, externalID, url, mqPriorityBulk)
}

func (uc *ProblemUseCase) enqueueFetchPrio(id uint, platform, externalID, url string, priority uint8) error {
	if uc.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	// 牛客：附带比赛页候选；有比赛映射时 Force，以便 FAILED_PERM 也能再爬
	force := false
	var fallbacks []string
	primary := url
	if strings.EqualFold(strings.TrimSpace(platform), spider.NowCoder) {
		fallbacks = uc.nowcoderContestFetchURLs(externalID, id)
		if len(fallbacks) > 0 {
			force = true
			// 优先比赛页作主 URL
			if problem_fetch.IsNowCoderContestURL(fallbacks[0]) {
				primary = fallbacks[0]
				rest := fallbacks[1:]
				if bank := problem_fetch.NowCoderBankProblemURL(externalID); bank != "" {
					fallbacks = append([]string{bank}, rest...)
				} else {
					fallbacks = rest
				}
				if url != "" && url != primary && !problem_fetch.IsNowCoderContestURL(url) {
					fallbacks = append(fallbacks, url)
				}
			}
		}
	}
	body, _ := json.Marshal(event.ProblemFetchEvent{
		ProblemID:    id,
		Platform:     platform,
		ExternalID:   externalID,
		URL:          primary,
		FallbackURLs: fallbacks,
		Force:        force,
	})
	if err := uc.declareProblemQueue("problem_fetch"); err != nil {
		return err
	}
	return uc.mq.Publish("", "problem_fetch", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Priority:     priority,
	})
}

func (uc *ProblemUseCase) enqueueAnalyze(id uint) error {
	return uc.enqueueAnalyzePrio(id, mqPriorityBulk)
}

// enqueueAnalyzePrio 投递 AI 分析；统一闸门：
// 1) 近 6 个月有提交（submit_logs）
// 2) 近窗提交者中至少有一名「题面 AI 资格」用户（默认非公共域组织 + Pro 订阅，可个人覆盖）
// 3) AI 分析月度配额：至少一名近窗提交者配额可用（quota>0 且未满）；通过的提交者各计数一次
// 题面爬取由 shouldEnqueueFetch / problemHasFetchSubmitter 单独闸门。
func (uc *ProblemUseCase) enqueueAnalyzePrio(id uint, priority uint8) error {
	if uc.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, id).Error; err != nil {
		return err
	}
	// 自动爬虫 / 回填 / 爬取成功后入队：超过 6 个月（以 submit_logs 最近提交为准）不进 AI
	if !uc.withinAnalyzeWindow(&p) {
		log.Debugf("enqueueAnalyze skip out-of-window id=%d last=%v", id, p.LastSubmittedAt)
		return nil
	}
	// 无 AI 资格用户近窗提交：只保留题面，不跑 AI
	if !uc.problemHasAISubmitter(id) {
		log.Debugf("enqueueAnalyze skip no AI-eligible submitters id=%d", id)
		return nil
	}
	// AI 分析月度配额闸门：至少一名近窗提交者配额可用；名单不可用时保守放行（不计数）
	if submitters := uc.pipelineSubmitterIDs(id, "ai"); submitters != nil {
		if len(submitters) > 0 && !uc.chargeAiAnalyzeQuota(submitters) {
			log.Debugf("enqueueAnalyze skip ai quota exhausted id=%d", id)
			return nil
		}
	}
	body, _ := json.Marshal(event.ProblemAnalyzeEvent{ProblemID: id})
	if err := uc.declareProblemQueue("problem_analyze"); err != nil {
		return err
	}
	return uc.mq.Publish("", "problem_analyze", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Priority:     priority,
	})
}

// aiAnalyzeLoc AI 分析月度配额按上海自然月
var aiAnalyzeLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// aiAnalyzeMonthKey 单用户 AI 分析月计数 key
func aiAnalyzeMonthKey(uid int64, now time.Time) string {
	return fmt.Sprintf("sub:ai_analyze:%d:%s", uid, now.In(aiAnalyzeLoc).Format("200601"))
}

// aiQuotaAllows 当月计数未达上限且配额>0（纯函数，便于测试）
func aiQuotaAllows(used, quota int64) bool {
	return quota > 0 && used < quota
}

// aiAnalyzeMonthKeyTTL 计数 key 存活到次月 0 点（+1 分钟缓冲），过期自动清零
func aiAnalyzeMonthKeyTTL() time.Duration {
	now := time.Now().In(aiAnalyzeLoc)
	next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, aiAnalyzeLoc)
	return time.Until(next) + time.Minute
}

// chargeAiAnalyzeQuota 对近窗提交者逐个做 AI 分析月配额检查并计数：
// - 组织开通（unlimited=true）：直接通过，不计数（组织成员优先消耗组织无限配额）
// - quota==0（无配额，如无组织免费用户）：不能触发
// - quota>0 且当月计数 < quota：通过并 INCR（首次设置月末 TTL）
// 至少一名提交者通过 → true；RPC/Redis 故障保守放行（该提交者不计数）。
// 配额值由 user 服务按组织/套餐实时返回，core_data 不缓存。
func (uc *ProblemUseCase) chargeAiAnalyzeQuota(submitters []int64) bool {
	if len(submitters) == 0 {
		return false
	}
	ctx := context.Background()
	anyPass := false
	for _, uid := range submitters {
		if uc.reg != nil {
			if cli, err := userrpc.SubscriptionClient(uc.reg); err == nil && cli != nil {
				q, err := cli.GetAiAnalyzeQuota(ctx, &subscription.GetAiAnalyzeQuotaReq{UserId: uid})
				if err != nil || q == nil {
					log.Warnf("GetAiAnalyzeQuota uid=%d: %v", uid, err)
					anyPass = true // RPC 故障：保守放行该提交者（不计数）
					continue
				}
				if q.GetUnlimited() {
					anyPass = true // 组织开通：无限配额，不计数
					continue
				}
				quota := int(q.GetQuotaPerMonth())
				if quota <= 0 {
					continue // 无配额：不能触发
				}
				if uc.data == nil || uc.data.RDB == nil {
					anyPass = true
					continue
				}
				key := aiAnalyzeMonthKey(uid, time.Now())
				used, err := uc.data.RDB.Get(ctx, key).Int64()
				if err != nil && err != redis.Nil {
					log.Warnf("ai analyze get uid=%d: %v", uid, err)
					anyPass = true // Redis 故障：保守放行
					continue
				}
				if !aiQuotaAllows(used, int64(quota)) {
					continue // 配额已满：不计数
				}
				if _, err := uc.data.RDB.Incr(ctx, key).Result(); err != nil {
					log.Warnf("ai analyze incr uid=%d: %v", uid, err)
					anyPass = true
					continue
				}
				if used == 0 {
					_ = uc.data.RDB.Expire(ctx, key, aiAnalyzeMonthKeyTTL()).Err()
				}
				anyPass = true
			}
		}
	}
	return anyPass
}

// nowcoderContestFetchURLs 从 contest_problems 解析比赛页回退链接。
// 例：external_id=319811 → https://ac.nowcoder.com/acm/contest/137561/A
// 刚结束比赛时题库页无权限，比赛页 window.pageInfo.problemId 仍与题库 id 一致。
func (uc *ProblemUseCase) nowcoderContestFetchURLs(externalID string, problemID uint) []string {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return nil
	}
	externalID = strings.TrimSpace(externalID)
	var rows []model.ContestProblem
	q := uc.data.DB.Where("platform = ?", spider.NowCoder)
	if externalID != "" {
		q = q.Where("external_id = ?", externalID)
	} else if problemID > 0 {
		q = q.Where("problem_id = ?", problemID)
	} else {
		return nil
	}
	_ = q.Order("updated_at DESC").Limit(8).Find(&rows).Error
	seen := map[string]struct{}{}
	var out []string
	for _, r := range rows {
		u := strings.TrimSpace(r.URL)
		if problem_fetch.IsNowCoderContestURL(u) {
			if _, ok := seen[u]; !ok {
				seen[u] = struct{}{}
				out = append(out, u)
			}
		}
		if cu := problem_fetch.NowCoderContestProblemURL(r.ContestID, r.Label); cu != "" {
			if _, ok := seen[cu]; !ok {
				seen[cu] = struct{}{}
				out = append(out, cu)
			}
		}
	}
	return out
}

// ProcessFetch 仅爬取题面；成功后状态 TAGGING 并投递 AI 队列。
// Force=true 时忽略用户爬取资格；BypassFetchPause=true 时忽略全局暂停；SkipAnalyze=true 时爬取成功后不入 AI。
func (uc *ProblemUseCase) ProcessFetch(ctx context.Context, ev event.ProblemFetchEvent) error {
	if pipelineControl.IsFetchPaused() && !ev.BypassFetchPause {
		return errProblemFetchPaused
	}
	runtime := sitesettings.Load(ctx, uc.data.RDB, nil)
	ojhttp.SetProxyConfig(ojhttp.ProxyConfig{
		BaseURL: runtime.OjProxyBaseURL,
		Secret:  runtime.OjProxySecret,
		Enabled: func(host string) bool { return task.IsProxyEnabled(uc.data.RDB, platformForHost(host)) },
	})
	var p model.Problem
	if err := uc.data.DB.First(&p, ev.ProblemID).Error; err != nil {
		return err
	}
	pipelineControl.TrackStart("fetch", p.ID, p.Platform, p.ExternalID, p.Title)
	defer pipelineControl.TrackEnd("fetch", p.ID)
	// 已识别完成且有题面：跳过。无题面的 COMPLETED/TAGGING 必须允许补爬（全平台）。
	hasContent := strings.TrimSpace(p.ContentMD) != ""
	if p.Status == model.ProblemStatusCompleted && hasContent && !ev.ForceRefetch {
		return nil
	}
	// 无爬取资格用户近窗提交：不爬题面（旧消息防御；前端显示「题面准备中」）
	// Force：题单加题等主动场景可忽略资格
	if !ev.Force && !uc.shouldEnqueueFetch(p.ID) {
		log.Infof("ProcessFetch skip no fetch-eligible submitters id=%d", p.ID)
		if !hasContent && p.Status != model.ProblemStatusSkipped {
			_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
				"status":    model.ProblemStatusPending,
				"error_msg": "无题面爬取资格用户提交，暂不爬取题面",
			}).Error
		}
		return nil
	}
	// 已有题面：不再爬取；入 AI（主动路径按 actor，否则窗口 + submitter 闸门）
	// 注意：不得因 status=TAGGING 且 content 空而跳过爬取
	if hasContent && !ev.ForceRefetch {
		if p.Status != model.ProblemStatusCompleted {
			_ = uc.data.DB.Model(&p).Update("status", model.ProblemStatusTagging).Error
			if !ev.SkipAnalyze {
				if ev.ActorUserID > 0 {
					return uc.enqueueAnalyzeForUser(p.ID, ev.ActorUserID)
				}
				if !pipelineControl.IsAnalyzePaused() {
					return uc.enqueueAnalyze(p.ID)
				}
			}
		}
		return nil
	}

	// 先解析牛客比赛页候选：后补 contest_problems 时，即使曾 FAILED_PERM 也可再爬
	var fallbacks []string
	if strings.EqualFold(strings.TrimSpace(p.Platform), spider.NowCoder) {
		fallbacks = uc.nowcoderContestFetchURLs(p.ExternalID, p.ID)
		if len(ev.FallbackURLs) > 0 {
			fallbacks = append(ev.FallbackURLs, fallbacks...)
		}
	}
	hasContestPath := false
	for _, u := range append([]string{ev.URL}, fallbacks...) {
		if problem_fetch.IsNowCoderContestURL(u) {
			hasContestPath = true
			break
		}
	}
	// Force / 已有比赛路径：允许从永久失败恢复（用户先提交后失败、后有比赛记录）
	allowRetryPerm := ev.Force || hasContestPath
	if p.Status == model.ProblemStatusFailedPerm && !allowRetryPerm {
		return nil
	}
	if p.Status == model.ProblemStatusFailed && isPermanentFetchError(p.ErrorMsg) && !allowRetryPerm {
		_ = uc.data.DB.Model(&p).Update("status", model.ProblemStatusFailedPerm).Error
		return nil
	}
	if p.Status == model.ProblemStatusSkipped {
		return nil
	}
	if allowRetryPerm && (p.Status == model.ProblemStatusFailedPerm || p.Status == model.ProblemStatusFailed) {
		_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
			"status":           model.ProblemStatusPending,
			"error_msg":        "",
			"fetch_attempts":   0,
			"fetch_fail_since": nil,
		}).Error
		p.Status = model.ProblemStatusPending
	}

	// 无题面时允许从 TAGGING / COMPLETED 进入 FETCHING（补爬）
	res := uc.data.DB.Model(&model.Problem{}).
		Where("id = ? AND status IN ?", p.ID, []string{
			model.ProblemStatusPending,
			model.ProblemStatusFailed,
			model.ProblemStatusFetching,
			model.ProblemStatusFailedPerm, // 恢复竞态
			model.ProblemStatusTagging,    // 空题面误标
			model.ProblemStatusCompleted,  // 有标签无题面
		}).
		Update("status", model.ProblemStatusFetching)
	if res.Error != nil {
		return res.Error
	}
	// 已被别人标成永久失败 / 并发跳过
	if res.RowsAffected == 0 {
		return nil
	}
	url := p.URL
	if url == "" {
		url = ev.URL
	}
	// 有比赛页时主 URL 用比赛路径（赛后题库常无权限）
	if hasContestPath {
		if problem_fetch.IsNowCoderContestURL(ev.URL) {
			url = ev.URL
		} else {
			for _, u := range fallbacks {
				if problem_fetch.IsNowCoderContestURL(u) {
					url = u
					break
				}
			}
		}
	}
	if err := uc.restorePausedProblemFetch(&p, ev.BypassFetchPause); err != nil {
		return err
	}
	// Source switches can change while a task is being claimed. Recheck after
	// the FETCHING transition immediately before any external request.
	var fetched *problem_fetch.FetchedContent
	var err error
	if p.Platform == spider.QOJ {
		// QOJ problem pages can require the configured crawler session, while
		// the generic fetcher intentionally uses an unauthenticated client.
		if provider, ok := spider.Get(spider.QOJ); ok {
			if qoj, ok := provider.(interface {
				FetchProblemHTML(context.Context, string) (string, int, error)
			}); ok {
				var body string
				var status int
				body, status, err = qoj.FetchProblemHTML(context.Background(), url)
				if err == nil && status != http.StatusOK {
					err = fmt.Errorf("QOJ status %d", status)
				}
				if err == nil {
					fetched, err = problem_fetch.ParseQOJHTML(body, p.ExternalID)
				}
			}
		}
	}
	if fetched == nil && err == nil {
		fetched, err = problem_fetch.FetchWithFallbacks(p.Platform, p.ExternalID, url, fallbacks)
	}
	if err != nil {
		return uc.handleFetchError(&p, err)
	}

	title := p.Title
	if fetched.Title != "" {
		// 拒绝用站点品牌名（QOJ.ac）覆盖已有/更好标题
		if !isBadFetchedTitle(p.Platform, fetched.Title) {
			title = fetched.Title
		} else if isBadFetchedTitle(p.Platform, title) {
			// 旧标题也是品牌垃圾时，至少落到题号
			if p.ExternalID != "" {
				title = "#" + p.ExternalID
			}
		}
	}
	// 已有标签/AI 解法：只补题面，不重跑分析、不碰 tags/solutions_meta
	hasAnalysis := len(nonEmptyTags(p.Tags)) > 0 || len(p.SolutionsMeta) > 0
	nextStatus := model.ProblemStatusTagging
	if hasAnalysis {
		nextStatus = model.ProblemStatusCompleted
	}
	updates := map[string]interface{}{
		"content_md":                  fetched.ContentMD,
		"content_source":              fetched.Source,
		"content_source_url":          fetched.SourceURL,
		"content_source_problem_id":   fetched.SourceProblemID,
		"content_source_statement_id": fetched.SourceStatementID,
		"content_language":            fetched.Language,
		"content_fetched_at":          time.Now(),
		"title":                       title,
		"error_msg":                   "",
		"status":                      nextStatus,
		"fetch_attempts":              0,
		"fetch_fail_since":            nil,
	}
	// 规范 URL：牛客数字题号始终写题库形态（即使从比赛页抓到）
	if bank := problem_fetch.NowCoderBankProblemURL(p.ExternalID); bank != "" &&
		strings.EqualFold(strings.TrimSpace(p.Platform), spider.NowCoder) {
		updates["url"] = bank
	} else if p.URL == "" && url != "" && !problem_fetch.IsNowCoderContestURL(url) {
		updates["url"] = url
	}
	if err := uc.data.DB.Model(&p).Updates(updates).Error; err != nil {
		return err
	}
	uc.BumpProblemDetailVer(p.ID)
	uc.progressMoveStatus(p.Status, nextStatus)
	// 爬取成功后入 AI：
	// - 已有标签/分析：绝不重跑 AI（保留既有 tags / solutions_meta）
	// - SkipAnalyze：仅爬不分析
	// - ActorUserID>0：用户主动场景，按操作者 AI 资格（绕过 submitter/6 月窗）
	// - 否则：走 enqueueAnalyzePrio（近 6 月 + AI 资格提交者）
	if hasAnalysis {
		log.Infof("ProcessFetch keep existing tags/solutions id=%d", p.ID)
		return nil
	}
	if ev.SkipAnalyze {
		log.Infof("ProcessFetch skip analyze (fetch-only) id=%d", p.ID)
		return nil
	}
	if ev.ActorUserID > 0 {
		return uc.enqueueAnalyzeForUser(p.ID, ev.ActorUserID)
	}
	// 分析暂停时仍入队（暂停不清队列，恢复后继续）；高优先级延续当前已出队的爬取任务
	return uc.enqueueAnalyzePrio(p.ID, mqPriorityIncremental)
}

// restorePausedProblemFetch closes the pause race after FETCHING is claimed.
// Only the still-empty FETCHING row may be restored; a concurrent success must win.
func (uc *ProblemUseCase) restorePausedProblemFetch(p *model.Problem, bypassGlobalPause bool) error {
	globalPaused := pipelineControl.IsFetchPaused()
	if bypassGlobalPause {
		globalPaused = false
	}
	if !globalPaused {
		return nil
	}
	if strings.TrimSpace(p.ContentMD) == "" {
		res := uc.data.DB.Model(&model.Problem{}).
			Where("id = ? AND status = ? AND TRIM(COALESCE(content_md, '')) = ''", p.ID, model.ProblemStatusFetching).
			Update("status", model.ProblemStatusPending)
		if res.Error != nil {
			return fmt.Errorf("%w: %s (restore status: %v)", errProblemFetchPaused, p.Platform, res.Error)
		}
		if res.RowsAffected > 0 {
			p.Status = model.ProblemStatusPending
		}
	}
	if globalPaused {
		return errProblemFetchPaused
	}
	return errProblemFetchPaused
}

// ForceEnqueueFetchOnly 强制入队题面爬取，忽略用户资格，且不触发 AI 分析。
// 兼容旧调用；新代码请用 ForceEnqueueFetch(problemID, actorUID)。
func (uc *ProblemUseCase) ForceEnqueueFetchOnly(problemID uint) error {
	return uc.ForceEnqueueFetch(problemID, 0)
}

func (uc *ProblemUseCase) ForceEnqueueRefetch(problemID uint, actorUID uint) error {
	if uc == nil || problemID == 0 {
		return nil
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return err
	}
	uc.scheduleUserPriorityFetchWithMode(p.ID, p.Platform, p.ExternalID, p.URL, true, actorUID, true)
	return nil
}

// ContentLooksBroken 历史坏题面（HTML→MD 粘连章节标题等），用户主动加题时应强制重爬。
// 全平台通用启发式：章节名与正文粘连、页头 Editorial 残留。
func ContentLooksBroken(md string) bool {
	return contentLooksBroken(md)
}

func contentLooksBroken(md string) bool {
	s := strings.TrimSpace(md)
	if s == "" {
		return false
	}
	// AtCoder 旧解析：h3 未换行 → "Problem StatementYou are..."
	glued := []string{
		"Problem StatementYou", "Problem StatementThere", "Problem StatementGiven",
		"Constraints1", "Constraints0", "ConstraintsN",
		"InputThe", "InputFrom", "OutputIf", "OutputPrint",
		"Sample Input 1\n", // keep normal
	}
	for _, g := range glued {
		if g == "Sample Input 1\n" {
			continue
		}
		if strings.Contains(s, g) {
			return true
		}
	}
	if strings.Contains(s, "\tEditorial") || strings.Contains(s, "\n\t\t\tEditorial") {
		return true
	}
	// 有 "Problem Statement" 却无 markdown 标题，且紧贴大写正文
	if strings.Contains(s, "Problem Statement") && !strings.Contains(s, "### Problem Statement") &&
		!strings.Contains(s, "## Problem Statement") {
		if idx := strings.Index(s, "Problem Statement"); idx >= 0 {
			rest := s[idx+len("Problem Statement"):]
			rest = strings.TrimLeft(rest, " \t")
			if rest != "" && rest[0] >= 'A' && rest[0] <= 'Z' {
				return true
			}
		}
	}
	return false
}

// ForceEnqueueFetch 强制题面爬取（忽略爬取资格与全局暂停；平台级暂停仍有效）。
// 用户主动路径：HTTP 不阻塞 MQ；最高优先级异步入队 + 后台直爬兜底。
// 无论 status（含 TAGGING/COMPLETED），只要 contentMd 空都允许补爬。
// 题面存在但明显损坏时：清空后重爬。
// ContentMD 正常时：若可分析则异步 enqueueAnalyzeForUser，否则 no-op。
func (uc *ProblemUseCase) ForceEnqueueFetch(problemID uint, actorUID uint) error {
	if uc == nil || problemID == 0 {
		return nil
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return err
	}
	hasContent := strings.TrimSpace(p.ContentMD) != ""
	// 损坏题面：清空后走补爬（用户主动路径）
	if hasContent && contentLooksBroken(p.ContentMD) {
		log.Infof("ForceEnqueueFetch broken content id=%d platform=%s, re-fetch", p.ID, p.Platform)
		_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
			"content_md":       "",
			"status":           model.ProblemStatusPending,
			"error_msg":        "",
			"fetch_attempts":   0,
			"fetch_fail_since": nil,
		}).Error
		p.ContentMD = ""
		p.Status = model.ProblemStatusPending
		hasContent = false
	}
	// 已有正常题面：尝试按操作者 AI 资格分析（异步，不堵 HTTP）
	if hasContent {
		if actorUID > 0 && len(nonEmptyTags(p.Tags)) == 0 {
			go func(id, uid uint) {
				defer func() {
					if rec := recover(); rec != nil {
						log.Errorf("enqueueAnalyzeForUser panic id=%d: %v", id, rec)
					}
				}()
				if err := uc.enqueueAnalyzeForUser(id, uid); err != nil {
					log.Warnf("enqueueAnalyzeForUser id=%d: %v", id, err)
				}
			}(problemID, actorUID)
		}
		return nil
	}
	if p.Status == model.ProblemStatusSkipped {
		return nil
	}
	// 用户/管理员主动：重置永久失败与误标 COMPLETED/TAGGING（空题面）
	if p.Status == model.ProblemStatusFailedPerm ||
		p.Status == model.ProblemStatusCompleted ||
		p.Status == model.ProblemStatusTagging ||
		(p.Status == model.ProblemStatusFailed && isPermanentFetchError(p.ErrorMsg)) {
		_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
			"status":           model.ProblemStatusPending,
			"error_msg":        "",
			"fetch_attempts":   0,
			"fetch_fail_since": nil,
		}).Error
		p.Status = model.ProblemStatusPending
	}
	skipAnalyze := actorUID == 0 || !uc.userHasAIEligibility(actorUID)
	// 用户主动：最高优先级；后台直爬 + MQ 双通道，HTTP 立即返回
	uc.scheduleUserPriorityFetch(p.ID, p.Platform, p.ExternalID, p.URL, skipAnalyze, actorUID)
	return nil
}

// scheduleUserPriorityFetch 用户主动补爬：MQ 最高优先级异步入队 + 后台直爬。
// 全平台通用；HTTP 路径禁止同步等 confirm。
func (uc *ProblemUseCase) scheduleUserPriorityFetch(id uint, platform, externalID, url string, skipAnalyze bool, actorUID uint) {
	uc.scheduleUserPriorityFetchWithMode(id, platform, externalID, url, skipAnalyze, actorUID, false)
}

func (uc *ProblemUseCase) scheduleUserPriorityFetchWithMode(id uint, platform, externalID, url string, skipAnalyze bool, actorUID uint, forceRefetch bool) {
	ev := event.ProblemFetchEvent{
		ProblemID:        id,
		Platform:         platform,
		ExternalID:       externalID,
		URL:              url,
		Force:            true,
		BypassFetchPause: true,
		SkipAnalyze:      skipAnalyze,
		ActorUserID:      actorUID,
		ForceRefetch:     forceRefetch,
	}
	// 牛客比赛页候选（与 enqueueFetchPrio 对齐）
	if strings.EqualFold(strings.TrimSpace(platform), spider.NowCoder) {
		if fb := uc.nowcoderContestFetchURLs(externalID, id); len(fb) > 0 {
			ev.FallbackURLs = fb
			if problem_fetch.IsNowCoderContestURL(fb[0]) {
				ev.URL = fb[0]
			}
		}
	}
	go func(ev event.ProblemFetchEvent) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Errorf("scheduleUserPriorityFetch panic id=%d: %v", ev.ProblemID, rec)
			}
		}()
		// 1) MQ 最高优先级（异步 confirm，失败仅日志）
		if err := uc.enqueueFetchForcedPrio(ev, mqPriorityUser); err != nil {
			log.Warnf("user fetch MQ id=%d: %v", ev.ProblemID, err)
		}
		// 2) 直爬兜底：不依赖消费者；与 MQ 并发时靠 status CAS 去重
		if err := uc.ProcessFetch(context.Background(), ev); err != nil {
			log.Warnf("user fetch direct id=%d: %v", ev.ProblemID, err)
		}
	}(ev)
}

func (uc *ProblemUseCase) enqueueFetchForced(id uint, platform, externalID, url string, skipAnalyze bool, actorUID uint) error {
	return uc.enqueueFetchForcedPrio(event.ProblemFetchEvent{
		ProblemID:        id,
		Platform:         platform,
		ExternalID:       externalID,
		URL:              url,
		Force:            true,
		BypassFetchPause: true,
		SkipAnalyze:      skipAnalyze,
		ActorUserID:      actorUID,
	}, mqPriorityUser)
}

func (uc *ProblemUseCase) enqueueFetchForcedPrio(ev event.ProblemFetchEvent, priority uint8) error {
	if uc.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	body, _ := json.Marshal(ev)
	if err := uc.declareProblemQueue("problem_fetch"); err != nil {
		return err
	}
	// 用户路径一律 async；其它调用方（admin）也避免堵 worker
	uc.mq.PublishAsync("", "problem_fetch", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Priority:     priority,
	})
	return nil
}

// enqueueAnalyzeForUser 用户主动场景入 AI：仅校验操作者 AI 资格，不校验 submitter/6 月窗。
// 标签非空或题面为空时跳过。
func (uc *ProblemUseCase) enqueueAnalyzeForUser(problemID uint, actorUID uint) error {
	if uc == nil || problemID == 0 {
		return nil
	}
	if actorUID == 0 || !uc.userHasAIEligibility(actorUID) {
		log.Debugf("enqueueAnalyzeForUser skip no AI eligibility actor=%d id=%d", actorUID, problemID)
		return nil
	}
	if uc.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return err
	}
	if len(nonEmptyTags(p.Tags)) > 0 {
		if strings.TrimSpace(p.ContentMD) != "" && p.Status != model.ProblemStatusCompleted {
			_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
				"status":    model.ProblemStatusCompleted,
				"error_msg": "",
			}).Error
		}
		return nil
	}
	if strings.TrimSpace(p.ContentMD) == "" {
		return nil
	}
	if p.Status == model.ProblemStatusCompleted || p.Status == model.ProblemStatusSkipped {
		return nil
	}
	if pipelineControl.IsAnalyzePaused() {
		// 暂停时仍入队，恢复后继续
		log.Debugf("enqueueAnalyzeForUser analyze paused, still enqueue id=%d", problemID)
	}
	_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
		"status":    model.ProblemStatusTagging,
		"error_msg": "",
	}).Error
	body, _ := json.Marshal(event.ProblemAnalyzeEvent{ProblemID: problemID, Force: true})
	if err := uc.declareProblemQueue("problem_analyze"); err != nil {
		return err
	}
	uc.mq.PublishAsync("", "problem_analyze", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Priority:     mqPriorityUser,
	})
	return nil
}

func (uc *ProblemUseCase) PrepareUserReanalyze(problemID, actorUID uint) (int, error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil || uc.mq == nil {
		return 0, fmt.Errorf("分析服务不可用")
	}
	if actorUID == 0 || !uc.userHasAIEligibility(actorUID) {
		return 0, fmt.Errorf("暂无题目分析权限")
	}
	if uc.data == nil || uc.data.RDB == nil {
		return 0, fmt.Errorf("AI 配额服务不可用")
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return 0, err
	}
	if strings.TrimSpace(p.ContentMD) == "" {
		return 0, fmt.Errorf("题面尚未准备好")
	}
	if err := uc.declareProblemQueue("problem_analyze"); err != nil {
		return 0, err
	}
	quotaClient, err := userrpc.SubscriptionClient(uc.reg)
	if err != nil {
		return 0, fmt.Errorf("AI 配额服务不可用")
	}
	q, err := quotaClient.GetAiAnalyzeQuota(context.Background(), &subscription.GetAiAnalyzeQuotaReq{UserId: int64(actorUID)})
	if err != nil || q == nil {
		return 0, fmt.Errorf("AI 配额读取失败")
	}
	monthKey := aiAnalyzeMonthKey(int64(actorUID), time.Now())
	used, _ := uc.data.RDB.Get(context.Background(), monthKey).Int64()
	if !q.GetUnlimited() && !aiQuotaAllows(used, int64(q.GetQuotaPerMonth())) {
		return 0, fmt.Errorf("本月 AI 分析次数已用完")
	}
	if !q.GetUnlimited() {
		if used, err = uc.data.RDB.Incr(context.Background(), monthKey).Result(); err != nil {
			return 0, fmt.Errorf("AI 配额扣减失败")
		}
		if used == 1 {
			_ = uc.data.RDB.Expire(context.Background(), monthKey, aiAnalyzeMonthKeyTTL()).Err()
		}
	}
	previousStatus, previousError := p.Status, p.ErrorMsg
	if err := uc.data.DB.Model(&p).Updates(map[string]interface{}{"status": model.ProblemStatusTagging, "error_msg": ""}).Error; err != nil {
		return 0, err
	}
	body, _ := json.Marshal(event.ProblemAnalyzeEvent{ProblemID: problemID, Force: true})
	if err := uc.mq.Publish("", "problem_analyze", false, false, amqp.Publishing{
		ContentType: "application/json", Body: body, DeliveryMode: amqp.Persistent, Priority: mqPriorityUser,
	}); err != nil {
		// Do not report a successful re-analysis when RabbitMQ did not confirm
		// the message. Restore the previous state so the problem can be retried.
		_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
			"status": previousStatus, "error_msg": previousError,
		}).Error
		return 0, fmt.Errorf("重新分析入队失败: %w", err)
	}
	return maxInt(0, int(q.GetQuotaPerMonth())-int(used)), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// CreateManualProblem 用户自主加题（无需审核）。platform=Manual。
// 有题面无标签且 actor 有 AI 资格时入分析队列。
func (uc *ProblemUseCase) CreateManualProblem(actorUID uint, title, contentMD, sourceURL string, tags []string) (*model.Problem, error) {
	if uc == nil {
		return nil, fmt.Errorf("usecase nil")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("请填写题目标题")
	}
	if utf8.RuneCountInString(title) > 200 {
		return nil, fmt.Errorf("标题过长")
	}
	contentMD = strings.TrimSpace(contentMD)
	if len(contentMD) > 200_000 {
		return nil, fmt.Errorf("题面过长")
	}
	tags = normalizeEditTags(tags)
	sourceURL = strings.TrimSpace(sourceURL)
	if len(sourceURL) > 1024 {
		sourceURL = sourceURL[:1024]
	}
	extID := "m_" + strings.ReplaceAll(uuidNew(), "-", "")
	hasContent := contentMD != ""
	hasTags := len(tags) > 0
	status := model.ProblemStatusCompleted
	if hasContent && !hasTags {
		status = model.ProblemStatusTagging
	}
	p := model.Problem{
		Platform:   "Manual",
		ExternalID: extID,
		Title:      title,
		URL:        sourceURL,
		ContentMD:  contentMD,
		Tags:       model.StringArray(tags),
		Status:     status,
	}
	if err := uc.data.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		_, _, err := dal.SyncProblemTags(context.Background(), tx, p.ID, tags)
		return err
	}); err != nil {
		return nil, err
	}
	if hasContent && !hasTags && actorUID > 0 {
		if err := uc.enqueueAnalyzeForUser(p.ID, actorUID); err != nil {
			log.Warnf("CreateManualProblem enqueue analyze id=%d: %v", p.ID, err)
		}
	}
	return &p, nil
}

// uuidNew 生成无连字符前的标准 UUID 字符串（失败时用随机 hex）
func uuidNew() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	// RFC 4122 version 4
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UpsertProblemFromParsed 按 platform+external_id 幂等入库；缺题面时强制爬取（默认不 AI，兼容旧调用）
func (uc *ProblemUseCase) UpsertProblemFromParsed(parsed *ParsedProblem) (*model.Problem, error) {
	return uc.UpsertProblemFromParsedForUser(parsed, 0)
}

// UpsertProblemFromParsedForUser 同 UpsertProblemFromParsed，actorUID 用于条件 AI
func (uc *ProblemUseCase) UpsertProblemFromParsedForUser(parsed *ParsedProblem, actorUID uint) (*model.Problem, error) {
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
			// 并发冲突再查
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
		if shouldReplaceProblemTitle(existing.Platform, existing.Title, parsed.Title) {
			_ = uc.data.DB.Model(&existing).Update("title", parsed.Title).Error
			existing.Title = parsed.Title
		}
		if existing.URL == "" && parsed.URL != "" {
			_ = uc.data.DB.Model(&existing).Update("url", parsed.URL).Error
			existing.URL = parsed.URL
		}
	}
	// 牛客比赛 URL 加题：先写 contest_problems，再 ForceEnqueueFetch
	// （scheduleUserPriorityFetch 会从映射取比赛页作主 URL）
	uc.ensureContestProblemMapping(parsed, existing.ID)
	if !parsed.SkipFetch {
		if err := uc.ForceEnqueueFetch(existing.ID, actorUID); err != nil {
			log.Warnf("ForceEnqueueFetch id=%d: %v", existing.ID, err)
		}
	}
	return &existing, nil
}

// ensureContestProblemMapping 将解析结果中的比赛映射写入 contest_problems（幂等）。
// 供加题路径：粘贴 /acm/contest/{id}/{F} 后，nowcoderContestFetchURLs 能命中回退页。
func (uc *ProblemUseCase) ensureContestProblemMapping(parsed *ParsedProblem, problemID uint) {
	if uc == nil || uc.data == nil || uc.data.DB == nil || parsed == nil || problemID == 0 {
		return
	}
	cid := strings.TrimSpace(parsed.ContestID)
	label := strings.TrimSpace(parsed.ContestLabel)
	if cid == "" || label == "" || !strings.EqualFold(parsed.Platform, spider.NowCoder) {
		return
	}
	contestURL := ""
	if len(parsed.FallbackURLs) > 0 {
		contestURL = strings.TrimSpace(parsed.FallbackURLs[0])
	}
	if contestURL == "" {
		contestURL = problem_fetch.NowCoderContestProblemURL(cid, label)
	}
	item := model.ContestProblem{
		Platform:   spider.NowCoder,
		ContestID:  cid,
		Label:      label,
		SortOrder:  0,
		ExternalID: parsed.ExternalID,
		Title:      firstNonEmpty(parsed.Title, label),
		URL:        contestURL,
		ProblemID:  problemID,
	}
	_ = uc.data.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "platform"}, {Name: "contest_id"}, {Name: "label"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"external_id", "title", "url", "problem_id", "updated_at",
		}),
	}).Create(&item).Error
}

// ProcessAnalyze 仅 AI 打标（不爬取、不送用户代码）
// 6 个月窗口：以 submit_logs 中该题最近一次提交时间为准（并回写 last_submitted_at）。
func (uc *ProblemUseCase) ProcessAnalyze(ctx context.Context, ev event.ProblemAnalyzeEvent) error {
	if pipelineControl.IsAnalyzePaused() {
		return fmt.Errorf("ai analyze paused")
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, ev.ProblemID).Error; err != nil {
		return err
	}
	dirtyTags, dirtyDifficulty := problemFactsDirtyFlags(p.ErrorMsg)
	pending, pendingErr := loadAbilityMaintenancePending(ctx, uc.data.DB, problemMaintenanceScope(p.ID))
	if pendingErr != nil {
		return pendingErr
	}
	if pending != nil {
		if err := uc.recoverProblemMaintenance(ctx, pending); err != nil {
			return err
		}
		uc.BumpProblemDetailVer(p.ID)
		return nil
	}
	if dirtyTags || dirtyDifficulty {
		updates := map[string]interface{}{"status": model.ProblemStatusCompleted, "error_msg": ""}
		if err := uc.applyProblemFactUpdates(ctx, &p, updates, []string(p.Tags), dirtyTags, dirtyDifficulty); err != nil {
			return err
		}
		uc.BumpProblemDetailVer(p.ID)
		uc.progressMoveStatus(p.Status, model.ProblemStatusCompleted)
		return nil
	}
	pipelineControl.TrackStart("analyze", p.ID, p.Platform, p.ExternalID, p.Title)
	defer pipelineControl.TrackEnd("analyze", p.ID)
	// 已识别完成：跳过
	if p.Status == model.ProblemStatusCompleted && !ev.Force {
		log.Debugf("ProcessAnalyze skip completed id=%d", p.ID)
		return nil
	}
	if p.Status == model.ProblemStatusSkipped && !ev.Force {
		log.Debugf("ProcessAnalyze skip skipped id=%d", p.ID)
		return nil
	}
	if p.Status == model.ProblemStatusFailedPerm && !ev.Force {
		log.Debugf("ProcessAnalyze skip failed_perm id=%d", p.ID)
		return nil
	}
	// 标签已有（人工填写或历史分析）：跳过 AI，避免覆盖；标签为空仍继续分析
	if len(nonEmptyTags(p.Tags)) > 0 && !ev.Force {
		if strings.TrimSpace(p.ContentMD) != "" {
			log.Infof("ProcessAnalyze skip tags-already-set id=%d tags=%v", p.ID, p.Tags)
			_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
				"status":    model.ProblemStatusCompleted,
				"error_msg": "",
			}).Error
			return nil
		}
		// 有标签无题面：不跑 AI，等题面
		log.Debugf("ProcessAnalyze skip tags-set-no-content id=%d", p.ID)
		return nil
	}
	if strings.TrimSpace(p.ContentMD) == "" {
		// 永久错误不重爬
		if isPermanentFetchError(p.ErrorMsg) {
			_ = uc.data.DB.Model(&p).Update("status", model.ProblemStatusFailedPerm).Error
			return nil
		}
		// 无题面，退回爬取
		_ = uc.data.DB.Model(&p).Update("status", model.ProblemStatusPending).Error
		return uc.enqueueFetch(p.ID, p.Platform, p.ExternalID, p.URL)
	}

	// 用户主动（Force）：已在入队侧校验操作者 AI 资格，跳过 6 月窗与 submitter 检查
	if !ev.Force {
		// 近 6 个月：以 submit_logs 最近提交为准（与看板「待分析近6月」同一口径）
		if !uc.withinAnalyzeWindow(&p) {
			// 超窗：不再占「待分析」名额；静默 Ack 会让人以为在跑 AI
			log.Warnf("ProcessAnalyze out-of-window id=%d last=%v → SKIPPED_ANALYZE", p.ID, p.LastSubmittedAt)
			_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
				"status":    model.ProblemStatusTagging, // 仍保留有题面待分析语义
				"error_msg": "超出6个月分析窗口(以submit_logs最近提交为准)，已跳过",
			}).Error
			// 返回 error 会 requeue；此处 Ack 丢弃，靠重置/新提交再入队
			return nil
		}
		// 无 AI 资格用户近窗提交：不跑题面 AI（题面已爬取可保留）
		if !uc.problemHasAISubmitter(p.ID) {
			log.Infof("ProcessAnalyze skip no AI-eligible submitters id=%d", p.ID)
			_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
				"status":    model.ProblemStatusTagging,
				"error_msg": "无题面AI资格用户提交，已跳过题面AI",
			}).Error
			return nil
		}
	}

	if uc.tagger == nil || !uc.tagger.Ready() {
		return uc.data.DB.Model(&p).Updates(map[string]interface{}{
			"status":    model.ProblemStatusFailed,
			"error_msg": "ai_analyze 未配置",
		}).Error
	}

	_ = uc.data.DB.Model(&p).Update("status", model.ProblemStatusTagging).Error
	log.Infof("ProcessAnalyze start id=%d platform=%s ext=%s last=%v", p.ID, p.Platform, p.ExternalID, p.LastSubmittedAt)

	var result *aiAnalyzeResult
	var aerr error
	if progressTagger, ok := any(uc.tagger).(interface {
		AnalyzeWithProgress(context.Context, string, string, func(string)) (*aiAnalyzeResult, error)
	}); ok {
		var output strings.Builder
		result, aerr = progressTagger.AnalyzeWithProgress(ctx, p.Title, p.ContentMD, func(chunk string) {
			output.WriteString(chunk)
			pipelineControl.TrackOutput("analyze", p.ID, output.String())
		})
	} else {
		result, aerr = uc.tagger.Analyze(ctx, p.Title, p.ContentMD)
	}
	if aerr != nil {
		log.Errorf("AI tag problem %d: %v", p.ID, aerr)
		_ = uc.data.DB.Model(&p).Updates(map[string]interface{}{
			"status":    model.ProblemStatusFailed,
			"error_msg": "AI: " + truncateErr(aerr.Error()),
		}).Error
		return aerr
	}
	oldStatus := p.Status
	updates := map[string]interface{}{
		"problem_type":   result.ProblemType,
		"difficulty":     result.Difficulty,
		"tags":           model.StringArray(result.AlgorithmTags),
		"solutions_meta": model.SolutionsMeta(result.SuggestedSolutions),
		"status":         model.ProblemStatusCompleted,
		"error_msg":      "",
		"analyzed_at":    time.Now(),
		"analyzed_model": uc.tagger.ModelName(),
	}
	// AI 顺手优化排版后的题面
	if strings.TrimSpace(result.ContentMD) != "" {
		updates["content_md"] = result.ContentMD
	}
	// QOJ 等：爬取误把站点品牌当标题时，用 AI 整理后的一级标题回填
	if isBadFetchedTitle(p.Platform, p.Title) {
		md := result.ContentMD
		if strings.TrimSpace(md) == "" {
			md = p.ContentMD
		}
		if t := titleFromMarkdownH1(md); t != "" {
			updates["title"] = t
		} else if p.ExternalID != "" {
			updates["title"] = "#" + p.ExternalID
		}
	}
	tagsChanged := !sameNormalizedTags([]string(p.Tags), result.AlgorithmTags)
	difficultyChanged := strings.TrimSpace(p.Difficulty) != strings.TrimSpace(result.Difficulty)
	if err := uc.applyProblemFactUpdates(ctx, &p, updates, result.AlgorithmTags, tagsChanged, difficultyChanged); err != nil {
		return err
	}
	uc.BumpProblemDetailVer(p.ID)
	uc.progressMoveStatus(oldStatus, model.ProblemStatusCompleted)
	return nil
}

// backfillWindow 历史回填 / AI 分析仅处理最近 N 个月有提交的题
const backfillWindow = 6 * 30 * 24 * time.Hour // ≈6 个月

// maxFetchAttempts 非瞬时爬取失败最大次数，超过则 FAILED_PERM
const maxFetchAttempts = 3

// transientFailWindow 405/WAF 等瞬时错误允许持续重试的最长时间，超时 → FAILED_PERM
const transientFailWindow = 24 * time.Hour

// latestSubmitTimeFromLogs 从 submit_logs 取该题最近一次提交时间
func (uc *ProblemUseCase) latestSubmitTimeFromLogs(problemID uint) *time.Time {
	var t *time.Time
	_ = uc.data.DB.Model(&model.SubmitLog{}).
		Select("MAX(time)").
		Where("problem_id = ?", problemID).
		Scan(&t).Error
	return t
}

// refreshLastSubmittedAt 用 submit_logs 最近提交回写 problems.last_submitted_at
func (uc *ProblemUseCase) refreshLastSubmittedAt(p *model.Problem) *time.Time {
	if p == nil || p.ID == 0 {
		return nil
	}
	latest := uc.latestSubmitTimeFromLogs(p.ID)
	if latest == nil {
		return p.LastSubmittedAt
	}
	if p.LastSubmittedAt == nil || latest.After(*p.LastSubmittedAt) {
		_ = uc.data.DB.Model(p).Update("last_submitted_at", *latest).Error
		p.LastSubmittedAt = latest
	}
	return p.LastSubmittedAt
}

// withinAnalyzeWindow 是否在 AI 分析 6 个月窗口内（以 submit_logs 为准）
// 无任何提交记录：不算近 6 月，不分析（避免 NULL last_submitted_at 虚高待分析后入队即 Ack）
func (uc *ProblemUseCase) withinAnalyzeWindow(p *model.Problem) bool {
	t := uc.refreshLastSubmittedAt(p)
	if t == nil {
		return false
	}
	return !t.Before(time.Now().Add(-backfillWindow))
}

// sqlRecentSubmitCutoff 近 6 月有提交：submit_logs 存在 time>=cutoff 的绑定记录
// 用于 Progress 统计 / ResetAll 入队，与 ProcessAnalyze 窗口一致
func sqlHasRecentSubmit(cutoff time.Time) (clause string, args []interface{}) {
	return `EXISTS (
		SELECT 1 FROM submit_logs s
		WHERE s.problem_id = problems.id
		  AND s.time IS NOT NULL
		  AND s.time >= ?
	)`, []interface{}{cutoff}
}

// TryStartAdminOp 管理端重操作互斥（补全/重置/重试）
func (uc *ProblemUseCase) TryStartAdminOp(name string) (ok bool, running string) {
	uc.adminOpMu.Lock()
	defer uc.adminOpMu.Unlock()
	if uc.adminOpName != "" {
		return false, uc.adminOpName
	}
	uc.adminOpName = name
	return true, ""
}

func (uc *ProblemUseCase) FinishAdminOp() {
	uc.adminOpMu.Lock()
	uc.adminOpName = ""
	uc.adminOpMu.Unlock()
}

// Backfill 增量回填（近 6 个月提交）：
// 1) 绑定未关联提交
// 2) 无题面且有组织用户提交 → 入爬取；纯公共域/散户不爬
// 3) 有题面且未分析完 → 入分析（enqueueAnalyzePrio 跳过纯公共域）
// 不清空 MQ（与 ResetQueues 区分）
func (uc *ProblemUseCase) Backfill(limit int) (scanned, bound, created, enqueued, enqueuedFetch, enqueuedAnalyze int64, err error) {
	if limit <= 0 {
		limit = 5000
	}

	// 0) 牛客错误 external_id → 解绑后重解析
	if res := uc.data.DB.Exec(`
		UPDATE submit_logs
		SET problem_id = NULL, external_id = ''
		WHERE platform = ?
		  AND (
		    external_id IS NULL OR external_id = ''
		    OR (
		      external_id !~ '^[0-9]+$'
		      AND external_id !~ '^[0-9a-fA-F]{32}$'
		    )
		  )
	`, spider.NowCoder); res.Error == nil && res.RowsAffected > 0 {
		log.Infof("Backfill: unbound %d NowCoder submits with bad external_id", res.RowsAffected)
	}

	_ = uc.markExistingPermanentFailures()

	// 1) 绑定近 6 个月未关联提交（resolveOne 按状态入爬/分析；已分析则丢弃）
	cutoff := time.Now().Add(-backfillWindow)
	var logs []model.SubmitLog
	err = uc.data.DB.Where("problem_id IS NULL OR problem_id = 0").
		Where("time IS NULL OR time >= ?", cutoff).
		// 力扣合成行（无 titleSlug）resolve 会失败跳过；lc-prob 可入库
		Order("CASE WHEN platform = 'NowCoder' THEN 0 WHEN platform = 'LeetCode' THEN 1 ELSE 2 END, id DESC").
		Limit(limit).Find(&logs).Error
	if err != nil {
		return
	}
	scanned = int64(len(logs))
	for i := range logs {
		_, isNew, rerr := uc.resolveOne(&logs[i], false)
		if rerr != nil {
			continue
		}
		bound++
		if isNew {
			created++
		}
	}

	// 2) 仅处理「近窗有资格用户提交」的题（批量集合，避免对纯公共域几千题逐题查）
	fetchSet, fetchOK := uc.recentPipelineProblemSet("fetch", cutoff)
	aiSet, aiOK := uc.recentPipelineProblemSet("ai", cutoff)
	if !fetchOK || !aiOK {
		// 名单不可用时保守：仍扫近窗未完成题，但加 limit 防止拖死
		log.Warnf("Backfill: pipeline set unavailable fetchOK=%v aiOK=%v, fallback limited scan", fetchOK, aiOK)
		recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
		var todos []model.Problem
		_ = uc.data.DB.
			Where("status NOT IN ?", []string{
				model.ProblemStatusSkipped,
				model.ProblemStatusCompleted,
				model.ProblemStatusFailedPerm,
			}).
			Where(recentClause, recentArgs...).
			Order("last_submitted_at DESC NULLS LAST, id DESC").
			Limit(limit).
			Find(&todos).Error
		for _, p := range todos {
			ef, ea := uc.backfillOneProblem(p)
			enqueuedFetch += ef
			enqueuedAnalyze += ea
			enqueued += ef + ea
		}
	} else {
		// 合并资格题 id
		idSet := make(map[uint]struct{}, len(fetchSet)+len(aiSet))
		for id := range fetchSet {
			idSet[id] = struct{}{}
		}
		for id := range aiSet {
			idSet[id] = struct{}{}
		}
		if len(idSet) > 0 {
			ids := make([]uint, 0, len(idSet))
			for id := range idSet {
				ids = append(ids, id)
			}
			var todos []model.Problem
			_ = uc.data.DB.
				Where("id IN ?", ids).
				Where("status NOT IN ?", []string{
					model.ProblemStatusSkipped,
					model.ProblemStatusCompleted,
					model.ProblemStatusFailedPerm,
				}).
				Order("last_submitted_at DESC NULLS LAST, id DESC").
				Find(&todos).Error
			for _, p := range todos {
				_, hasFetch := fetchSet[p.ID]
				_, hasAI := aiSet[p.ID]
				ef, ea := uc.backfillOneProblemWithGate(p, hasFetch, hasAI)
				enqueuedFetch += ef
				enqueuedAnalyze += ea
				enqueued += ef + ea
			}
		}
	}
	log.Infof("Backfill: scanned=%d bound=%d created=%d fetch=%d analyze=%d",
		scanned, bound, created, enqueuedFetch, enqueuedAnalyze)
	return
}

// backfillOneProblem 单题回填入队（逐题资格检查）
func (uc *ProblemUseCase) backfillOneProblem(p model.Problem) (enqueuedFetch, enqueuedAnalyze int64) {
	return uc.backfillOneProblemWithGate(p, uc.problemHasFetchSubmitter(p.ID), uc.problemHasAISubmitter(p.ID))
}

func (uc *ProblemUseCase) backfillOneProblemWithGate(p model.Problem, hasFetch, hasAI bool) (enqueuedFetch, enqueuedAnalyze int64) {
	if !hasFetch && !hasAI {
		return 0, 0
	}
	if strings.TrimSpace(p.ContentMD) == "" {
		if !hasFetch {
			return 0, 0
		}
		_ = uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
			Updates(map[string]interface{}{
				"status":           model.ProblemStatusPending,
				"error_msg":        "",
				"fetch_attempts":   0,
				"fetch_fail_since": nil,
			}).Error
		if e := uc.enqueueFetchPrio(p.ID, p.Platform, p.ExternalID, p.URL, mqPriorityBulk); e == nil {
			return 1, 0
		}
		return 0, 0
	}
	if !hasAI {
		return 0, 0
	}
	_ = uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
		Updates(map[string]interface{}{
			"status":    model.ProblemStatusTagging,
			"error_msg": "",
		}).Error
	if e := uc.enqueueAnalyzePrio(p.ID, mqPriorityBulk); e == nil {
		return 0, 1
	}
	return 0, 0
}

// ResetQueues 重置 MQ：purge 爬取/分析队列，再按 DB 待爬取/待分析重灌（bulk 优先级）
// 与 Backfill 不同：不扫提交、不改业务状态，只重建队列。
func (uc *ProblemUseCase) ResetQueues() (purgedFetch, purgedAnalyze, enqueuedFetch, enqueuedAnalyze int, err error) {
	if n, e := uc.purgeFetchQueue(); e == nil {
		purgedFetch = n
	} else if err == nil {
		err = e
	}
	if n, e := uc.purgeAnalyzeQueue(); e == nil {
		purgedAnalyze = n
	} else if err == nil {
		err = e
	}

	// 单次重灌上限：防止历史积压把整表拉进内存/灌爆 MQ（可再次触发续灌）
	const resetQueueScanLimit = 5000

	// 待爬取：PENDING / FETCHING；仅有组织用户提交的题才重灌
	cutoff := time.Now().Add(-backfillWindow)
	// 批量取「近窗有爬取资格用户提交」题集合，避免逐题 COUNT
	fetchSet, fetchOK := uc.recentPipelineProblemSet("fetch", cutoff)
	var fetchTodos []model.Problem
	_ = uc.data.DB.
		Where("status IN ?", []string{model.ProblemStatusPending, model.ProblemStatusFetching}).
		Where("(content_md IS NULL OR content_md = '')").
		Order("last_submitted_at DESC NULLS LAST, id DESC").
		Limit(resetQueueScanLimit).
		Find(&fetchTodos).Error
	for _, p := range fetchTodos {
		if fetchOK {
			if _, ok := fetchSet[p.ID]; !ok {
				continue
			}
		} else if !uc.shouldEnqueueFetch(p.ID) {
			// 名单不可用回退逐题检查
			continue
		}
		_ = uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
			Update("status", model.ProblemStatusPending).Error
		if e := uc.enqueueFetchPrio(p.ID, p.Platform, p.ExternalID, p.URL, mqPriorityBulk); e == nil {
			enqueuedFetch++
		}
	}

	// 待分析：TAGGING + 有题面；已 COMPLETED 不入队
	recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
	var analyzeTodos []model.Problem
	_ = uc.data.DB.
		Where("status = ?", model.ProblemStatusTagging).
		Where("content_md IS NOT NULL AND content_md != ''").
		Where(recentClause, recentArgs...).
		Order("last_submitted_at DESC NULLS LAST, id DESC").
		Limit(resetQueueScanLimit).
		Find(&analyzeTodos).Error
	for _, p := range analyzeTodos {
		if e := uc.enqueueAnalyzePrio(p.ID, mqPriorityBulk); e == nil {
			enqueuedAnalyze++
		}
	}
	log.Infof("ResetQueues: purged_fetch=%d purged_analyze=%d enq_fetch=%d enq_analyze=%d",
		purgedFetch, purgedAnalyze, enqueuedFetch, enqueuedAnalyze)
	return
}

// ClearNowCoderContentAndRefetch 清空全部 NowCoder 题面（content_md），
// 保留 tags / solutions_meta / difficulty / problem_type。
// requeue=true 时强制入队重爬；已有标签/分析的题爬回后不会重跑 AI。
func (uc *ProblemUseCase) ClearNowCoderContentAndRefetch(requeue bool) (cleared, enqueued int64, err error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return 0, 0, fmt.Errorf("usecase not ready")
	}
	// 只清题面字段与爬取状态；绝不碰 tags / solutions_meta
	res := uc.data.DB.Model(&model.Problem{}).
		Where("platform = ?", spider.NowCoder).
		Updates(map[string]interface{}{
			"content_md":       "",
			"status":           model.ProblemStatusPending,
			"error_msg":        "",
			"fetch_attempts":   0,
			"fetch_fail_since": nil,
		})
	if res.Error != nil {
		return 0, 0, res.Error
	}
	cleared = res.RowsAffected
	log.Infof("ClearNowCoderContent: cleared content_md for %d NowCoder problems (tags/solutions kept)", cleared)
	go uc.rebuildProgressCounters()

	if !requeue || cleared == 0 {
		return cleared, 0, nil
	}
	// 强制重爬：带比赛路径；SkipAnalyze 由 ProcessFetch 根据是否已有标签决定
	var list []model.Problem
	_ = uc.data.DB.Select("id, platform, external_id, url, tags, solutions_meta").
		Where("platform = ?", spider.NowCoder).
		Find(&list).Error
	for _, p := range list {
		// ForceEnqueueFetchContest 对无题面强制入队；有比赛映射会走比赛页
		fb := uc.nowcoderContestFetchURLs(p.ExternalID, p.ID)
		if e := uc.ForceEnqueueFetchContest(p.ID, fb...); e == nil {
			enqueued++
		} else {
			// 兜底：普通强制入队（SkipAnalyze 当已有标签）
			skip := len(nonEmptyTags(p.Tags)) > 0 || len(p.SolutionsMeta) > 0
			if e2 := uc.enqueueFetchForced(p.ID, p.Platform, p.ExternalID, p.URL, skip, 0); e2 == nil {
				// enqueueFetchForced 需要 Force=true — check
				enqueued++
			}
		}
	}
	log.Infof("ClearNowCoderContent: enqueued fetch %d / %d", enqueued, len(list))
	return cleared, enqueued, nil
}

// isBadFetchedTitle 爬取/解析得到的「站点品牌」伪标题（QOJ 首 h1 常为 QOJ.ac）
func isBadFetchedTitle(platform, title string) bool {
	if strings.EqualFold(strings.TrimSpace(platform), spider.QOJ) {
		return problem_fetch.IsQOJBrandTitle(title)
	}
	return false
}

// isPlaceholderProblemTitle 仅题号/占位（如 POJ 入库时的 "1000" / "#1000"），可被真实题名替换。
func isPlaceholderProblemTitle(platform, title, externalID string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	ext := strings.TrimSpace(externalID)
	// 纯 external_id 或 #external_id
	if ext != "" && (t == ext || t == "#"+ext) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(platform), spider.POJ) {
		// POJ 早期占位：纯数字题号
		onlyNum := true
		for _, r := range strings.TrimPrefix(t, "#") {
			if r < '0' || r > '9' {
				onlyNum = false
				break
			}
		}
		if onlyNum && t != "" {
			return true
		}
	}
	return false
}

// shouldReplaceProblemTitle 空标题或品牌垃圾标题可被更好的新标题替换
func shouldReplaceProblemTitle(platform, oldTitle, newTitle string) bool {
	newTitle = strings.TrimSpace(newTitle)
	if newTitle == "" || isBadFetchedTitle(platform, newTitle) {
		return false
	}
	oldTitle = strings.TrimSpace(oldTitle)
	if oldTitle == "" || isBadFetchedTitle(platform, oldTitle) {
		return true
	}
	// 占位题号 → 真实「#1000. A+B Problem」
	if isPlaceholderProblemTitle(platform, oldTitle, "") && !isPlaceholderProblemTitle(platform, newTitle, "") {
		return true
	}
	return false
}

// titleFromMarkdownH1 取 Markdown 首个一级标题（AI 整理题面常用「# 题名」）
func titleFromMarkdownH1(md string) string {
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "##") {
			continue
		}
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "#\t") {
			t := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if t != "" && !problem_fetch.IsQOJBrandTitle(t) {
				return t
			}
		}
		// 首行非标题则不再往下找（避免误取正文中的 #）
		if !strings.HasPrefix(line, "#") {
			break
		}
	}
	return ""
}

// RepairQOJBrandTitles 全量修复 QOJ 题目标题被识别为「QOJ.ac」的脏数据。
// 策略：优先从已有 content_md 一级标题回填；不足时可选 refetch 拉官方题头；再退回 #题号。
// refetch=true 时对仍无好标题的题访问 qoj.ac（有间隔，避免打爆）。
func (uc *ProblemUseCase) RepairQOJBrandTitles(limit int, refetch bool) (scanned, fixed, failed, skipped int64, err error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return 0, 0, 0, 0, fmt.Errorf("usecase not ready")
	}
	q := uc.data.DB.Model(&model.Problem{}).
		Where("platform = ?", spider.QOJ).
		Where(`title IS NULL OR BTRIM(title) = '' OR LOWER(BTRIM(title)) IN ('qoj.ac','qoj','qoj ac')`)
	if limit > 0 {
		q = q.Limit(limit)
	}
	var list []model.Problem
	if err = q.Find(&list).Error; err != nil {
		return
	}
	log.Infof("RepairQOJBrandTitles: candidates=%d limit=%d refetch=%v", len(list), limit, refetch)
	for i := range list {
		p := list[i]
		scanned++
		newTitle := titleFromMarkdownH1(p.ContentMD)
		if newTitle == "" && refetch {
			fetched, ferr := problem_fetch.FetchWithFallbacks(p.Platform, p.ExternalID, p.URL, nil)
			if ferr != nil {
				log.Warnf("RepairQOJBrandTitles fetch id=%d ext=%s: %v", p.ID, p.ExternalID, ferr)
				failed++
				time.Sleep(400 * time.Millisecond)
				continue
			}
			if fetched != nil {
				t := strings.TrimSpace(fetched.Title)
				if t != "" && !problem_fetch.IsQOJBrandTitle(t) {
					newTitle = t
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
		if newTitle == "" && strings.TrimSpace(p.ExternalID) != "" {
			newTitle = "#" + strings.TrimSpace(p.ExternalID)
		}
		if newTitle == "" || problem_fetch.IsQOJBrandTitle(newTitle) || newTitle == strings.TrimSpace(p.Title) {
			skipped++
			continue
		}
		if e := uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).Update("title", newTitle).Error; e != nil {
			log.Warnf("RepairQOJBrandTitles update id=%d: %v", p.ID, e)
			failed++
			continue
		}
		uc.BumpProblemDetailVer(p.ID)
		fixed++
		if fixed%100 == 0 {
			log.Infof("RepairQOJBrandTitles progress fixed=%d/%d", fixed, len(list))
		}
	}
	if fixed > 0 {
		uc.BumpProblemListVer()
	}
	log.Infof("RepairQOJBrandTitles done scanned=%d fixed=%d failed=%d skipped=%d", scanned, fixed, failed, skipped)
	return scanned, fixed, failed, skipped, nil
}

// ClearRecentFailed 清空近期失败：近 6 月有提交且状态为 FAILED 的题 → FAILED_PERM，
// 停止 scheduleFetchRetry / 消费者自动退避重试。管理员仍可用「重试永久失败」手动再试。
func (uc *ProblemUseCase) ClearRecentFailed() (cleared int64, err error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return 0, fmt.Errorf("usecase not ready")
	}
	cutoff := time.Now().Add(-backfillWindow)
	recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
	res := uc.data.DB.Model(&model.Problem{}).
		Where("status = ?", model.ProblemStatusFailed).
		Where(recentClause, recentArgs...).
		Updates(map[string]interface{}{
			"status":           model.ProblemStatusFailedPerm,
			"error_msg":        "已停止自动重试（管理员清空近期失败）",
			"fetch_fail_since": nil,
		})
	if res.Error != nil {
		return 0, res.Error
	}
	cleared = res.RowsAffected
	if cleared > 0 {
		log.Infof("ClearRecentFailed: %d FAILED → FAILED_PERM (auto-retry stopped)", cleared)
		// 进度计数可能缓存，尽量失效
		go uc.rebuildProgressCounters()
	}
	return cleared, nil
}

// RetryFailed 重试错误队列。
// includePermanent=false：仅 FAILED；并解除误标的 WAF/登录墙/DOM 类 FAILED_PERM
// includePermanent=true：近 6 月 FAILED_PERM 中非硬永久错误也重置入队（管理员「重试永久失败」）
// 仅近 6 月有提交 + 有流水线资格用户提交的题才会真正入队（避免公共域假入队后立刻 Ack）
func (uc *ProblemUseCase) RetryFailed(limit int, includePermanent bool) (scanned, enqueued, blacklisted int64, err error) {
	pipelineControl.SetAnalyzePaused(false)

	// 解除误标：WAF/登录墙/DOM 类不应进黑名单（历史曾标 FAILED_PERM）
	// 注意：暂无访问权限是真永久（题库页不可访），不在此解除；有 contest 路径时另开再爬
	if res := uc.data.DB.Model(&model.Problem{}).
		Where("status = ?", model.ProblemStatusFailedPerm).
		Where(
			"error_msg LIKE ? OR error_msg LIKE ? OR error_msg LIKE ? OR error_msg LIKE ? OR error_msg LIKE ?",
			"%需要登录%", "%被拦截%", "%WAF%",
			"%未找到题面%", "%题面为空%",
		).
		// 排除牛客题库无权限 / CF 未找到题面（真永久：等 contest 模式再爬 / 题目不存在需人工处理）
		Where("error_msg NOT LIKE ? AND error_msg NOT LIKE ? AND error_msg NOT LIKE ?",
			"%暂无访问权限%", "%没有查看题目的权限%", "%CF 未找到题面%").
		Updates(map[string]interface{}{
			"status":           model.ProblemStatusFailed,
			"error_msg":        "retry: was false permanent (WAF/login/DOM)",
			"fetch_attempts":   0,
			"fetch_fail_since": nil,
		}); res.Error == nil && res.RowsAffected > 0 {
		log.Infof("RetryFailed: unblocked %d false FAILED_PERM (WAF/login/DOM)", res.RowsAffected)
	}

	// 管理员显式重试永久失败：把近 6 月非硬永久的 FAILED_PERM 全部降级为 FAILED
	if includePermanent {
		cutoff := time.Now().Add(-backfillWindow)
		recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
		var perms []model.Problem
		_ = uc.data.DB.Where("status = ?", model.ProblemStatusFailedPerm).
			Where(recentClause, recentArgs...).
			Find(&perms).Error
		var unblocked int64
		for _, p := range perms {
			// 硬永久（QOJ 403 / 付费题等）仍跳过
			if isPermanentFetchError(p.ErrorMsg) || isQOJFailedForbidden(&p) {
				continue
			}
			if err2 := uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
				Updates(map[string]interface{}{
					"status":           model.ProblemStatusFailed,
					"error_msg":        "retry: admin requeue permanent",
					"fetch_attempts":   0,
					"fetch_fail_since": nil,
				}).Error; err2 == nil {
				unblocked++
			}
		}
		if unblocked > 0 {
			log.Infof("RetryFailed: admin unblocked %d FAILED_PERM for retry", unblocked)
		}
	}

	// 先把已是硬永久错误文案的 FAILED 升为黑名单
	blacklisted = uc.markExistingPermanentFailures()

	// 近 6 月以 submit_logs 为准（与 Progress / ProcessAnalyze 一致）
	cutoff := time.Now().Add(-backfillWindow)
	recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
	q := uc.data.DB.Where("status = ?", model.ProblemStatusFailed).
		Where(recentClause, recentArgs...).
		Order("last_submitted_at DESC NULLS LAST, id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var todos []model.Problem
	if err = q.Find(&todos).Error; err != nil {
		return
	}
	scanned = int64(len(todos))

	fetchSet, fetchOK := uc.recentPipelineProblemSet("fetch", cutoff)
	aiSet, aiOK := uc.recentPipelineProblemSet("ai", cutoff)

	seen := map[uint]bool{}
	for _, p := range todos {
		if seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		// 双保险：error_msg 已是硬永久错误 → 黑名单，不入队
		if isPermanentFetchError(p.ErrorMsg) {
			_ = uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
				Update("status", model.ProblemStatusFailedPerm).Error
			blacklisted++
			continue
		}
		hasContent := strings.TrimSpace(p.ContentMD) != ""
		hasFetch := false
		hasAI := false
		if fetchOK {
			_, hasFetch = fetchSet[p.ID]
		} else {
			hasFetch = uc.problemHasFetchSubmitter(p.ID)
		}
		if aiOK {
			_, hasAI = aiSet[p.ID]
		} else {
			hasAI = uc.problemHasAISubmitter(p.ID)
		}
		if hasContent {
			if !hasAI {
				continue
			}
			_ = uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
				Updates(map[string]interface{}{
					"status":           model.ProblemStatusTagging,
					"error_msg":        "",
					"fetch_attempts":   0,
					"fetch_fail_since": nil,
				}).Error
			if e := uc.enqueueAnalyze(p.ID); e == nil {
				enqueued++
			}
		} else {
			if !hasFetch {
				continue
			}
			_ = uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
				Updates(map[string]interface{}{
					"status":           model.ProblemStatusPending,
					"error_msg":        "",
					"fetch_attempts":   0,
					"fetch_fail_since": nil,
				}).Error
			if e := uc.enqueueFetch(p.ID, p.Platform, p.ExternalID, p.URL); e == nil {
				enqueued++
			}
		}
	}
	log.Infof("RetryFailed: includePermanent=%v scanned=%d enqueued=%d blacklisted=%d",
		includePermanent, scanned, enqueued, blacklisted)
	return
}

// markExistingPermanentFailures 将历史 FAILED 中匹配永久错误文案的标为 FAILED_PERM
func (uc *ProblemUseCase) markExistingPermanentFailures() int64 {
	var list []model.Problem
	_ = uc.data.DB.Where("status = ?", model.ProblemStatusFailed).
		Where("error_msg IS NOT NULL AND error_msg != ''").
		Find(&list).Error
	var n int64
	for _, p := range list {
		if !isPermanentFetchError(p.ErrorMsg) && !isQOJFailedForbidden(&p) {
			continue
		}
		if err := uc.data.DB.Model(&model.Problem{}).Where("id = ?", p.ID).
			Updates(map[string]interface{}{
				"status":    model.ProblemStatusFailedPerm,
				"error_msg": normalizeQOJForbiddenMsg(p.ErrorMsg, p.Platform),
			}).Error; err == nil {
			n++
		}
	}
	if n > 0 {
		log.Infof("markExistingPermanentFailures: %d → FAILED_PERM", n)
	}
	return n
}

// isQOJFailedForbidden 历史 QOJ 题 error_msg 含 403 / status 403 → 无权限
func isQOJFailedForbidden(p *model.Problem) bool {
	if p == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(p.Platform), "QOJ") {
		return false
	}
	msg := p.ErrorMsg
	return strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "QOJ 无权限") ||
		(strings.Contains(msg, "403") && strings.Contains(strings.ToLower(msg), "forbidden"))
}

func normalizeQOJForbiddenMsg(msg, platform string) string {
	if strings.EqualFold(strings.TrimSpace(platform), "QOJ") &&
		(strings.Contains(msg, "status 403") || isQOJForbiddenError(msg)) {
		return "QOJ 无权限访问题面(403)"
	}
	if isPermanentFetchError(msg) {
		return msg
	}
	return msg
}

type ListProblemFilter struct {
	Page         int64
	PageSize     int64
	Sort         string
	Platforms    []string
	Tags         []string
	UserStatus   string
	UserID       int64
	Keyword      string
	Difficulty   string
	FollowingIDs []int64 // 非空：仅这些用户提交过的题
}

type listCachePayload struct {
	List  []model.Problem
	Total int64
}

func listFilterCacheable(f ListProblemFilter) bool {
	// 首屏、无关键词/状态筛选/关注过滤；platforms/tags/difficulty 写入 key
	if f.Page != 1 {
		return false
	}
	if strings.TrimSpace(f.Keyword) != "" {
		return false
	}
	if strings.TrimSpace(f.UserStatus) != "" {
		return false
	}
	if f.FollowingIDs != nil {
		return false
	}
	return true
}

func (uc *ProblemUseCase) List(f ListProblemFilter) ([]model.Problem, map[uint]string, int64, error) {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}

	// 默认列表短缓存（不含 userStatusMap）
	if listFilterCacheable(f) && uc.data != nil && uc.data.RDB != nil {
		ver := uc.redisVer(problemListVerKey)
		plats := strings.Join(f.Platforms, ",")
		tags := strings.Join(dal.NormalizeTags(f.Tags), ",")
		diff := strings.TrimSpace(f.Difficulty)
		key := fmt.Sprintf("problem:list:v%s:p%d:ps%d:plat{%s}:tag{%s}:diff{%s}",
			ver, f.Page, f.PageSize, plats, tags, diff)
		payload, _, err := data2.GetCacheDalTTL[listCachePayload](context.Background(), uc.data.RDB, key, problemListCacheTTL, func(data *listCachePayload) error {
			list, total, e := uc.listProblemsDB(f)
			if e != nil {
				return e
			}
			data.List = list
			data.Total = total
			return nil
		})
		if err == nil && payload != nil {
			userStatusMap := map[uint]string{}
			if f.UserID > 0 && len(payload.List) > 0 {
				ids := make([]uint, 0, len(payload.List))
				for i := range payload.List {
					ids = append(ids, payload.List[i].ID)
				}
				if m, e := dal.GetUserProblemStatuses(context.Background(), uc.data.DB, f.UserID, ids); e == nil {
					userStatusMap = m
				}
			}
			return payload.List, userStatusMap, payload.Total, nil
		}
	}

	list, total, err := uc.listProblemsDB(f)
	if err != nil {
		return nil, nil, 0, err
	}
	userStatusMap := map[uint]string{}
	if f.UserID > 0 && len(list) > 0 {
		ids := make([]uint, 0, len(list))
		for i := range list {
			ids = append(ids, list[i].ID)
		}
		if m, e := dal.GetUserProblemStatuses(context.Background(), uc.data.DB, f.UserID, ids); e == nil {
			userStatusMap = m
		}
	}
	return list, userStatusMap, total, nil
}

func (uc *ProblemUseCase) listProblemsDB(f ListProblemFilter) ([]model.Problem, int64, error) {
	q := uc.data.DB.Model(&model.Problem{})
	if len(f.Platforms) > 0 {
		q = q.Where("platform IN ?", f.Platforms)
	}
	if len(f.Tags) > 0 {
		clean := dal.NormalizeTags(f.Tags)
		if len(clean) > 0 {
			q = q.Where(`id IN (
				SELECT problem_id FROM problem_tags WHERE tag IN ?
			)`, clean)
		}
	}
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := sqllike.Pattern(kw)
		q = q.Where("(title ILIKE ? OR external_id ILIKE ?)", like, like)
	}
	if d := strings.TrimSpace(f.Difficulty); d != "" {
		q = q.Where("difficulty = ?", d)
	}
	if len(f.FollowingIDs) > 0 {
		q = q.Where(`EXISTS (
			SELECT 1 FROM user_problem_status ups
			WHERE ups.problem_id = problems.id AND ups.user_id IN ?
		)`, f.FollowingIDs)
	} else if f.FollowingIDs != nil {
		q = q.Where("1 = 0")
	}

	if f.UserID > 0 && f.UserStatus != "" {
		want := strings.ToUpper(strings.TrimSpace(f.UserStatus))
		switch want {
		case "NONE":
			q = q.Where(`NOT EXISTS (
				SELECT 1 FROM user_problem_status ups
				WHERE ups.problem_id = problems.id AND ups.user_id = ?
			)`, f.UserID)
		case "AC":
			q = q.Where(`EXISTS (
				SELECT 1 FROM user_problem_status ups
				WHERE ups.problem_id = problems.id AND ups.user_id = ? AND ups.status = 'AC'
			)`, f.UserID)
		case "TRIED":
			q = q.Where(`EXISTS (
				SELECT 1 FROM user_problem_status ups
				WHERE ups.problem_id = problems.id AND ups.user_id = ? AND ups.status = 'TRIED'
			)`, f.UserID)
		}
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := `
		CASE
			WHEN content_md IS NULL OR btrim(content_md) = '' THEN 2
			WHEN tags IS NULL OR btrim(tags::text) IN ('', '[]', 'null') THEN 1
			ELSE 0
		END ASC,
		last_submitted_at DESC NULLS LAST,
		id DESC`
	var list []model.Problem
	err := q.Order(order).Offset(int((f.Page - 1) * f.PageSize)).Limit(int(f.PageSize)).Find(&list).Error
	return list, total, err
}

// TagCount 标签及题目数（用于筛选器）
type TagCount struct {
	Tag   string
	Count int64
}

// HotProblemRow 全站热题一行（含题库信息 + 近窗统计）
type HotProblemRow struct {
	Problem     model.Problem
	SubmitCount int64
	SolverCount int64
	AcCount     int64
	Score       float64
	LastTime    time.Time
}

type hotListCachePayload struct {
	Rows  []HotProblemRow
	Total int64
	Days  int
}

const (
	// problemHotCacheTTL 热题榜短缓存（窗口聚合较重）
	problemHotCacheTTL = 90 * time.Second
	// hotScore weights documented for API
	// score = submit*1 + solver*3 + ac*2
)

// ListHot 全站热题：近 days 天 submit/solver/ac 综合分排序。
// days 默认 2，夹紧到 [1,7]。
func (uc *ProblemUseCase) ListHot(page, pageSize int64, days int) ([]HotProblemRow, int64, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if days <= 0 {
		days = 2
	}
	if days > 7 {
		days = 7
	}

	ctx := context.Background()
	if uc.data != nil && uc.data.RDB != nil {
		key := fmt.Sprintf("problem:hot:d%d:p%d:ps%d", days, page, pageSize)
		payload, _, err := data2.GetCacheDalTTL[hotListCachePayload](ctx, uc.data.RDB, key, problemHotCacheTTL, func(data *hotListCachePayload) error {
			rows, total, e := uc.listHotDB(page, pageSize, days)
			if e != nil {
				return e
			}
			data.Rows = rows
			data.Total = total
			data.Days = days
			return nil
		})
		if err == nil && payload != nil {
			return payload.Rows, payload.Total, payload.Days, nil
		}
	}
	rows, total, err := uc.listHotDB(page, pageSize, days)
	return rows, total, days, err
}

func (uc *ProblemUseCase) listHotDB(page, pageSize int64, days int) ([]HotProblemRow, int64, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	aggs, total, err := dal.ListHotProblems(context.Background(), uc.data.DB, since, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	if len(aggs) == 0 {
		return []HotProblemRow{}, total, nil
	}
	ids := make([]uint, 0, len(aggs))
	for _, a := range aggs {
		ids = append(ids, a.ProblemID)
	}
	var problems []model.Problem
	if err := uc.data.DB.Where("id IN ?", ids).Find(&problems).Error; err != nil {
		return nil, 0, err
	}
	byID := make(map[uint]model.Problem, len(problems))
	for i := range problems {
		byID[problems[i].ID] = problems[i]
	}
	out := make([]HotProblemRow, 0, len(aggs))
	for _, a := range aggs {
		p, ok := byID[a.ProblemID]
		if !ok {
			// 题库行缺失时跳过（孤儿 problem_id）
			continue
		}
		out = append(out, HotProblemRow{
			Problem:     p,
			SubmitCount: a.SubmitCount,
			SolverCount: a.SolverCount,
			AcCount:     a.AcCount,
			Score:       a.Score,
			LastTime:    a.LastTime,
		})
	}
	return out, total, nil
}

// ListTags 从 problem_tags 倒排聚合（Redis 缓存）
func (uc *ProblemUseCase) ListTags(limit int) ([]TagCount, error) {
	if limit <= 0 {
		limit = 100
	}
	ctx := context.Background()
	if uc.data != nil && uc.data.RDB != nil {
		ver := uc.redisVer(problemTagsVerKey)
		key := fmt.Sprintf("problem:tags:count:v%s:lim%d", ver, limit)
		cached, _, err := data2.GetCacheDalTTL[[]TagCount](ctx, uc.data.RDB, key, problemTagsCacheTTL, func(data *[]TagCount) error {
			list, e := uc.listTagsDB(limit)
			if e != nil {
				return e
			}
			*data = list
			return nil
		})
		if err == nil && cached != nil {
			return *cached, nil
		}
	}
	return uc.listTagsDB(limit)
}

func (uc *ProblemUseCase) listTagsDB(limit int) ([]TagCount, error) {
	rows, err := dal.ListTagCounts(context.Background(), uc.data.DB, limit)
	if err != nil {
		return nil, err
	}
	out := make([]TagCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, TagCount{Tag: r.Tag, Count: r.Count})
	}
	// 表空时回退 jsonb（启动竞态）
	if len(out) == 0 {
		var fb []TagCount
		_ = uc.data.DB.Raw(`
			SELECT tag, COUNT(DISTINCT p.id) AS count
			FROM problems p
			CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(p.tags::jsonb, '[]'::jsonb)) AS tag
			WHERE p.tags IS NOT NULL AND p.tags::text NOT IN ('', '[]', 'null') AND BTRIM(tag) <> ''
			GROUP BY tag ORDER BY count DESC, tag ASC LIMIT ?
		`, limit).Scan(&fb).Error
		return fb, nil
	}
	return out, nil
}

func (uc *ProblemUseCase) Get(id uint) (*model.Problem, error) {
	return uc.getProblemCached(id)
}

// ProblemRelatedContest 题目在站内出现过的比赛（全平台）
type ProblemRelatedContest struct {
	Platform     string
	ContestID    string
	Label        string
	ContestName  string
	ContestLogID uint
	ContestTime  int64
	ProblemTitle string
	ContestURL   string
}

// ListRelatedContests 按 problem_id（及 external_id 兜底）反查 contest_problems，
// 并尽量挂上 contest_logs 的名称/时间/站内详情 id。
func (uc *ProblemUseCase) ListRelatedContests(problemID uint) ([]ProblemRelatedContest, error) {
	if uc == nil || uc.data == nil || uc.data.DB == nil || problemID == 0 {
		return nil, nil
	}
	var p model.Problem
	if err := uc.data.DB.Select("id, platform, external_id").First(&p, problemID).Error; err != nil {
		return nil, err
	}
	var cps []model.ContestProblem
	q := uc.data.DB.Where("problem_id = ?", problemID)
	if p.ExternalID != "" {
		// 历史行可能只写了 external_id、problem_id=0
		q = uc.data.DB.Where(
			"problem_id = ? OR (platform = ? AND external_id = ?)",
			problemID, p.Platform, p.ExternalID,
		)
	}
	if err := q.Order("updated_at DESC").Limit(50).Find(&cps).Error; err != nil {
		return nil, err
	}
	// 去重 platform+contest_id+label
	type key struct{ plat, cid, label string }
	seen := map[key]struct{}{}
	out := make([]ProblemRelatedContest, 0, len(cps))
	for _, cp := range cps {
		k := key{cp.Platform, cp.ContestID, cp.Label}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		item := ProblemRelatedContest{
			Platform:     cp.Platform,
			ContestID:    cp.ContestID,
			Label:        cp.Label,
			ProblemTitle: cp.Title,
			ContestURL:   cp.URL,
		}
		// 任取一条 contest_logs 作为站内详情入口
		var cl model.ContestLog
		err := uc.data.DB.
			Where("platform = ? AND contest_id = ?", cp.Platform, cp.ContestID).
			Order("id ASC").
			First(&cl).Error
		if err == nil {
			item.ContestLogID = cl.ID
			item.ContestName = cl.ContestName
			item.ContestURL = firstNonEmpty(cl.ContestUrl, item.ContestURL)
			if !cl.Time.IsZero() {
				item.ContestTime = cl.Time.Unix()
			}
		}
		out = append(out, item)
	}
	// 时间新→旧
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ContestTime != out[j].ContestTime {
			return out[i].ContestTime > out[j].ContestTime
		}
		return out[i].ContestID > out[j].ContestID
	})
	return out, nil
}

func (uc *ProblemUseCase) ListSubmissions(problemID uint, userID, page, pageSize int64, followingIDs []int64, status string) ([]model.SubmitLog, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	q := uc.data.DB.Model(&model.SubmitLog{}).Where("problem_id = ?", problemID)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	if followingIDs != nil {
		if len(followingIDs) == 0 {
			return []model.SubmitLog{}, 0, nil
		}
		q = q.Where("user_id IN ?", followingIDs)
	}
	if strings.EqualFold(strings.TrimSpace(status), "AC") {
		q = q.Where(sqlACStatusCond("status"))
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []model.SubmitLog
	err := q.Order("time desc").Offset(int((page - 1) * pageSize)).Limit(int(pageSize)).Find(&list).Error
	return list, total, err
}

// FollowingProblemStatus 关注用户对本题 AC/TRIED/NONE（上限 200，读预聚合）
func (uc *ProblemUseCase) FollowingProblemStatus(problemID uint, followingIDs []int64) ([]struct {
	UserID int64
	Status string
}, error) {
	out := make([]struct {
		UserID int64
		Status string
	}, 0)
	if problemID == 0 || len(followingIDs) == 0 {
		return out, nil
	}
	if len(followingIDs) > 200 {
		followingIDs = followingIDs[:200]
	}
	statusMap := make(map[int64]string, len(followingIDs))
	for _, id := range followingIDs {
		statusMap[id] = "NONE"
	}
	m, err := dal.GetFollowingProblemStatuses(context.Background(), uc.data.DB, problemID, followingIDs)
	if err != nil {
		return nil, err
	}
	for uid, st := range m {
		if st == model.UserProblemStatusAC || st == model.UserProblemStatusTried {
			statusMap[uid] = st
		}
	}
	for _, id := range followingIDs {
		out = append(out, struct {
			UserID int64
			Status string
		}{UserID: id, Status: statusMap[id]})
	}
	return out, nil
}

// UserProfile 见 user_profile.go（缓存 + MQ 预计算）

type ProgressSnapshot struct {
	Items []struct {
		Status string
		Count  int64
	}
	Failed          []model.Problem
	FailedPerm      []model.Problem
	FailedTotal     int64
	FailedPermTotal int64
	FailedPage      int64
	FailedPageSize  int64
	InProgress      []model.Problem
	Total           int64
	Paused          bool // AI 暂停（兼容）
	FetchPaused     bool
	AnalyzePaused   bool
	ActiveJobs      []ActiveJob
	Queues          []struct {
		Name        string
		Messages    int64
		Consumers   int64
		Concurrency int64
	}
}

const progressSnapshotCacheKey = "problem:progress:snapshot:v1"
const progressSnapshotCacheTTL = 15 * time.Second

func (uc *ProblemUseCase) Progress(page, pageSize int64) (ProgressSnapshot, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	// 短缓存：管理端轮询 + 并发打开时避免反复 EXISTS(submit_logs) 扫表
	if uc.data != nil && uc.data.RDB != nil {
		cacheKey := fmt.Sprintf("%s:p%d:s%d", progressSnapshotCacheKey, page, pageSize)
		if b, err := uc.data.RDB.Get(context.Background(), cacheKey).Bytes(); err == nil && len(b) > 0 {
			var cached ProgressSnapshot
			if json.Unmarshal(b, &cached) == nil {
				// 暂停态 / 活跃任务以进程内为准（秒级变化，不宜吃 15s 旧缓存）
				cached.Paused = pipelineControl.IsAnalyzePaused()
				cached.FetchPaused = pipelineControl.IsFetchPaused()
				cached.AnalyzePaused = pipelineControl.IsAnalyzePaused()
				cached.ActiveJobs = pipelineControl.SnapshotActive()
				return cached, nil
			}
		}
	}

	var snap ProgressSnapshot
	type sc struct {
		Status string
		Count  int64
	}
	// 全量：PENDING / FETCHING / COMPLETED
	// 近 6 个月：以 submit_logs 有 time>=cutoff 为准（与 ProcessAnalyze 一致，禁止 NULL 虚高）
	cutoff := time.Now().Add(-backfillWindow)
	recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
	fullStatuses := []string{
		model.ProblemStatusPending,
		model.ProblemStatusFetching,
		model.ProblemStatusCompleted,
	}

	// 优先 Redis 全量状态计数（P8）；近窗状态仍 SQL
	var rows []sc
	if m, ok := uc.progressCountersFromRedis(); ok {
		for st, c := range m {
			// 全量三类直接用 hash；其余仍走近窗 SQL
			isFull := false
			for _, fs := range fullStatuses {
				if st == fs {
					isFull = true
					break
				}
			}
			if isFull {
				rows = append(rows, sc{Status: st, Count: c})
			}
		}
	} else {
		if err := uc.data.DB.Model(&model.Problem{}).
			Select("status, count(*) as count").
			Where("status IN ?", fullStatuses).
			Group("status").Scan(&rows).Error; err != nil {
			return snap, err
		}
		// 异步回填 hash
		go uc.rebuildProgressCounters()
	}
	var recent []sc
	if err := uc.data.DB.Model(&model.Problem{}).
		Select("status, count(*) as count").
		Where("status NOT IN ?", fullStatuses).
		Where(recentClause, recentArgs...).
		Group("status").Scan(&recent).Error; err != nil {
		return snap, err
	}
	rows = append(rows, recent...)
	for _, r := range rows {
		snap.Items = append(snap.Items, struct {
			Status string
			Count  int64
		}{r.Status, r.Count})
		snap.Total += r.Count
	}
	failedQuery := uc.data.DB.Model(&model.Problem{}).Where("status = ?", model.ProblemStatusFailed).Where(recentClause, recentArgs...)
	permQuery := uc.data.DB.Model(&model.Problem{}).Where("status = ?", model.ProblemStatusFailedPerm).Where(recentClause, recentArgs...)
	_ = failedQuery.Count(&snap.FailedTotal).Error
	_ = permQuery.Count(&snap.FailedPermTotal).Error
	offset := (page - 1) * pageSize
	_ = failedQuery.Order("updated_at desc, id desc").Offset(int(offset)).Limit(int(pageSize)).Find(&snap.Failed).Error
	_ = permQuery.Order("updated_at desc, id desc").Offset(int(offset)).Limit(int(pageSize)).Find(&snap.FailedPerm).Error
	snap.FailedPage, snap.FailedPageSize = page, pageSize
	// 爬取中全量；待分析仅近 6 个月（submit_logs）
	_ = uc.data.DB.Where(
		"(status = ?) OR (status = ? AND "+recentClause+")",
		append([]interface{}{model.ProblemStatusFetching, model.ProblemStatusTagging}, recentArgs...)...,
	).Order("updated_at desc").Limit(30).Find(&snap.InProgress).Error

	snap.Paused = pipelineControl.IsAnalyzePaused()
	snap.FetchPaused = pipelineControl.IsFetchPaused()
	snap.AnalyzePaused = pipelineControl.IsAnalyzePaused()
	snap.ActiveJobs = pipelineControl.SnapshotActive()
	snap.Queues = uc.queueStats()

	if uc.data != nil && uc.data.RDB != nil {
		// 缓存不含瞬时 ActiveJobs（读侧会覆盖）；计数/失败列表吃 15s 即可
		toStore := snap
		toStore.ActiveJobs = nil
		if b, err := json.Marshal(toStore); err == nil {
			cacheKey := fmt.Sprintf("%s:p%d:s%d", progressSnapshotCacheKey, page, pageSize)
			_ = uc.data.RDB.Set(context.Background(), cacheKey, b, progressSnapshotCacheTTL).Err()
		}
	}
	return snap, nil
}

func (uc *ProblemUseCase) queueStats() []struct {
	Name        string
	Messages    int64
	Consumers   int64
	Concurrency int64
} {
	out := make([]struct {
		Name        string
		Messages    int64
		Consumers   int64
		Concurrency int64
	}, 0, 2)
	analyzeConcurrency := runtimeConcurrency(context.Background(), uc.data.RDB, analyzeConcurrencySetting)
	for _, q := range []struct {
		name string
		conc int64
		stat string
	}{
		{"problem_fetch", int64(problemFetchConcurrency), model.ProblemStatusPending},
		{"problem_analyze", int64(analyzeConcurrency), model.ProblemStatusTagging},
	} {
		var msgs, consumers int64
		inspected := false
		// 优先读真实 MQ 积压/消费者
		if uc.mq != nil {
			if info, err := uc.mq.QueueInspect(q.name); err == nil {
				msgs = int64(info.Messages)
				consumers = int64(info.Consumers)
				inspected = true
			}
		}
		// inspect 失败时用 DB 近似积压
		if !inspected {
			cq := uc.data.DB.Model(&model.Problem{}).Where("status = ?", q.stat)
			// 分析队列仅近 6 个月（submit_logs）；爬取队列全量
			if q.name == "problem_analyze" {
				cutoff := time.Now().Add(-backfillWindow)
				rc, ra := sqlHasRecentSubmit(cutoff)
				cq = cq.Where(rc, ra...)
			}
			_ = cq.Count(&msgs).Error
			if q.name == "problem_fetch" {
				var fetching int64
				_ = uc.data.DB.Model(&model.Problem{}).Where("status = ?", model.ProblemStatusFetching).Count(&fetching).Error
				msgs += fetching
			}
		}
		out = append(out, struct {
			Name        string
			Messages    int64
			Consumers   int64
			Concurrency int64
		}{q.name, msgs, consumers, q.conc})
	}
	return out
}

func (uc *ProblemUseCase) purgeQueue(name string) (int, error) {
	if uc.mq == nil {
		return 0, fmt.Errorf("mq not ready")
	}
	_ = uc.declareProblemQueue(name)
	return uc.mq.QueuePurge(name, false)
}

func (uc *ProblemUseCase) purgeAnalyzeQueue() (purgedAnalyze int, err error) {
	return uc.purgeQueue("problem_analyze")
}

func (uc *ProblemUseCase) purgeFetchQueue() (purgedFetch int, err error) {
	return uc.purgeQueue("problem_fetch")
}

// PauseAnalyze 暂停 AI 分析（保留队列消息，恢复后继续消费）
func (uc *ProblemUseCase) PauseAnalyze() (purged int, err error) {
	pipelineControl.SetAnalyzePaused(true)
	return 0, nil
}

// ResumeAnalyze 恢复 AI 分析
func (uc *ProblemUseCase) ResumeAnalyze() {
	pipelineControl.SetAnalyzePaused(false)
}

// PauseFetch 暂停题面爬取（保留队列消息，恢复后继续消费）
func (uc *ProblemUseCase) PauseFetch() (purged int, err error) {
	pipelineControl.SetFetchPaused(true)
	return 0, nil
}

// ResumeFetch 恢复题面爬取
func (uc *ProblemUseCase) ResumeFetch() {
	pipelineControl.SetFetchPaused(false)
}

// EmergencyStop 兼容旧 API：暂停 AI（不再 purge；清队列请用 ResetQueues）
func (uc *ProblemUseCase) EmergencyStop() (purgedFetch, purgedAnalyze int, err error) {
	_, err = uc.PauseAnalyze()
	return 0, 0, err
}

// Resume 兼容旧 API：恢复 AI
func (uc *ProblemUseCase) Resume() {
	uc.ResumeAnalyze()
}

func (uc *ProblemUseCase) ProgressPausedAnalyze() bool {
	return pipelineControl.IsAnalyzePaused()
}

func (uc *ProblemUseCase) ProgressPausedFetch() bool {
	return pipelineControl.IsFetchPaused()
}

// ResetAll 仅重置 AI 分析结果（保留 content_md 题面），清空 AI 队列并可选重新入队分析
// 顺序必须是：暂停 → 清空队列 → 改 DB → 恢复暂停 → 再入队
// 若在暂停期间入队，消费者会把消息 Ack 丢掉（只剩碰巧在恢复后取出的少数任务）。
type resetMaintenancePayload struct {
	Requeue bool `json:"requeue"`
}

func resetMaintenanceRequeue(pending *model.AbilityMaintenancePending, created, requested bool) (bool, error) {
	if created || pending == nil || strings.TrimSpace(pending.Payload) == "" {
		return requested, nil
	}
	var payload resetMaintenancePayload
	if err := json.Unmarshal([]byte(pending.Payload), &payload); err != nil {
		return false, err
	}
	return payload.Requeue, nil
}

func (uc *ProblemUseCase) ResetAll(requeue bool) (reset, enqueued, purgedFetch, purgedAnalyze int, err error) {
	ctx := context.Background()
	requestedPayload, marshalErr := json.Marshal(resetMaintenancePayload{Requeue: requeue})
	if marshalErr != nil {
		err = marshalErr
		return
	}
	pending, created, pendingErr := ensureAbilityMaintenancePending(ctx, uc.data.DB, model.AbilityMaintenancePending{
		Scope: "global:reset", Operation: "reset", Payload: string(requestedPayload), TagsChanged: true, DifficultyChanged: true,
	})
	if pendingErr != nil {
		err = pendingErr
		return
	}
	requeue, err = resetMaintenanceRequeue(pending, created, requeue)
	if err != nil {
		return
	}
	pipelineControl.SetAnalyzePaused(true)
	if pending.Phase == "intent" {
		purgedAnalyze, err = uc.purgeAnalyzeQueue()
		if err != nil {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if err = advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "queue_purged"); err != nil {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
	}
	if pending.Phase == "fence_finalized" {
		claimed, claimErr := claimAbilityMaintenanceRelay(ctx, uc.data.DB, pending)
		if claimErr != nil {
			err = claimErr
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if !claimed {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		defer func() {
			if releaseErr := releaseAbilityMaintenanceRelay(context.Background(), uc.data.DB, pending); releaseErr != nil {
				log.Warnf("reset relay release scope=%s intent=%s: %v", pending.Scope, pending.OperationID, releaseErr)
			}
		}()
		if err = uc.publishAbilityMaintenanceTargets(ctx, pending); err != nil {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		var consumed bool
		consumed, err = uc.abilityMaintenanceTargetsConsumed(ctx, pending)
		if err != nil || !consumed {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		pipelineControl.SetAnalyzePaused(false)
		enqueued, err = uc.completeResetMaintenanceTail(ctx, pending, requeue, uc.enqueueAnalyze)
		return
	}
	profileToken, fenceErr := beginGlobalProfileInvalidationForIntent(ctx, uc.data.RDB, pending.OperationID)
	if fenceErr != nil {
		err = fenceErr
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	if claimErr := claimAbilityMaintenancePending(ctx, uc.data.DB, pending, profileToken.Owner); claimErr != nil {
		err = errors.Join(claimErr, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, profileToken))
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	workCtx := profileToken.Context()
	validate := func() error {
		return validateProfileInvalidation(workCtx, uc.data.RDB, profileGlobalGenerationKey, profileToken)
	}
	if validateErr := validate(); validateErr != nil {
		err = errors.Join(validateErr, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, profileToken))
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	abandon := func(cause error) error {
		return errors.Join(cause, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, profileToken))
	}
	if pending.Phase == "derived_ready" {
		if finishErr := FinishGlobalProfileInvalidation(workCtx, uc.data.RDB, profileToken); finishErr != nil {
			err = abandon(finishErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if err = advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "fence_finalized"); err != nil {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		claimed, claimErr := claimAbilityMaintenanceRelay(ctx, uc.data.DB, pending)
		if claimErr != nil {
			err = claimErr
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if !claimed {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		defer func() {
			if releaseErr := releaseAbilityMaintenanceRelay(context.Background(), uc.data.DB, pending); releaseErr != nil {
				log.Warnf("reset relay release scope=%s intent=%s: %v", pending.Scope, pending.OperationID, releaseErr)
			}
		}()
		if err = uc.publishAbilityMaintenanceTargets(ctx, pending); err != nil {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		var consumed bool
		consumed, err = uc.abilityMaintenanceTargetsConsumed(ctx, pending)
		if err != nil || !consumed {
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		pipelineControl.SetAnalyzePaused(false)
		enqueued, err = uc.completeResetMaintenanceTail(ctx, pending, requeue, uc.enqueueAnalyze)
		return
	}
	// 清除分析字段，保留题面 content_md；有题面的回到 TAGGING，无题面保持 PENDING
	if pending.Phase == "queue_purged" {
		reset, err = resetProblemFactsWithPending(workCtx, uc.data.DB, pending)
		if err != nil {
			err = abandon(err)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
	}
	if pending.Phase == "facts" {
		if uc.abilityStats == nil {
			err = abandon(fmt.Errorf("ResetAll: ability refresher unavailable"))
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		modelVersion, refreshErr := uc.refreshAbilityStatsForMaintenance(workCtx, pending)
		if refreshErr != nil {
			err = abandon(refreshErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if validateErr := validate(); validateErr != nil {
			err = abandon(validateErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		_ = modelVersion
	}
	if validateErr := validate(); validateErr != nil {
		err = abandon(validateErr)
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	if pending.Phase == "model_ready" {
		userIDs, listErr := uc.allCanonicalACUsers(workCtx)
		if listErr != nil {
			err = abandon(listErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if stageErr := prepareAbilityMaintenanceRebuildTargets(workCtx, uc.data.DB, pending, userIDs); stageErr != nil {
			err = abandon(stageErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
	}
	if pending.Phase == "targets_ready" {
		if rebuildErr := rebuildPendingAbilityMaintenanceTargets(workCtx, uc.data.DB, pending, validate, func(userID int64) error {
			return dal.RebuildUserTagACForUser(workCtx, uc.data.DB, userID)
		}); rebuildErr != nil {
			err = abandon(rebuildErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if validateErr := validate(); validateErr != nil {
			err = abandon(validateErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
		if stageErr := stageRebuiltAbilityMaintenanceTargets(workCtx, uc.data.DB, pending); stageErr != nil {
			err = abandon(stageErr)
			pipelineControl.SetAnalyzePaused(false)
			return
		}
	}
	if finishErr := FinishGlobalProfileInvalidation(workCtx, uc.data.RDB, profileToken); finishErr != nil {
		err = abandon(finishErr)
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	if phaseErr := advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "fence_finalized"); phaseErr != nil {
		err = phaseErr
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	claimed, claimErr := claimAbilityMaintenanceRelay(ctx, uc.data.DB, pending)
	if claimErr != nil {
		err = claimErr
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	if !claimed {
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	defer func() {
		if releaseErr := releaseAbilityMaintenanceRelay(context.Background(), uc.data.DB, pending); releaseErr != nil {
			log.Warnf("reset relay release scope=%s intent=%s: %v", pending.Scope, pending.OperationID, releaseErr)
		}
	}()
	if relayErr := uc.publishAbilityMaintenanceTargets(ctx, pending); relayErr != nil {
		err = relayErr
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	consumed, consumedErr := uc.abilityMaintenanceTargetsConsumed(ctx, pending)
	if consumedErr != nil {
		err = consumedErr
		pipelineControl.SetAnalyzePaused(false)
		return
	}
	if !consumed {
		pipelineControl.SetAnalyzePaused(false)
		return
	}

	// 先恢复再入队，避免 paused 期间消息被 Ack 丢弃
	pipelineControl.SetAnalyzePaused(false)
	enqueued, err = uc.completeResetMaintenanceTail(ctx, pending, requeue, uc.enqueueAnalyze)
	return
}

func (uc *ProblemUseCase) completeResetMaintenanceTail(ctx context.Context, pending *model.AbilityMaintenancePending, requeue bool, enqueue func(uint) error) (int, error) {
	enqueued := 0
	if requeue {
		// 批量回写 last_submitted_at ← submit_logs.MAX(time)
		updateLastSubmittedSQL := `
			UPDATE problems p
			SET last_submitted_at = s.mx
			FROM (
				SELECT problem_id, MAX(time) AS mx
				FROM submit_logs
				WHERE problem_id IS NOT NULL AND problem_id <> 0
				GROUP BY problem_id
			) s
			WHERE p.id = s.problem_id
			  AND (p.last_submitted_at IS NULL OR p.last_submitted_at < s.mx)
		`
		if uc.data.DB.Dialector.Name() != "postgres" {
			updateLastSubmittedSQL = `
				UPDATE problems
				SET last_submitted_at = (
					SELECT MAX(submit_logs.time) FROM submit_logs WHERE submit_logs.problem_id = problems.id
				)
				WHERE EXISTS (SELECT 1 FROM submit_logs WHERE submit_logs.problem_id = problems.id)
			`
		}
		if err := uc.data.DB.WithContext(ctx).Exec(updateLastSubmittedSQL).Error; err != nil {
			return enqueued, err
		}

		// 仅：有题面 + TAGGING + submit_logs 近 6 月有提交（禁止 NULL 虚入队）
		cutoff := time.Now().Add(-backfillWindow)
		recentClause, recentArgs := sqlHasRecentSubmit(cutoff)
		var list []model.Problem
		q := uc.data.DB.Where("status = ?", model.ProblemStatusTagging).
			Where("content_md IS NOT NULL AND content_md != ''").
			Where(recentClause, recentArgs...).
			Order("last_submitted_at DESC NULLS LAST, id DESC")
		if err := q.WithContext(ctx).Find(&list).Error; err != nil {
			return enqueued, err
		}
		for _, p := range list {
			if enqueue == nil {
				return enqueued, fmt.Errorf("ResetAll: analyze publisher unavailable")
			}
			if err := enqueue(p.ID); err != nil {
				return enqueued, err
			}
			enqueued++
		}
		log.Infof("ResetAll: analyze_enqueued=%d (enqueue after unpause)", enqueued)
	}
	if err := uc.completeAbilityMaintenanceTargets(ctx, pending); err != nil {
		return enqueued, err
	}
	return enqueued, nil
}

func resetProblemFacts(ctx context.Context, db *gorm.DB) (reset int, err error) {
	pending, _, err := ensureAbilityMaintenancePending(ctx, db, model.AbilityMaintenancePending{
		Scope: "global:reset", Operation: "reset", TagsChanged: true, DifficultyChanged: true,
	})
	if err != nil {
		return 0, err
	}
	if err := claimAbilityMaintenancePending(ctx, db, pending, uuid.NewString()); err != nil {
		return 0, err
	}
	return resetProblemFactsWithPending(ctx, db, pending)
}

func resetProblemFactsWithPending(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) (reset int, err error) {
	expected := *pending
	txPending := expected
	err = problemFactsTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		res := tx.Model(&model.Problem{}).
			Where("status IN ?", []string{model.ProblemStatusCompleted, model.ProblemStatusTagging, model.ProblemStatusFailed}).
			Where("content_md IS NOT NULL AND content_md != ''").
			Updates(map[string]interface{}{
				"status": model.ProblemStatusTagging, "problem_type": "", "difficulty": "",
				"tags": model.StringArray{}, "solutions_meta": model.SolutionsMeta{}, "error_msg": "",
			})
		if res.Error != nil {
			return res.Error
		}
		reset = int(res.RowsAffected)
		res2 := tx.Model(&model.Problem{}).
			Where("status IN ?", []string{model.ProblemStatusFailed, model.ProblemStatusFetching}).
			Where("(content_md IS NULL OR content_md = '')").
			Updates(map[string]interface{}{"status": model.ProblemStatusPending, "error_msg": ""})
		if res2.Error != nil {
			return res2.Error
		}
		reset += int(res2.RowsAffected)
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.ProblemTag{}).Error; err != nil {
			return err
		}
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.UserTagACSnapshot{}).Error; err != nil {
			return err
		}
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.UserTagAC{}).Error; err != nil {
			return err
		}
		return markAbilityMaintenanceFacts(ctx, tx, &txPending, true, true)
	})
	if err == nil {
		*pending = txPending
		return reset, nil
	}
	stored, loadErr := loadAbilityMaintenancePending(context.Background(), db, expected.Scope)
	if loadErr != nil {
		return reset, errors.Join(err, loadErr)
	}
	if stored != nil && stored.OperationID == expected.OperationID && stored.Phase == "facts" && stored.Revision == expected.Revision+1 {
		*pending = *stored
		return reset, nil
	}
	return reset, err
}

func truncateErr(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// handleFetchError 爬取失败：瞬时错误退避重试，持续超 24h 或非瞬时满 3 次 → FAILED_PERM。
// 禁止在 worker 内长 Sleep：4 并发被 2～10 分钟 sleep 占满会导致整条「获取题面中」假死。
// 瞬时失败：写 FAILED 后 return nil（Ack 释放 worker），后台 goroutine 到点再入队。
func (uc *ProblemUseCase) handleFetchError(p *model.Problem, err error) error {
	msg := truncateErr(err.Error())
	attempts := p.FetchAttempts + 1
	st := model.ProblemStatusFailed
	updates := map[string]interface{}{
		"fetch_attempts": attempts,
		"error_msg":      msg,
	}

	// QOJ 403 = 无权限：直接永久失效（即使文案仍是旧的 "status 403"）
	if isPermanentFetchError(msg) || isQOJFailedForbidden(&model.Problem{Platform: p.Platform, ErrorMsg: msg}) {
		if isQOJFailedForbidden(&model.Problem{Platform: p.Platform, ErrorMsg: msg}) {
			msg = "QOJ 无权限访问题面(403)"
			updates["error_msg"] = msg
		}
		st = model.ProblemStatusFailedPerm
		updates["status"] = st
		updates["fetch_fail_since"] = nil
		_ = uc.data.DB.Model(p).Updates(updates).Error
		return nil
	}

	if isTransientFetchError(msg) {
		// 记录首次瞬时失败时间
		failSince := p.FetchFailSince
		if failSince == nil {
			now := time.Now()
			failSince = &now
			updates["fetch_fail_since"] = now
		}
		if time.Since(*failSince) >= transientFailWindow {
			st = model.ProblemStatusFailedPerm
			msg = fmt.Sprintf("瞬时失败超过24小时: %s", msg)
			updates["status"] = st
			updates["error_msg"] = truncateErr(msg)
			updates["fetch_fail_since"] = nil
			_ = uc.data.DB.Model(p).Updates(updates).Error
			return nil
		}
		wait := transientBackoff(attempts)
		msg = fmt.Sprintf("瞬时失败(退避%v, 自%s起可重试至24h): %s",
			wait.Round(time.Second), failSince.Format("01-02 15:04"), msg)
		updates["status"] = st
		updates["error_msg"] = truncateErr(msg)
		_ = uc.data.DB.Model(p).Updates(updates).Error
		log.Warnf("problem %d fetch transient, schedule requeue after %v: %s", p.ID, wait, msg)
		// 异步延迟再入队，不占消费并发
		uc.scheduleFetchRetry(p.ID, p.Platform, p.ExternalID, p.URL, wait)
		return nil
	}

	// 普通可恢复错误：满 3 次 → 永久
	if attempts >= maxFetchAttempts {
		st = model.ProblemStatusFailedPerm
		msg = fmt.Sprintf("爬取失败超过%d次: %s", maxFetchAttempts, msg)
		updates["status"] = st
		updates["error_msg"] = truncateErr(msg)
		updates["fetch_fail_since"] = nil
		_ = uc.data.DB.Model(p).Updates(updates).Error
		return nil
	}
	updates["status"] = st
	_ = uc.data.DB.Model(p).Updates(updates).Error
	// 短退避后异步重入，避免立刻热循环占满 MQ
	uc.scheduleFetchRetry(p.ID, p.Platform, p.ExternalID, p.URL, 5*time.Second)
	return nil
}

// scheduleFetchRetry 延迟后重新入队题面爬取（仅当仍无题面且状态可爬）。
func (uc *ProblemUseCase) scheduleFetchRetry(id uint, platform, externalID, problemURL string, wait time.Duration) {
	if uc == nil || id == 0 {
		return
	}
	if wait < 0 {
		wait = 0
	}
	// 封顶，防止异常 wait 占死 goroutine 太久
	if wait > 15*time.Minute {
		wait = 15 * time.Minute
	}
	go func() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		<-timer.C
		if pipelineControl.IsFetchPaused() {
			return
		}
		var cur model.Problem
		if err := uc.data.DB.Select("id, status, content_md, platform, external_id, url").
			First(&cur, id).Error; err != nil {
			return
		}
		if strings.TrimSpace(cur.ContentMD) != "" {
			return
		}
		switch cur.Status {
		case model.ProblemStatusFailed, model.ProblemStatusPending, model.ProblemStatusFetching:
			// ok
		case model.ProblemStatusFailedPerm:
			// 仅当已有比赛路径映射时才自动重试（后补比赛记录场景）
			if !strings.EqualFold(cur.Platform, spider.NowCoder) ||
				len(uc.nowcoderContestFetchURLs(cur.ExternalID, cur.ID)) == 0 {
				return
			}
		default:
			return
		}
		plat := firstNonEmpty(platform, cur.Platform)
		ext := firstNonEmpty(externalID, cur.ExternalID)
		u := firstNonEmpty(problemURL, cur.URL)
		// enqueueFetchPrio 会自动附带比赛页 + Force
		if e := uc.enqueueFetchPrio(id, plat, ext, u, mqPriorityIncremental); e != nil {
			log.Warnf("scheduleFetchRetry enqueue id=%d: %v", id, e)
		}
	}()
}

// transientBackoff 405/WAF 退避：30s → 1m → 2m → 5m → 10m（封顶）
func transientBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	switch {
	case attempts <= 1:
		return 30 * time.Second
	case attempts == 2:
		return time.Minute
	case attempts == 3:
		return 2 * time.Minute
	case attempts == 4:
		return 5 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// isTransientFetchError 瞬时/风控类错误：退避重试，满 24h 才升 FAILED_PERM
// 含：WAF、登录墙、DOM 未找到（常为权限壳/空页误判）等
// 注意：NowCoder「暂无访问权限」不是瞬时——立刻 FAILED_PERM；有 contest 路径时再爬
// 注意：CF「未找到题面」不是瞬时——200 且无 Cloudflare 仍缺 .problem-statement 即题目不存在
func isTransientFetchError(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	// 权限类已永久：勿被同句「请稍后重试」误判为瞬时
	if isNowCoderNoAccessError(msg) {
		return false
	}
	// CF 缺题面：与牛客权限壳不同，CF 无题面即题目不存在，不退避
	if strings.Contains(msg, "CF 未找到题面") {
		return false
	}
	// 历史误标永久失败的文案也当可退避（管理员重试 / 消费者再爬）
	if strings.Contains(msg, "未找到题面") ||
		strings.Contains(msg, "题面为空") {
		return true
	}
	return strings.Contains(msg, "Cloudflare") ||
		strings.Contains(msg, "请稍后重试") ||
		strings.Contains(msg, "WAF") ||
		strings.Contains(msg, "需要登录") ||
		strings.Contains(msg, "被拦截") ||
		strings.Contains(msg, "status 405") ||
		strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "status 429") ||
		strings.Contains(msg, "status 503") ||
		strings.Contains(msg, "status 502") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "瞬时失败")
}

// isNowCoderNoAccessError 题库页无权限（赛后 /acm/problem/{id} 常不可匿名访问）
// 立刻 FAILED_PERM 不退避；后补 contest_problems 后 ProcessFetch 走比赛页再给机会
func isNowCoderNoAccessError(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "暂无访问权限") ||
		strings.Contains(msg, "没有查看题目的权限")
}

// isPermanentFetchError 真正不可恢复：立刻 FAILED_PERM，不入退避窗口
// - NowCoder 题库无权限：立刻永久；有比赛页映射时 Force/hasContestPath 仍可再爬
// - CF 未找到题面：200 且无 Cloudflare 仍缺 .problem-statement → 题目不存在/不可见，立刻永久
// - QOJ 403 = 无权限：直接永久
// - 其余 DOM/空题面：走瞬时退避（满 24h 才永久）
func isPermanentFetchError(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	// QOJ 无权限：优先判定，避免被通用「status 403」瞬时规则吞掉
	if isQOJForbiddenError(msg) {
		return true
	}
	// 牛客题库无权限：立刻永久（不再 24h 退避）；contest 模式另开路径
	if isNowCoderNoAccessError(msg) {
		return true
	}
	// CF 缺题面：题目不存在，重试无意义，立刻永久（优先于通用「未找到题面」瞬时规则）
	if strings.Contains(msg, "CF 未找到题面") {
		return true
	}
	if isTransientFetchError(msg) {
		return false
	}
	// 仅硬错误立刻永久；DOM/空题面已归入瞬时
	permanent := []string{
		"无法解析 CF external_id",
		"力扣付费题/无公开题面",
		"LeetCode 缺少 titleSlug",
		"leetcode 题目不存在",
		"不支持的平台",
		"缺少题面 URL",
		"竞赛题无稳定题面 URL",
		"AtCoder 缺少 URL",
		"empty url",
		"JSON 无题面",
		"NowCoder 无稳定题面 URL",
		"NowCoder 缺少题面 URL",
	}
	for _, p := range permanent {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isQOJForbiddenError 错误文案本身已标明 QOJ 无权限/403
func isQOJForbiddenError(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "QOJ 无权限访问题面") {
		return true
	}
	// 带 QOJ 前缀的 status 403（新路径返回 "QOJ status 403" 时的兜底）
	if strings.Contains(msg, "QOJ") && strings.Contains(msg, "status 403") {
		return true
	}
	return false
}

// isACStatus 是否算通过（与 AC 数量统计同源）
// 覆盖：AC / OK / Accepted / 答案正确 / 通过 等
func isACStatus(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	u := strings.ToUpper(s)
	if u == "AC" || u == "OK" || u == "ACCEPT" || u == "ACCEPTED" {
		return true
	}
	if strings.Contains(u, "ACCEPT") { // Accepted, Partially Accepted 等 — 全 AC 平台通常不带 Partially
		// CF: OK；部分平台写 Accepted
		if strings.Contains(u, "PARTIAL") || strings.Contains(u, "部分") {
			return false
		}
		return true
	}
	// 中文（牛客等）
	if strings.Contains(s, "答案正确") || s == "通过" || strings.Contains(s, "完全正确") {
		return true
	}
	// AtCoder 等
	if u == "AC" || strings.HasPrefix(u, "AC ") {
		return true
	}
	return false
}

// sqlACStatusCond 生成 SQL 片段，col 为列名（可带表别名，如 s.status）
func sqlACStatusCond(col string) string {
	return `(` +
		`UPPER(` + col + `) IN ('AC','OK','ACCEPT','ACCEPTED')` +
		` OR (UPPER(` + col + `) LIKE '%ACCEPT%' AND UPPER(` + col + `) NOT LIKE '%PARTIAL%')` +
		` OR ` + col + ` LIKE '%答案正确%'` +
		` OR ` + col + ` = '通过'` +
		` OR ` + col + ` LIKE '%完全正确%'` +
		`)`
}

func mapSubmitStatus(s string) string {
	if isACStatus(s) {
		return "AC"
	}
	if strings.TrimSpace(s) == "" {
		return "NONE"
	}
	return "TRIED"
}

func rankStatus(s string) int {
	switch s {
	case "AC":
		return 3
	case "TRIED":
		return 2
	case "NONE":
		return 1
	default:
		return 0
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
