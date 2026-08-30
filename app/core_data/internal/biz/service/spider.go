package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spidermetrics"
	"cwxu-algo/app/core_data/task"

	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var platformWriteUnlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

const (
	// submitInsertBatchSize 批量 upsert；2c4g 上 300 比 500 更平滑
	submitInsertBatchSize = 300
	// globalCacheBumpMinInterval 1w 日活：组织 ver 更长节流，避免统计 thrash
	globalCacheBumpMinInterval = 2 * time.Minute

	// pendingVerdictRetryDelay 遇到 Judging/评测中后，延迟再做一次增量爬
	pendingVerdictRetryDelay = 5 * time.Minute
	// pendingVerdictMaxAge 仅对最近提交的 pending 触发重爬（赛后 system test 等）
	pendingVerdictMaxAge = 24 * time.Hour
	// pendingVerdictMaxRounds 单用户单平台连续重爬上限（5min×72≈6h），防 OJ 永久卡死
	pendingVerdictMaxRounds = 72
	// pendingVerdictScheduleTTL 调度占坑 TTL，略长于 delay，防并发叠多个 timer
	pendingVerdictScheduleTTL = pendingVerdictRetryDelay + time.Minute
	qojFullScoreRepairKey     = "historical-full-score"
	qojFullScoreRepairVersion = 1
)

type SpiderUseCase struct {
	data       *data.Data
	problem    *ProblemUseCase
	spiderTask *task.SpiderTask
}

// SubmitImportResult reports database changes made by ImportSubmitLogs.
// Inserted deliberately excludes verdict refreshes so page retries can expose
// an exact count of newly created submit rows.
type SubmitImportResult struct {
	Inserted  int64
	Refreshed int64
}

type ClientSyncPageImport struct {
	SessionID            string
	Restart              int32
	Page                 int32
	Digest               string
	FirstSubmitID        string
	RemoteCount          int32
	PerPage              int32
	InsertedBefore       int64
	ProcessedPagesBefore int32
	CompletionReason     string
	NextAvailableAt      int64
	CompletedAt          time.Time
	ExpiresAt            time.Time
}

type ClientSyncPageImportResult struct {
	PageInserted     int64
	Inserted         int64
	ProcessedPages   int32
	NextPage         int32
	FirstSubmitID    string
	RemoteCount      int32
	PerPage          int32
	CompletionReason string
	NextAvailableAt  int64
	Replayed         bool
	DigestMatched    bool
}

func (r SubmitImportResult) Changed() bool { return r.Inserted+r.Refreshed > 0 }

type submitImportOptions struct {
	repairRequired bool
	complete       bool
	needAll        bool
	clientPage     *ClientSyncPageImport
	clientResult   *ClientSyncPageImportResult
}

func NewSpiderUseCase(data *data.Data, problem *ProblemUseCase, spiderTask *task.SpiderTask) *SpiderUseCase {
	return &SpiderUseCase{
		data:       data,
		problem:    problem,
		spiderTask: spiderTask,
	}
}

// ForcePublishUserProfile exposes only the profile action required by direct
// platform-binding maintenance paths.
func (uc *SpiderUseCase) ForcePublishUserProfile(userID int64) error {
	if uc == nil || uc.problem == nil {
		return fmt.Errorf("user profile publisher unavailable")
	}
	return uc.problem.ForcePublishUserProfile(userID)
}

func (uc *SpiderUseCase) ForcePublishMaintenanceUserProfile(userID int64, intentID string) error {
	if uc == nil || uc.problem == nil {
		return fmt.Errorf("user profile publisher unavailable")
	}
	return uc.problem.ForcePublishMaintenanceUserProfile(userID, intentID)
}

// RelayAbilityMaintenanceTargets keeps platform cleanup on the same durable
// profile outbox path as problem maintenance.
func (uc *SpiderUseCase) RelayAbilityMaintenanceTargets(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if uc == nil || uc.problem == nil {
		return fmt.Errorf("user profile publisher unavailable")
	}
	return uc.problem.RelayAbilityMaintenanceTargets(ctx, pending)
}

// loadDataTimeout 单用户整次爬取上限，防止某平台挂死占满 worker 导致 spider 队列堆积
const loadDataTimeout = 8 * time.Minute

// ensureContestSem 后补比赛题目录的共享有界并发：
// 多用户比赛同步同时触发时不再无界起 goroutine（每场一个），全进程最多同时 4 个
var ensureContestSem = make(chan struct{}, 4)

// LoadData 加载数据。platform 非空时只抓该平台；空则抓全部已绑定平台。
// 无绑定平台时成功返回；有平台且全部失败则返回 error（consumer 可重试）。
// 仅在有新写入时失效缓存，避免空跑爬虫打穿 period/heatmap 缓存。
func (uc *SpiderUseCase) LoadData(userId int64, needAll bool, platform string) error {
	if platform != "" && task.IsLegacyServerCrawlerDisabled(platform) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadDataTimeout)
	defer cancel()

	var platforms []model.Platform
	q := uc.data.DB.Where("user_id = ?", userId)
	if platform != "" {
		q = q.Where("platform = ?", platform)
	}
	if err := q.Find(&platforms).Error; err != nil {
		return err
	}
	if len(platforms) == 0 {
		return nil
	}
	filtered := platforms[:0]
	for _, plat := range platforms {
		if !task.IsLegacyServerCrawlerDisabled(plat.Platform) {
			filtered = append(filtered, plat)
		}
	}
	platforms = filtered
	if len(platforms) == 0 {
		return nil
	}

	var failCount int
	var lastErr error
	for _, plat := range platforms {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("load data timeout user=%d after partial: %w", userId, err)
		}
		_, err := uc.loadOnePlatform(ctx, userId, plat, needAll)
		if err != nil {
			failCount++
			lastErr = err
		}
	}
	if failCount == len(platforms) && lastErr != nil {
		// 单平台任务（MQ 按平台拆）不要说 all platforms，便于日志与 lastError 分类
		if len(platforms) == 1 {
			return fmt.Errorf("%s sync failed user=%d: %w", platforms[0].Platform, userId, lastErr)
		}
		return fmt.Errorf("%d/%d platforms failed user=%d: %w", failCount, len(platforms), userId, lastErr)
	}
	return nil
}

