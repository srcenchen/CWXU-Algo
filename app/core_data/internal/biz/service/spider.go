package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spidermetrics"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
)

type SpiderUseCase struct {
	data       *data.Data
	problem    *ProblemUseCase
	spiderTask *task.SpiderTask
}

func NewSpiderUseCase(data *data.Data, problem *ProblemUseCase, spiderTask *task.SpiderTask) *SpiderUseCase {
	return &SpiderUseCase{
		data:       data,
		problem:    problem,
		spiderTask: spiderTask,
	}
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
	var dirty bool
	defer func() {
		if dirty {
			uc.invalidateCache(userId)
		}
	}()

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

	var failCount int
	var lastErr error
	for _, plat := range platforms {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("load data timeout user=%d after partial: %w", userId, err)
		}
		changed, err := uc.loadOnePlatform(ctx, userId, plat, needAll)
		if changed {
			dirty = true
		}
		if err != nil {
			failCount++
			lastErr = err
		}
	}
	if failCount == len(platforms) && lastErr != nil {
		return fmt.Errorf("all platforms failed for user %d: %w", userId, lastErr)
	}
	return nil
}

// fetchAndSave 拉取并写入提交；返回新插入行数。fetchCtx 为整次爬取超时（透传到 HTTP）
func (uc *SpiderUseCase) fetchAndSave(fetchCtx context.Context, userId int64, plat model.Platform, needAll bool) (int64, error) {
	p, ok := spider.Get(plat.Platform)
	if !ok {
		return 0, fmt.Errorf("平台插件不存在")
	}
	sbFetch, ok := p.(spider.SubmitLogFetcher)
	if !ok {
		return 0, fmt.Errorf("平台未实现 SubmitLogFetcher")
	}

	// 拉取前记录代数；重绑会 BumpGeneration，写入前再比对，丢弃过期全量结果
	var genAtStart int64
	if uc.data != nil && uc.data.RDB != nil {
		genAtStart = task.CurrentGeneration(uc.data.RDB, userId, plat.Platform)
	}

	tmp, err := sbFetch.FetchSubmitLog(fetchCtx, userId, plat.Username, needAll)
	if err != nil {
		return 0, err
	}
	if len(tmp) == 0 {
		return 0, nil
	}

	// 写入前归一化 is_ac，读路径不再 UPPER(BTRIM(status))
	model.FillIsACBatch(tmp)
	// 去掉误写的「平台:」前缀（如 LuoGu:286690434 → 286690434），避免外链拼错
	model.NormalizeSubmitIDs(tmp)

	// 只插入真正的新行，才能准确累加 daily_user_stats（OnConflict DoNothing 无法区分）
	ctx := context.Background()

	// 同用户同平台串行写入：FilterNew + Insert + ApplyDaily 必须原子视角，
	// 否则两次全量爬虫并发时都会把整批当成「新行」叠 daily/user_ac（重绑连点常见）。
	unlock, locked := uc.tryPlatformWriteLock(ctx, userId, plat.Platform)
	if !locked {
		return 0, fmt.Errorf("平台写入锁占用 user=%d platform=%s", userId, plat.Platform)
	}
	defer unlock()

	if uc.data != nil && uc.data.RDB != nil {
		if cur := task.CurrentGeneration(uc.data.RDB, userId, plat.Platform); cur != genAtStart {
			log.Infof("Spider: drop stale fetch user=%d platform=%s gen %d→%d", userId, plat.Platform, genAtStart, cur)
			return 0, nil
		}
	}

	// 力扣：先清历史重复最近通过，再过滤待插入（同题只留一条）
	if plat.Platform == spider.LeetCode {
		if n, perr := dal.PruneLeetCodeProbDuplicates(ctx, uc.data.DB, userId); perr != nil {
			log.Warnf("Spider: prune leetcode prob dups user=%d: %v", userId, perr)
		} else if n > 0 {
			log.Infof("Spider: pruned %d duplicate leetcode recent-AC rows user=%d", n, userId)
		}
	}
	// 回写已入库的 pending/空状态（CF 评测中先入库后终态不会再进 FilterNew）
	nRefresh, rerr := dal.RefreshPendingSubmitVerdicts(ctx, uc.data.DB, tmp)
	if rerr != nil {
		log.Warnf("Spider: refresh pending status user=%d platform=%s: %v", userId, plat.Platform, rerr)
	} else if nRefresh > 0 {
		log.Infof("Spider: refreshed pending status user=%d platform=%s n=%d", userId, plat.Platform, nRefresh)
	}

	// submit_logs 去重：已有 submit_id 不再累加预聚合（防全量重爬双计）
	neu, err := dal.FilterNewSubmitLogs(ctx, uc.data.DB, tmp)
	if err != nil {
		return 0, err
	}
	var inserted int64
	if len(neu) > 0 {
		// 异常大批量：多为首次全量或明细被清后重爬
		if len(neu) >= 2000 {
			log.Warnf("Spider: large new batch user=%d platform=%s fetched=%d new=%d needAll=%v",
				userId, plat.Platform, len(tmp), len(neu), needAll)
		}

		// 预聚合 + 写入 submit_logs（唯一键 (platform, submit_id) + OnConflict DoNothing）
		// 预聚合已批量化（见 dal），事务内语句数为常数级，避免逐行长事务
		err = uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := dal.ApplyDailyDeltas(ctx, tx, dal.AggregateSubmitDeltas(neu)); err != nil {
				return err
			}
			if err := dal.ApplyUserACFromSubmits(ctx, tx, neu); err != nil {
				return err
			}
			return tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "platform"}, {Name: "submit_id"}},
				DoNothing: true,
			}).CreateInBatches(&neu, submitInsertBatchSize).Error
		})
		if err != nil {
			return 0, err
		}
		inserted = int64(len(neu))
		spidermetrics.IncRows(inserted)
	}

	// 赛后/评测中：本批仍有 Judging 等非终态 → 5 分钟后增量爬，直到没有为止
	uc.maybeSchedulePendingVerdictRetry(userId, plat.Platform, tmp)

	return inserted + nRefresh, nil
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
	key := fmt.Sprintf("spider:writelock:%d:%s", userId, platform)
	const (
		waitStep = 2 * time.Second
		waitMax  = 60 * time.Second
	)
	deadline := time.Now().Add(waitMax)
	for {
		// 与 loadDataTimeout 同量级，防止进程崩溃后死锁
		ok, err := uc.data.RDB.SetNX(ctx, key, "1", loadDataTimeout).Result()
		if err != nil {
			log.Warnf("Spider: writelock redis error (allow): %v", err)
			return func() {}, true
		}
		if ok {
			return func() {
				_ = uc.data.RDB.Del(context.Background(), key).Err()
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
				"rank": gorm.Expr("CASE WHEN EXCLUDED.rank > 0 THEN EXCLUDED.rank ELSE contest_logs.rank END"),
				"ac_count": gorm.Expr("GREATEST(contest_logs.ac_count, EXCLUDED.ac_count)"),
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

// loadOnePlatform 返回 (是否有数据变更, error)
func (uc *SpiderUseCase) loadOnePlatform(ctx context.Context, userId int64, plat model.Platform, needAll bool) (bool, error) {
	// needAll 全量：最多 3 次（原先 12 次会把 worker 占死、队列堆积）
	maxRetries := 1
	if needAll {
		maxRetries = 3
	}
	var anyChange bool
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		rows, err := uc.fetchAndSave(ctx, userId, plat, needAll)
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
		}
		if err == nil {
			// 与提交/比赛一并刷新 rating（未实现 RatingFetcher 的平台自动跳过）
			uc.fetchAndSaveRating(plat)
			log.Infof("Spider: %s %s 成功 new_rows=%d", plat.Platform, plat.Username, rows)
			if anyChange && uc.problem != nil {
				// 异步绑定，避免在 spider worker 内串行 resolve 拖垮队列
				uid := userId
				go uc.problem.BindSubmitsAfterSpider(uid)
			}
			return anyChange, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "平台") {
			log.Errorf(
				"Spider: %s %s 失败: %v",
				plat.Platform,
				plat.Username,
				err,
			)
			// 提交失败仍尝试刷 rating（独立接口，失败不阻断）
			uc.fetchAndSaveRating(plat)
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
			uc.fetchAndSaveRating(plat)
			return anyChange, err
		}
		time.Sleep(3 * time.Second)
	}
	uc.fetchAndSaveRating(plat)
	if lastErr != nil {
		return anyChange, lastErr
	}
	return anyChange, fmt.Errorf("platform %s max retries exceeded", plat.Platform)
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

	// 画像：ver 已变，入队后台重算（HTTP 先读 latest 兜底）
	if uc.problem != nil {
		uc.problem.EnqueueUserProfileRebuild(userId)
	}
}