// fetchAndSave 拉取并写入提交；返回新插入行数。fetchCtx 为整次爬取超时（透传到 HTTP）
func (uc *SpiderUseCase) fetchAndSave(fetchCtx context.Context, userId int64, plat model.Platform, needAll, repairRequired bool) (int64, bool, error) {
	p, ok := spider.Get(plat.Platform)
	if !ok {
		return 0, false, fmt.Errorf("平台插件不存在")
	}
	sbFetch, ok := p.(spider.SubmitLogFetcher)
	if !ok {
		return 0, false, fmt.Errorf("平台未实现 SubmitLogFetcher")
	}

	// 拉取前记录代数；重绑会 BumpGeneration，写入前再比对，丢弃过期全量结果
	var genAtStart int64
	if uc.data != nil && uc.data.RDB != nil {
		var genErr error
		genAtStart, genErr = task.CurrentGeneration(fetchCtx, uc.data.RDB, userId, plat.Platform)
		if genErr != nil {
			return 0, false, fmt.Errorf("read binding generation: %w", genErr)
		}
	}

	var tmp []model.SubmitLog
	complete := false
	var err error
	if cf, ok := p.(spider.CompleteSubmitLogFetcher); ok {
		tmp, complete, err = cf.FetchSubmitLogComplete(fetchCtx, userId, plat.Username, needAll)
	} else {
		tmp, err = sbFetch.FetchSubmitLog(fetchCtx, userId, plat.Username, needAll)
	}
	if err != nil {
		return 0, false, err
	}

	result, err := uc.importSubmitLogs(context.Background(), userId, plat.Platform, genAtStart, tmp, submitImportOptions{
		repairRequired: repairRequired,
		complete:       complete,
		needAll:        needAll,
	})
	if err != nil {
		// 重绑时旧服务端 fetch 延续历史语义：静默丢弃，避免把正常交接
		// 记录成 OJ 失败或触发额外重试。浏览器同步在 service 边界单独
		// 映射为稳定 SESSION_EXPIRED。
		if kratoserrors.Reason(err) == "SYNC_BINDING_CHANGED" {
			log.Infof("Spider: discard stale fetch user=%d platform=%s", userId, plat.Platform)
			return 0, false, nil
		}
		return 0, false, err
	}
	return result.Inserted + result.Refreshed, complete, nil
}

// ImportSubmitLogs is the shared submit ingestion path used by server-side
// spiders and browser-local sync. The caller must pass the binding generation
// captured when its work/session started.
func (uc *SpiderUseCase) ImportSubmitLogs(ctx context.Context, userID int64, platform string, generation int64, logs []model.SubmitLog) (SubmitImportResult, error) {
	return uc.importSubmitLogs(ctx, userID, platform, generation, logs, submitImportOptions{})
}

// ImportClientSyncPage durably records the page result in the same SQL
// transaction as new submit rows, aggregates, and the optional final
// checkpoint. Redis can then be retried without losing page counters.
func (uc *SpiderUseCase) ImportClientSyncPage(ctx context.Context, userID int64, platform string, generation int64, logs []model.SubmitLog, page ClientSyncPageImport) (ClientSyncPageImportResult, error) {
	var pageResult ClientSyncPageImportResult
	_, err := uc.importSubmitLogs(ctx, userID, platform, generation, logs, submitImportOptions{
		clientPage: &page, clientResult: &pageResult,
	})
	return pageResult, err
}

// CompleteClientSync advances the browser checkpoint under the same
// user/platform lock and generation guard as submit writes.
func (uc *SpiderUseCase) CompleteClientSync(ctx context.Context, userID int64, platform string, generation int64, head string, completedAt time.Time) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return fmt.Errorf("submit importer is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	unlock, locked := uc.tryPlatformWriteLock(ctx, userID, platform)
	if !locked {
		return fmt.Errorf("平台写入锁占用 user=%d platform=%s", userID, platform)
	}
	defer unlock()
	if uc.data.RDB != nil {
		current, err := task.CurrentGeneration(ctx, uc.data.RDB, userID, platform)
		if err != nil {
			return kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
		}
		if current != generation {
			return kratoserrors.Conflict("SYNC_BINDING_CHANGED", "洛谷绑定已变化，请重新开始同步")
		}
	}
	result := uc.data.DB.WithContext(ctx).Model(&model.Platform{}).
		Where("user_id = ? AND platform = ?", userID, platform).
		Updates(map[string]interface{}{
			"client_sync_head_submit_id": strings.TrimSpace(head),
			"client_sync_completed_at":   completedAt.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return kratoserrors.Conflict("SYNC_BINDING_CHANGED", "洛谷绑定已变化，请重新开始同步")
	}
	return nil
}

// ScheduleSubmitPostProcess keeps browser imports on the existing problem
// binding path. It is safe to call more than once.
func (uc *SpiderUseCase) ScheduleSubmitPostProcess(userID int64) {
	if uc == nil || uc.problem == nil || userID <= 0 {
		return
	}
	go func() {
		if err := uc.problem.BindSubmitsAfterSpiderForPlatform(userID, spider.LuoGu); err != nil {
			log.Warnf("SpiderUseCase: bind browser-sync submits user=%d platform=%s: %v", userID, spider.LuoGu, err)
		}
	}()
}

func (uc *SpiderUseCase) importSubmitLogs(ctx context.Context, userId int64, platform string, generation int64, logs []model.SubmitLog, opts submitImportOptions) (SubmitImportResult, error) {
	var result SubmitImportResult
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return result, fmt.Errorf("submit importer is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	platform = strings.TrimSpace(platform)
	if userId <= 0 || platform == "" {
		return result, kratoserrors.BadRequest("INVALID_ARGUMENT", "提交归属无效")
	}

	// Own the slice before enforcing server-side identity and normalization.
	tmp := append([]model.SubmitLog(nil), logs...)
	for i := range tmp {
		tmp[i].UserID = userId
		tmp[i].Platform = platform
	}
	model.FillIsACBatch(tmp)
	model.NormalizeSubmitIDs(tmp)

	// 同用户同平台串行写入：FilterNew + Insert + ApplyDaily 必须原子视角，
	// 否则两次全量爬虫并发时都会把整批当成「新行」叠 daily/user_ac（重绑连点常见）。
	unlock, locked := uc.tryPlatformWriteLock(ctx, userId, platform)
	if !locked {
		return result, fmt.Errorf("平台写入锁占用 user=%d platform=%s", userId, platform)
	}
	defer unlock()

	if uc.data != nil && uc.data.RDB != nil {
		cur, genErr := task.CurrentGeneration(ctx, uc.data.RDB, userId, platform)
		if genErr != nil {
			return result, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
		}
		if cur != generation {
			return result, kratoserrors.Conflict("SYNC_BINDING_CHANGED", "洛谷绑定已变化，请重新开始同步")
		}
	}
	if opts.clientPage != nil {
		if opts.clientResult == nil || opts.clientPage.SessionID == "" || opts.clientPage.Page <= 0 || opts.clientPage.Digest == "" {
			return result, kratoserrors.BadRequest("INVALID_ARGUMENT", "同步页回执无效")
		}
		replayed, receipt, found, receiptErr := loadClientSyncPageReceipt(ctx, uc.data.DB, userId, platform, generation, *opts.clientPage)
		if receiptErr != nil {
			return result, receiptErr
		}
		if found {
			if effectErr := uc.applyClientSyncReceiptEffects(ctx, receipt); effectErr != nil {
				return result, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
			}
			page := *opts.clientPage
			progress := ClientSyncAuditProgress{SessionID: page.SessionID, ProcessedPages: replayed.ProcessedPages, RemoteCount: replayed.RemoteCount, Inserted: replayed.Inserted, RestartCount: page.Restart, UpdatedAt: page.CompletedAt}
			if auditErr := uc.UpdateClientSyncAudit(ctx, progress); auditErr != nil {
				log.Warnf("client-sync audit replay session=%s: %v", page.SessionID, auditErr)
			}
			*opts.clientResult = replayed
			return SubmitImportResult{Inserted: replayed.PageInserted}, nil
		}
	}
	if len(tmp) == 0 {
		if opts.clientPage != nil {
			return uc.persistClientSyncPage(ctx, userId, platform, generation, nil, idsFromSubmitLogs(tmp), tmp, opts)
		}
		if err := finishRepair(ctx, uc.data.DB, userId, platform, opts.repairRequired, opts.complete, nil); err != nil {
			return result, err
		}
		return result, nil
	}

	// submit_logs 允许同一平台提交被多个站内用户分别保存；FilterNewSubmitLogs
	// 会按当前用户去重，重复绑定不会阻断本次同步。
	ids := make([]string, 0, len(tmp))
	for i := range tmp {
		if tmp[i].SubmitID != "" {
			ids = append(ids, tmp[i].SubmitID)
		}
	}

	// 力扣：先清历史重复最近通过，再过滤待插入（同题只留一条）
	if platform == spider.LeetCode {
		if n, perr := dal.PruneLeetCodeProbDuplicates(ctx, uc.data.DB, userId); perr != nil {
			log.Warnf("Spider: prune leetcode prob dups user=%d: %v", userId, perr)
		} else if n > 0 {
			log.Infof("Spider: pruned %d duplicate leetcode recent-AC rows user=%d", n, userId)
		}
	}
	// 回写已入库的 pending/空状态（CF 评测中先入库后终态不会再进 FilterNew）
	nRefresh, rerr := dal.RefreshPendingSubmitVerdicts(ctx, uc.data.DB, tmp)
	if rerr != nil {
		return result, fmt.Errorf("refresh submit status user=%d platform=%s: %w", userId, platform, rerr)
	} else if nRefresh > 0 {
		log.Infof("Spider: refreshed pending status user=%d platform=%s n=%d", userId, platform, nRefresh)
	}
	result.Refreshed = nRefresh
	// 力扣：已入库 lc-prob 补 lang / 可读标题（早期 Lang="-"、LCR 乱码 slug）
	if platform == spider.LeetCode {
		if nMeta, merr := dal.RefreshLeetCodeProbMeta(ctx, uc.data.DB, tmp); merr != nil {
			log.Warnf("Spider: refresh leetcode meta user=%d: %v", userId, merr)
		} else if nMeta > 0 {
			log.Infof("Spider: refreshed leetcode lc-prob meta user=%d n=%d", userId, nMeta)
			result.Refreshed += nMeta
		}
	}

	// submit_logs 去重：已有 submit_id 不再累加预聚合（防全量重爬双计）
	neu, err := dal.FilterNewSubmitLogs(ctx, uc.data.DB, tmp)
	if err != nil {
		return result, err
	}
	if opts.clientPage != nil {
		return uc.persistClientSyncPage(ctx, userId, platform, generation, neu, ids, tmp, opts)
	}
	var inserted int64
	if len(neu) > 0 {
		// 异常大批量：多为首次全量或明细被清后重爬
		if len(neu) >= 2000 {
			log.Warnf("Spider: large new batch user=%d platform=%s fetched=%d new=%d needAll=%v",
				userId, platform, len(tmp), len(neu), opts.needAll)
		}

		// submit_logs 的全局唯一键是 owner claim 的数据库真相。必须先让真实
		// insert 成功，再对同一批行累计统计；跨用户竞态的 loser 会在唯一键处
		// 回滚，不能像 ON CONFLICT DO NOTHING 那样继续污染 loser 的聚合。
		err = uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.CreateInBatches(&neu, submitInsertBatchSize).Error; err != nil {
				return err
			}
			if err := dal.ApplyDailyDeltas(ctx, tx, dal.AggregateSubmitDeltas(neu)); err != nil {
				return err
			}
			if err := dal.ApplyUserACFromSubmits(ctx, tx, neu); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		inserted = int64(len(neu))
		result.Inserted = inserted
		spidermetrics.IncRows(platform, inserted)
	}

	// 赛后/评测中：本批仍有 Judging 等非终态 → 5 分钟后增量爬，直到没有为止
	uc.maybeSchedulePendingVerdictRetry(userId, platform, tmp)
	// Keep the write lock until the repair marker is durable so a concurrent rebind
	// cannot delete the old state and then have this stale fetch recreate it.
	if err := finishRepair(ctx, uc.data.DB, userId, platform, opts.repairRequired, opts.complete, nil); err != nil {
		return result, err
	}
	if result.Changed() && uc.data.RDB != nil {
		uc.invalidateCache(userId)
	}
	return result, nil
}

func hasSubmitOwnerConflict(ctx context.Context, db *gorm.DB, platform string, ids []string, userID int64) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	var count int64
	err := db.WithContext(ctx).Model(&model.SubmitLog{}).
		Where("platform = ? AND submit_id IN ? AND user_id <> ?", platform, ids, userID).
		Count(&count).Error
	return count > 0, err
}

func idsFromSubmitLogs(logs []model.SubmitLog) []string {
	ids := make([]string, 0, len(logs))
	for i := range logs {
		if logs[i].SubmitID != "" {
			ids = append(ids, logs[i].SubmitID)
		}
	}
	return ids
}

func loadClientSyncPageReceipt(ctx context.Context, db *gorm.DB, userID int64, platform string, generation int64, page ClientSyncPageImport) (ClientSyncPageImportResult, *model.ClientSyncPageReceipt, bool, error) {
	var receipt model.ClientSyncPageReceipt
	err := db.WithContext(ctx).Where("session_id = ? AND restart = ? AND page = ?", page.SessionID, page.Restart, page.Page).First(&receipt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ClientSyncPageImportResult{}, nil, false, nil
	}
	if err != nil {
		return ClientSyncPageImportResult{}, nil, false, err
	}
	if receipt.UserID != userID || receipt.Platform != platform || receipt.Generation != generation {
		return ClientSyncPageImportResult{}, nil, false, kratoserrors.Conflict("SYNC_BINDING_CHANGED", "洛谷绑定已变化，请重新开始同步")
	}
	result := clientSyncPageResult(receipt, true)
	result.DigestMatched = receipt.Digest == page.Digest
	return result, &receipt, true, nil
}

func clientSyncPageResult(receipt model.ClientSyncPageReceipt, replayed bool) ClientSyncPageImportResult {
	return ClientSyncPageImportResult{
		PageInserted: receipt.PageInserted, Inserted: receipt.Inserted,
		ProcessedPages: receipt.ProcessedPages, NextPage: receipt.NextPage,
		FirstSubmitID: receipt.FirstSubmitID, RemoteCount: receipt.RemoteCount,
		PerPage: receipt.PerPage, CompletionReason: receipt.CompletionReason,
		NextAvailableAt: receipt.NextAvailableAt, Replayed: replayed, DigestMatched: true,
	}
}

func (uc *SpiderUseCase) persistClientSyncPage(ctx context.Context, userID int64, platform string, generation int64, neu []model.SubmitLog, idsSource []string, fetched []model.SubmitLog, opts submitImportOptions) (SubmitImportResult, error) {
	var result SubmitImportResult
	page := *opts.clientPage
	ids := idsSource
	if len(ids) == 0 {
		ids = idsFromSubmitLogs(fetched)
	}
	var pageResult ClientSyncPageImportResult
	var persistedReceipt *model.ClientSyncPageReceipt
	created := false
	err := uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		replayed, receipt, found, err := loadClientSyncPageReceipt(ctx, tx, userID, platform, generation, page)
		if err != nil {
			return err
		}
		if found {
			pageResult = replayed
			persistedReceipt = receipt
			return nil
		}
		if conflict, err := hasSubmitOwnerConflict(ctx, tx, platform, ids, userID); err != nil {
			return err
		} else if conflict {
			return kratoserrors.Conflict("SUBMIT_OWNER_CONFLICT", "提交记录已属于其他用户")
		}
		if len(neu) > 0 {
			if err := tx.CreateInBatches(&neu, submitInsertBatchSize).Error; err != nil {
				return err
			}
			if err := dal.ApplyDailyDeltas(ctx, tx, dal.AggregateSubmitDeltas(neu)); err != nil {
				return err
			}
			if err := dal.ApplyUserACFromSubmits(ctx, tx, neu); err != nil {
				return err
			}
		}
		if page.CompletionReason != "" {
			updated := tx.Model(&model.Platform{}).
				Where("user_id = ? AND platform = ?", userID, platform).
				Updates(map[string]interface{}{
					"client_sync_head_submit_id": strings.TrimSpace(page.FirstSubmitID),
					"client_sync_completed_at":   page.CompletedAt.UTC(),
				})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return kratoserrors.Conflict("SYNC_BINDING_CHANGED", "洛谷绑定已变化，请重新开始同步")
			}
		}
		if err := uc.reserveClientSyncReceipt(ctx, tx, userID, platform, page, len(neu) > 0); err != nil {
			if errors.Is(err, errClientSyncReceiptLimit) {
				return kratoserrors.Conflict("LUOGU_RECORDS_CHANGED", "单次同步页数过多，请稍后重试")
			}
			return err
		}
		createdReceipt := model.ClientSyncPageReceipt{
			SessionID: page.SessionID, Restart: page.Restart, Page: page.Page, Digest: page.Digest,
			UserID: userID, Platform: platform, Generation: generation,
			PageInserted: int64(len(neu)), Inserted: page.InsertedBefore + int64(len(neu)),
			ProcessedPages: page.ProcessedPagesBefore + 1, NextPage: page.Page + 1,
			FirstSubmitID: page.FirstSubmitID, RemoteCount: page.RemoteCount, PerPage: page.PerPage,
			CompletionReason: page.CompletionReason, NextAvailableAt: page.NextAvailableAt,
			HasPending: countRecentPendingSubmits(fetched, pendingVerdictMaxAge) > 0,
			ExpiresAt:  page.ExpiresAt.UTC(),
		}
		if len(neu) == 0 && !createdReceipt.HasPending {
			now := time.Now().UTC()
			createdReceipt.EffectsAppliedAt = &now
		}
		if err := tx.Create(&createdReceipt).Error; err != nil {
			return err
		}
		pageResult = clientSyncPageResult(createdReceipt, false)
		persistedReceipt = &createdReceipt
		created = true
		return nil
	})
	if err != nil {
		replayed, receipt, found, reloadErr := loadClientSyncPageReceipt(ctx, uc.data.DB, userID, platform, generation, page)
		if reloadErr == nil && found {
			if effectErr := uc.applyClientSyncReceiptEffects(ctx, receipt); effectErr != nil {
				return result, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
			}
			*opts.clientResult = replayed
			return SubmitImportResult{Inserted: replayed.PageInserted}, nil
		}
		if reloadErr != nil {
			return result, reloadErr
		}
		conflict, conflictErr := hasSubmitOwnerConflict(ctx, uc.data.DB, platform, ids, userID)
		if conflictErr != nil {
			return result, conflictErr
		}
		if conflict {
			return result, kratoserrors.Conflict("SUBMIT_OWNER_CONFLICT", "提交记录已属于其他用户")
		}
		return result, err
	}
	// Audit is intentionally outside the submit/receipt transaction: audit
	// availability must never roll back an otherwise valid idempotent page.
	progress := ClientSyncAuditProgress{SessionID: page.SessionID, ProcessedPages: pageResult.ProcessedPages, RemoteCount: pageResult.RemoteCount, Inserted: pageResult.Inserted, RestartCount: page.Restart, UpdatedAt: page.CompletedAt}
	if auditErr := uc.UpdateClientSyncAudit(ctx, progress); auditErr != nil {
		log.Warnf("client-sync audit progress session=%s: %v", page.SessionID, auditErr)
	} else if pageResult.CompletionReason != "" {
		if auditErr := uc.TerminateClientSyncAudit(ctx, page.SessionID, "completed", pageResult.CompletionReason, "", "", page.CompletedAt); auditErr != nil {
			log.Warnf("client-sync audit completion session=%s: %v", page.SessionID, auditErr)
		}
	}
	*opts.clientResult = pageResult
	result.Inserted = pageResult.PageInserted
	if effectErr := uc.applyClientSyncReceiptEffects(ctx, persistedReceipt); effectErr != nil {
		return result, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	if created && result.Inserted > 0 {
		spidermetrics.IncRows(platform, result.Inserted)
	}
	return result, nil
}

// countRecentPendingSubmits 统计 maxAge 内仍为评测中的提交数
func countRecentPendingSubmits(logs []model.SubmitLog, maxAge time.Duration) int {
	if len(logs) == 0 || maxAge <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for i := range logs {
		if !model.IsPendingSubmitStatus(logs[i].Status) {
			continue
		}
		// 无时间戳时保守计入（避免漏掉赛后 system test）
		if logs[i].Time.IsZero() || logs[i].Time.After(cutoff) {
			n++
		}
	}
	return n
}

func pendingVerdictScheduleKey(userId int64, platform string) string {
	return fmt.Sprintf("spider:pending_retry:%d:%s", userId, platform)
}

func pendingVerdictRoundKey(userId int64, platform string) string {
	return fmt.Sprintf("spider:pending_retry_n:%d:%s", userId, platform)
}

// maybeSchedulePendingVerdictRetry 本批拉取仍有 Judging/评测中时，5 分钟后入队增量爬。
// 再次爬到终态则不再调度；仍有 pending 则继续，直到清空或达轮次上限。
func (uc *SpiderUseCase) maybeSchedulePendingVerdictRetry(userId int64, platform string, fetched []model.SubmitLog) {
	if uc == nil || userId <= 0 || platform == "" {
		return
	}
	n := countRecentPendingSubmits(fetched, pendingVerdictMaxAge)
	if n == 0 {
		// 评测已全部出结果：清连续重爬计数
		if uc.data != nil && uc.data.RDB != nil {
			_ = uc.data.RDB.Del(context.Background(), pendingVerdictRoundKey(userId, platform)).Err()
		}
		return
	}
	uc.schedulePendingVerdictRetry(userId, platform, n)
}

// pendingVerdictDueZKey Redis ZSET：member=userId:platform，score=到期 unix 秒。
// 进程重启后 cron 仍可扫出到期项，避免 time.AfterFunc 丢失。
const pendingVerdictDueZKey = "spider:pending_retry_due"

func pendingVerdictMember(userId int64, platform string) string {
	return fmt.Sprintf("%d:%s", userId, platform)
}

// schedulePendingVerdictRetry 用 Redis 占坑 + ZSET 到期表；cron 扫到期后 needAll=false 增量入队。
func (uc *SpiderUseCase) schedulePendingVerdictRetry(userId int64, platform string, pendingN int) {
	if uc.spiderTask == nil {
		return
	}
	ctx := context.Background()
	if uc.data == nil || uc.data.RDB == nil {
		// 无 Redis 时无法持久化调度，退化为进程内 timer（重启会丢）
		log.Warnf("Spider: pending-verdict no redis, AfterFunc fallback user=%d platform=%s", userId, platform)
		uid, plat := userId, platform
		time.AfterFunc(pendingVerdictRetryDelay, func() {
			res := uc.spiderTask.DoPlatform(uid, plat, false)
			log.Infof("Spider: pending-verdict retry enqueue (fallback) user=%d platform=%s published=%d",
				uid, plat, res.Published)
		})
		return
	}
	// 已有未触发的调度则跳过（同用户同平台只挂一个）
	ok, err := uc.data.RDB.SetNX(ctx, pendingVerdictScheduleKey(userId, platform), "1", pendingVerdictScheduleTTL).Result()
	if err != nil {
		log.Warnf("Spider: pending-verdict schedule SetNX user=%d platform=%s: %v", userId, platform, err)
	} else if !ok {
		log.Debugf("Spider: pending-verdict retry already scheduled user=%d platform=%s pending=%d",
			userId, platform, pendingN)
		return
	}
	// 占坑成功后再计轮次（OJ 永久卡死保护）
	rk := pendingVerdictRoundKey(userId, platform)
	round, err := uc.data.RDB.Incr(ctx, rk).Result()
	if err == nil {
		_ = uc.data.RDB.Expire(ctx, rk, pendingVerdictMaxAge).Err()
		if round > pendingVerdictMaxRounds {
			_ = uc.data.RDB.Del(ctx, pendingVerdictScheduleKey(userId, platform)).Err()
			log.Warnf("Spider: pending-verdict retry cap reached user=%d platform=%s rounds=%d pending=%d",
				userId, platform, round, pendingN)
			return
		}
	}
	dueAt := float64(time.Now().Add(pendingVerdictRetryDelay).Unix())
	member := pendingVerdictMember(userId, platform)
	if err := uc.data.RDB.ZAdd(ctx, pendingVerdictDueZKey, redis.Z{Score: dueAt, Member: member}).Err(); err != nil {
		log.Warnf("Spider: pending-verdict ZAdd user=%d platform=%s: %v", userId, platform, err)
		// ZAdd 失败仍保留 schedule key TTL，到期后自然释放；正确性靠周期爬兜底
		return
	}
	log.Infof("Spider: schedule pending-verdict retry user=%d platform=%s pending=%d after %v (redis zset)",
		userId, platform, pendingN, pendingVerdictRetryDelay)
}

// tryPlatformWriteLock 获取 user+platform 写入锁；短轮询等待，避免重绑后新任务与旧任务交接时直接失败。
// 返回 (unlock, ok)
func (uc *SpiderUseCase) tryPlatformWriteLock(ctx context.Context, userId int64, platform string) (func(), bool) {
	if uc.data == nil || uc.data.RDB == nil {
		return func() {}, true
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Warnf("Spider: writelock token error (allow): %v", err)
		return func() {}, true
	}
	token := hex.EncodeToString(tokenBytes)
	key := fmt.Sprintf("spider:writelock:%d:%s", userId, platform)
	const (
		waitStep = 2 * time.Second
		waitMax  = 60 * time.Second
	)
	deadline := time.Now().Add(waitMax)
	for {
		// 与 loadDataTimeout 同量级，防止进程崩溃后死锁
		ok, err := uc.data.RDB.SetNX(ctx, key, token, loadDataTimeout).Result()
		if err != nil {
			log.Warnf("Spider: writelock redis error (allow): %v", err)
			return func() {}, true
		}
		if ok {
			return func() {
				_ = platformWriteUnlockScript.Run(context.Background(), uc.data.RDB, []string{key}, token).Err()
			}, true
		}
		if time.Now().After(deadline) {
			return func() {}, false
		}
		select {
		case <-ctx.Done():
			return func() {}, false
		case <-time.After(waitStep):
		}
	}
}

// fetchAndSaveContest 拉取并写入比赛记录；返回是否有写入尝试（Save 无法可靠区分 RowsAffected，有数据即视为可能变更）
// ctx 与 loadDataTimeout 对齐：超时后不再发起 OJ 拉取（平台插件暂无透传 ctx，至少在入口尊重取消）。
func (uc *SpiderUseCase) fetchAndSaveContest(ctx context.Context, userId int64, plat model.Platform, needAll bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("contest fetch cancelled: %w", err)
	}
	p, ok := spider.Get(plat.Platform)
	if !ok {
		return false, fmt.Errorf("平台插件不存在")
	}
	sbFetch, ok := p.(spider.SubmitContestFetcher)
	if !ok {
		return false, fmt.Errorf("平台未实现 SubmitContestFetcher")
	}
	tmp, err := sbFetch.FetchContestLog(userId, plat.Username, needAll)
	if err != nil {
		return false, err
	}
	if len(tmp) == 0 {
		return false, nil
	}

	// 冲突时更新：唯一键 (platform, user_id, contest_id)，避免力扣与其它平台 contest_id 撞号互相覆盖
	err = uc.data.DB.
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "platform"}, {Name: "user_id"}, {Name: "contest_id"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				// 有真实排名才覆盖；否则保留旧值（站内榜可对 rank=0 按 AC 模拟）
				"rank":        gorm.Expr("CASE WHEN EXCLUDED.rank > 0 THEN EXCLUDED.rank ELSE contest_logs.rank END"),
				"ac_count":    gorm.Expr("GREATEST(contest_logs.ac_count, EXCLUDED.ac_count)"),
				"total_count": gorm.Expr("GREATEST(contest_logs.total_count, EXCLUDED.total_count)"),
				"contest_name": gorm.Expr(
					"CASE WHEN EXCLUDED.contest_name <> '' THEN EXCLUDED.contest_name ELSE contest_logs.contest_name END",
				),
				"contest_url": gorm.Expr(
					"CASE WHEN EXCLUDED.contest_url <> '' THEN EXCLUDED.contest_url ELSE contest_logs.contest_url END",
				),
				"time": gorm.Expr(
					"CASE WHEN EXCLUDED.time > TIMESTAMP '1970-01-02' THEN EXCLUDED.time ELSE contest_logs.time END",
				),
			}),
		}).
		CreateInBatches(&tmp, submitInsertBatchSize).Error
	if err != nil {
		return true, err
	}
	// 牛客：用参赛历史/比赛页实时 start+end 写入日历（各场真实赛长，非固定 3h）
	// history 有 end 的场次全量写入；缺 end 再限量抓页
	if NormalizeCalendarPlatform(plat.Platform) == spider.NowCoder {
		pageCap := 30
		if needAll {
			pageCap = 80
		}
		ensureNowCoderCalendarsFromContestLogs(uc.data.DB, tmp, pageCap)
	}
	// 题级明细（XCPCIO 格子）；失败不阻断场级写入
	detailOK := false
	if df, ok := p.(spider.ContestDetailFetcher); ok {
		if cells, dErr := df.FetchContestDetails(userId, plat.Username, needAll); dErr != nil {
			log.Warnf("Spider: FetchContestDetails %s %s: %v", plat.Platform, plat.Username, dErr)
		} else if len(cells) > 0 {
			if sErr := uc.saveContestUserProblems(userId, plat.Platform, cells); sErr != nil {
				log.Warnf("Spider: saveContestUserProblems %s %s: %v", plat.Platform, plat.Username, sErr)
			} else {
				detailOK = true
			}
		}
	}
	// 原生无明细 / 失败：按「题目集 ∩ 时间窗 ∩ 提交」反推（牛客/力扣补洞/全平台兜底）
	// 无论是否有原生明细，都回填赛后补题（UPSOLVE，不计分）
	if uc.data != nil && uc.data.DB != nil {
		// 最近若干场，避免全量历史扫爆
		limit := 15
		if needAll {
			limit = 40
		}
		n := 0
		for _, cl := range tmp {
			if n >= limit {
				break
			}
			if cl.ContestId == "" {
				continue
			}
			if !detailOK {
				if _, iErr := InferContestUserProblemsForUser(uc.data.DB, plat.Platform, cl.ContestId, userId, cl.Time); iErr != nil {
					log.Warnf("Spider: InferContestUserProblems %s %s %s: %v", plat.Platform, plat.Username, cl.ContestId, iErr)
				}
			}
			if _, uErr := InferContestUpsolvesForUser(uc.data.DB, plat.Platform, cl.ContestId, userId, cl.Time); uErr != nil {
				log.Warnf("Spider: InferContestUpsolves %s %s %s: %v", plat.Platform, plat.Username, cl.ContestId, uErr)
			}
			n++
		}
	}
	// 后补比赛记录：异步 ensure 题目录 + 对无题面强制再爬（牛客走比赛路径）
	// 解决「先爬提交→题面失败/永久失败，后才有比赛记录」
	if uc.problem != nil {
		seen := map[string]struct{}{}
		capN := 12
		if needAll {
			capN = 25
		}
		n := 0
		for _, cl := range tmp {
			if n >= capN {
				break
			}
			cid := strings.TrimSpace(cl.ContestId)
			if cid == "" {
				continue
			}
			if _, ok := seen[cid]; ok {
				continue
			}
			seen[cid] = struct{}{}
			n++
			pName, cID := plat.Platform, cid
			go func() {
				// 共享信号量限并发，防止一次同步冲出几十个 ensure goroutine
				ensureContestSem <- struct{}{}
				defer func() { <-ensureContestSem }()
				if _, e := uc.problem.EnsureContestProblemsOnce(pName, cID); e != nil {
					log.Warnf("Spider: ensure contest after log %s/%s: %v", pName, cID, e)
				}
				// EnsureOnce 内部对 done 也会 RequeueMissing；此处再兜一层
				if m := uc.problem.RequeueMissingContestProblemFetches(pName, cID); m > 0 {
					log.Infof("Spider: requeue missing problem content %s/%s n=%d", pName, cID, m)
				}
			}()
		}
	}
	return true, nil
}

// saveContestUserProblems 将题级格子 UPSERT 进 contest_user_problems。
// 按场次合并已有 UPSOLVE，避免原生明细把补题格降级成 TRIED。
func (uc *SpiderUseCase) saveContestUserProblems(userId int64, platform string, cells []spider.ContestProblemCell) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil || len(cells) == 0 {
		return nil
	}
	platform = strings.TrimSpace(platform)
	byContest := map[string][]model.ContestUserProblem{}
	for _, c := range cells {
		if c.ContestID == "" || c.ExternalID == "" {
			continue
		}
		st := strings.TrimSpace(c.Status)
		if st == "" {
			continue
		}
		byContest[c.ContestID] = append(byContest[c.ContestID], model.ContestUserProblem{
			Platform:    platform,
			ContestID:   c.ContestID,
			UserID:      userId,
			Label:       c.Label,
			ExternalID:  c.ExternalID,
			Status:      st,
			Attempts:    c.Attempts,
			FirstACAt:   c.FirstACAt,
			RelativeSec: c.RelativeSec,
			ScoreDelta:  c.ScoreDelta,
		})
	}
	for cid, rows := range byContest {
		rows = mergeContestCellUpserts(uc.data.DB, platform, cid, rows)
		if len(rows) == 0 {
			continue
		}
		if err := uc.data.DB.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "platform"}, {Name: "contest_id"}, {Name: "user_id"}, {Name: "external_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"label", "status", "attempts", "first_ac_at", "relative_sec", "score_delta", "updated_at",
			}),
		}).CreateInBatches(&rows, 100).Error; err != nil {
			return err
		}
	}
	return nil
}

// fetchAndSaveRating 抓取并写回 platforms.rating（失败只打日志，不阻断提交/比赛同步）
func (uc *SpiderUseCase) fetchAndSaveRating(plat model.Platform) {
	p, ok := spider.Get(plat.Platform)
	if !ok {
		return
	}
	rf, ok := p.(spider.RatingFetcher)
	if !ok {
		return // 平台未实现 rating（如 QOJ）
	}
	rating, has, err := rf.FetchRating(plat.Username)
	if err != nil {
		log.Warnf("Spider: FetchRating %s %s: %v", plat.Platform, plat.Username, err)
		return
	}
	upd := map[string]interface{}{
		"rating":     rating,
		"has_rating": has,
	}
	if !has {
		upd["rating"] = 0
	}
	if err := uc.data.DB.Model(&model.Platform{}).
		Where("user_id = ? AND platform = ?", plat.UserID, plat.Platform).
		Updates(upd).Error; err != nil {
		log.Warnf("Spider: save rating %s %s: %v", plat.Platform, plat.Username, err)
		return
	}
	if has {
		log.Infof("Spider: rating %s %s = %d", plat.Platform, plat.Username, rating)
	} else {
		log.Infof("Spider: rating %s %s = (none)", plat.Platform, plat.Username)
	}
}

// recordOjStatus 回写 OJ 登录状态到 site_configs（仅 LuoGu/QOJ 需要登录）
func (uc *SpiderUseCase) recordOjStatus(ctx context.Context, platform, status, errMsg string) {
	if platform != spider.LuoGu && platform != spider.QOJ {
		return
	}
	if uc.data == nil || uc.data.UserDB == nil {
		return
	}
	sitesettings.UpdateOjStatus(ctx, uc.data.RDB, uc.data.UserDB, platform, status, errMsg)
}

// injectOjCredentials 从站点设置读取 OJ 凭证并注入到爬虫客户端
func (uc *SpiderUseCase) injectOjCredentials(ctx context.Context) {
	if uc.data == nil {
		return
	}
	rt := sitesettings.Load(ctx, uc.data.RDB, nil)
	var luoguSetter, qojSetter interface{ SetCredentials(string, string) }
	if p, ok := spider.Get(spider.LuoGu); ok {
		if setter, ok := p.(interface{ SetCredentials(string, string) }); ok {
			luoguSetter = setter
		}
	}
	if p, ok := spider.Get(spider.QOJ); ok {
		if setter, ok := p.(interface{ SetCredentials(string, string) }); ok {
			qojSetter = setter
		}
	}
	applyOjCredentials(rt, luoguSetter, qojSetter)
}

func applyOjCredentials(rt *sitesettings.Runtime, luogu, qoj interface{ SetCredentials(string, string) }) {
	if rt == nil {
		rt = &sitesettings.Runtime{}
	}
	if luogu != nil {
		if setter, ok := luogu.(interface {
			SetCredentialsVersioned(string, string, int64)
		}); ok {
			setter.SetCredentialsVersioned(rt.OjLuoguUsername, rt.OjLuoguPassword, rt.ConfigVersion)
		} else {
			luogu.SetCredentials(rt.OjLuoguUsername, rt.OjLuoguPassword)
		}
	}
	if qoj != nil {
		qoj.SetCredentials(rt.OjQojUsername, rt.OjQojPassword)
	}
}

func (uc *SpiderUseCase) hasProfileRebuildAfterBindingMarker(userID int64, platform string) bool {
	if uc == nil || uc.data == nil || uc.data.RDB == nil || userID <= 0 || strings.TrimSpace(platform) == "" {
		return false
	}
	marked, err := task.HasProfileRebuildAfterBinding(context.Background(), uc.data.RDB, userID, platform)
	if err != nil {
		log.Warnf("Spider: read profile rebind marker user=%d platform=%s: %v", userID, platform, err)
		return false
	}
	return marked
}

// loadOnePlatform 返回 (是否有数据变更, error)
func (uc *SpiderUseCase) loadOnePlatform(ctx context.Context, userId int64, plat model.Platform, needAll bool) (bool, error) {
	// Rating 是平台公开基础数据，独立于提交记录同步、题面抓取和站点
	// 登录账号；先执行，后续提交链路失败也不能影响 Rating 更新。
	uc.fetchAndSaveRating(plat)
	uc.injectOjCredentials(ctx)
	var repairRequired bool
	if plat.Platform == spider.QOJ {
		var err error
		needAll, err = forceRepairFetch(ctx, uc.data.DB, userId, plat.Platform, needAll)
		if err != nil {
			return false, err
		}
		repairRequired = needAll
	}
	// needAll 全量：最多 3 次（原先 12 次会把 worker 占死、队列堆积）
	maxRetries := 1
	if needAll {
		maxRetries = 3
	}
	var anyChange bool
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		rows, _, err := uc.fetchAndSave(ctx, userId, plat, needAll, repairRequired)
		if rows > 0 {
			anyChange = true
		}
		if err := ctx.Err(); err != nil {
			return anyChange, fmt.Errorf("load platform timeout user=%d platform=%s: %w", userId, plat.Platform, err)
		}
		if contestChanged, contestErr := uc.fetchAndSaveContest(ctx, userId, plat, needAll); contestErr != nil {
			log.Errorf("Spider: fetchAndSaveContest %s %s 失败: %v", plat.Platform, plat.Username, contestErr)
		} else if contestChanged {
			anyChange = true
			// Contest writes happen after submit ingestion. Always bump once more so
			// a reader cannot refill the newly-versioned cache in the submit/contest
			// gap and then keep stale contest data.
			uc.invalidateAfterContestWrite(userId, true)
		}
		if err == nil {
			log.Infof("Spider: %s %s 成功 new_rows=%d", plat.Platform, plat.Username, rows)
			if (anyChange || uc.hasProfileRebuildAfterBindingMarker(userId, plat.Platform)) && uc.problem != nil {
				// 异步绑定，避免在 spider worker 内串行 resolve 拖垮队列。
				// 只有对应 OJ 的换绑标记才会在绑定完成后触发强制画像。
				uid, platform := userId, plat.Platform
				go func() {
					if err := uc.problem.BindSubmitsAfterSpiderForPlatform(uid, platform); err != nil {
						log.Warnf("Spider: bind submits after crawl user=%d platform=%s: %v", uid, platform, err)
					}
				}()
			}
			uc.recordOjStatus(ctx, plat.Platform, "ok", "")
			return anyChange, nil
		}
		lastErr = err
		if errors.Is(err, spider.ErrEmptyPlatformUsername) {
			return anyChange, err
		}
		uc.recordOjStatus(ctx, plat.Platform, "fail", err.Error())
		if strings.Contains(err.Error(), "平台") {
			log.Errorf(
				"Spider: %s %s 失败: %v",
				plat.Platform,
				plat.Username,
				err,
			)
			return anyChange, err
		}
		log.Errorf(
			"Spider: %s %s 失败 (重试 %d/%d): %v",
			plat.Platform,
			plat.Username,
			i+1,
			maxRetries,
			err,
		)
		if !needAll || i+1 >= maxRetries {
			return anyChange, err
		}
		time.Sleep(3 * time.Second)
	}
	if lastErr != nil {
		return anyChange, lastErr
	}
	return anyChange, fmt.Errorf("platform %s max retries exceeded", plat.Platform)
}

func (uc *SpiderUseCase) invalidateAfterContestWrite(userID int64, changed bool) {
	if changed {
		uc.invalidateCache(userID)
	}
}

func forceRepairFetch(ctx context.Context, db *gorm.DB, userID int64, platform string, requestedAll bool) (bool, error) {
	if platform != spider.QOJ {
		return requestedAll, nil
	}
	var state model.SpiderRepairState
	err := db.WithContext(ctx).Where("user_id = ? AND platform = ? AND repair_key = ?", userID, platform, qojFullScoreRepairKey).First(&state).Error
	if err == nil {
		return requestedAll || state.Version < qojFullScoreRepairVersion, nil
	}
	if err == gorm.ErrRecordNotFound {
		return true, nil
	}
	return false, err
}

func finishRepair(ctx context.Context, db *gorm.DB, userID int64, platform string, attemptedAll, complete bool, fetchErr error) error {
	if fetchErr != nil {
		return fetchErr
	}
	if platform != spider.QOJ || !attemptedAll || !complete {
		return nil
	}
	state := model.SpiderRepairState{UserID: userID, Platform: platform, RepairKey: qojFullScoreRepairKey, Version: qojFullScoreRepairVersion, CompletedAt: time.Now()}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "platform"}, {Name: "repair_key"}},
			DoUpdates: clause.AssignmentColumns([]string{"version", "completed_at"}),
		}).Create(&state).Error
	})
}

func (uc *SpiderUseCase) invalidateCache(userId int64) {
	ctx := context.Background()
	rdb := uc.data.RDB

	// 1. 精确 key，直接删
	_ = rdb.Del(
		ctx,
		fmt.Sprintf("core:submit_log:user:%d", userId),
		fmt.Sprintf("user:%d:lastSubmitTime", userId),
		fmt.Sprintf("core:contest_log:user:%d", userId),
	).Err()

	// 2. 个人统计版本：只失效该用户 period/heatmap 缓存
	_ = rdb.Incr(ctx, fmt.Sprintf("statistic:user:%d:ver", userId)).Err()

	// 3. 组织/全站全局版本：节流 INCR，避免 50 用户 cron 轮询时缓存 thrash
	//    SetNX 成功才 bump，窗口内其它爬虫跳过全局失效
	ok, err := rdb.SetNX(ctx, "statistic:global:ver:lock", "1", globalCacheBumpMinInterval).Result()
	bumpGlobal := func() {
		_ = rdb.Incr(ctx, "statistic:heatmap:global:ver").Err()
		_ = rdb.Incr(ctx, "statistic:period:global:ver").Err()
		// 组织动态首屏 / 比赛列表短缓存
		_ = rdb.Incr(ctx, "core:submit_feed:global:ver").Err()
		_ = rdb.Incr(ctx, "core:contest:list:global:ver").Err()
	}
	if err != nil {
		// Redis 异常时仍尝试 bump，保证正确性优先
		bumpGlobal()
	} else if ok {
		bumpGlobal()
	}

	_ = rdb.Incr(ctx, fmt.Sprintf("core:contest_log:user:%d:ver", userId)).Err()

	// 热用户：异步预热 period 缓存（读路径更快，2c4g 上仅高热度触发）
	go MaybeWarmUserPeriod(context.Background(), uc.data.DB, rdb, userId)

	// 画像不在每次提交后实时重构；由每日 02:00 活跃用户任务，或
	// 换绑 OJ 的对应全量爬取完成后触发。
}
