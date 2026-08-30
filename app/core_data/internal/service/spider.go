package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/spider"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/opsmetrics"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/ratelimit"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	spiderregistry "cwxu-algo/app/core_data/internal/spider"
	calspider "cwxu-algo/app/core_data/internal/spider/calendar"
	"cwxu-algo/app/core_data/internal/userrpc"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var spiderPlatformWriteUnlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

func clientSyncAuditKeywordCondition(dialect string) string {
	if dialect == "sqlite" {
		return "LOWER(oj_uid) LIKE LOWER(?) OR LOWER(client_version) LIKE LOWER(?) OR CAST(user_id AS TEXT) LIKE ?"
	}
	return "oj_uid ILIKE ? OR client_version ILIKE ? OR CAST(user_id AS TEXT) ILIKE ?"
}

func normalizeClientSyncAuditPlatform(platform string) (string, error) {
	switch strings.TrimSpace(platform) {
	case "":
		return "", nil
	case "luogu", "LuoGu":
		return "luogu", nil
	default:
		return "", errors.BadRequest("INVALID_PLATFORM", "平台无效")
	}
}

var (
	SetForbidden    = errors.Forbidden("权限错误", "权限不允许，设置失败")
	InternalError   = errors.InternalServer("内部错误", "内部错误，操作失败")
	UpdateForbidden = errors.Forbidden("权限错误", "仅站点管理员可手动同步 OJ 数据")
	RateLimitError  = errors.New(429, "TOO_MANY_REQUESTS", "请求过于频繁，请稍后再试")
)

func settleUnfinishedProfileInvalidation(mutationCommitted bool, finish, abandon func() error) error {
	if mutationCommitted {
		return abandon()
	}
	return finish()
}

type SpiderService struct {
	spider.UnimplementedSpiderServer
	db                  *gorm.DB
	rdb                 *redis.Client
	spider              *task.SpiderTask
	reg                 *discovery.Register
	luoguTokenValidator luoguTokenValidator
	luoguImporter       luoguSubmitImporter
	luoguClock          luoguSyncClock
}

func (s SpiderService) allow(ctx context.Context, key string, interval time.Duration) bool {
	ok, err := ratelimit.Allow(ctx, s.rdb, key, interval)
	if err != nil {
		log.Warnf("spider rate limit redis error (allow): %v", err)
	}
	return ok
}

func (s SpiderService) Update(ctx context.Context, req *spider.UpdateReq) (*spider.UpdateRes, error) {
	// 仅站点管理员可手动触发全量同步（普通用户依赖定时任务与绑定后自动抓取）
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return nil, UpdateForbidden
	}
	if !s.allow(ctx, ratelimit.SpiderUpdateKey(req.UserId), 60*time.Second) {
		return nil, RateLimitError
	}
	// 后台入队（与 UpdateAll 一致）：Publish 等 confirm 可达秒级，HTTP 路径不阻塞
	go s.spider.Do(req.UserId, true) // 全量更新该用户全部已绑定平台
	return &spider.UpdateRes{
		Code:    0,
		Message: "更新成功，请稍等片刻，该用户的全量 OJ 数据正在同步",
	}, nil
}

func (s *SpiderService) AdminListClientSyncAudits(ctx context.Context, req *spider.AdminListClientSyncAuditsReq) (*spider.AdminListClientSyncAuditsRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return nil, errors.Forbidden("SYNC_AUDIT_PERMISSION_DENIED", "需要用户同步运维权限")
	}
	pageNum, pageSize := int32(1), int32(20)
	if req != nil {
		if req.PageNum > 0 {
			pageNum = req.PageNum
		}
		if req.PageSize > 0 {
			pageSize = req.PageSize
		}
	}
	if pageSize > 100 {
		pageSize = 100
	}
	q := s.db.WithContext(ctx).Model(&model.ClientSyncAudit{})
	if req != nil {
		if keyword := strings.TrimSpace(req.Keyword); keyword != "" {
			like := "%" + keyword + "%"
			condition := clientSyncAuditKeywordCondition(s.db.Dialector.Name())
			q = q.Where(condition, like, like, like)
		}
		platform, err := normalizeClientSyncAuditPlatform(req.Platform)
		if err != nil {
			return nil, err
		}
		if platform != "" {
			q = q.Where("platform = ?", platform)
		}
		if v := strings.TrimSpace(req.Status); v != "" {
			switch v {
			case "running", "completed", "failed", "terminated", "expired":
			default:
				return nil, errors.BadRequest("INVALID_STATUS", "同步状态无效")
			}
			q = q.Where("status = ?", v)
		}
		if req.From > 0 && req.To > 0 && req.From > req.To {
			return nil, errors.BadRequest("INVALID_TIME_RANGE", "时间范围无效")
		}
		if req.From > 0 {
			q = q.Where("started_at >= ?", time.Unix(req.From, 0).UTC())
		}
		if req.To > 0 {
			q = q.Where("started_at <= ?", time.Unix(req.To, 0).UTC())
		}
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, errors.InternalServer("SYNC_AUDIT_LIST_FAILED", "加载同步日志失败")
	}
	var rows []model.ClientSyncAudit
	if err := q.Order("started_at DESC").Offset(int((pageNum - 1) * pageSize)).Limit(int(pageSize)).Find(&rows).Error; err != nil {
		return nil, errors.InternalServer("SYNC_AUDIT_LIST_FAILED", "加载同步日志失败")
	}
	items := make([]*spider.ClientSyncAuditInfo, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		item := &spider.ClientSyncAuditInfo{SessionId: r.SessionID, AuthorizationId: r.AuthorizationID, UserId: r.UserID, Platform: "luogu", OjUid: r.OJUID, ClientKind: r.ClientKind, ClientVersion: r.ClientVersion, Status: r.Status, CompletionReason: r.CompletionReason, StartedAt: r.StartedAt.Unix(), UpdatedAt: r.UpdatedAt.Unix(), ProcessedPages: r.ProcessedPages, RemoteCount: r.RemoteCount, Inserted: r.Inserted, RestartCount: r.RestartCount, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage}
		if r.TerminalAt != nil {
			item.TerminalAt = r.TerminalAt.Unix()
		}
		items = append(items, item)
	}
	return &spider.AdminListClientSyncAuditsRes{List: items, Total: total, PageNum: pageNum, PageSize: pageSize}, nil
}

// UpdateAll 管理员一键触发所有已绑定 OJ 用户的全量更新（分批入队，削峰）
func (s SpiderService) UpdateAll(ctx context.Context, _ *spider.UpdateAllReq) (*spider.UpdateAllRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return nil, SetForbidden
	}
	adminId := int64(auth.GetCurrentUserId(ctx))
	if !s.allow(ctx, ratelimit.SpiderUpdateAllKey(adminId), 5*time.Minute) {
		return nil, RateLimitError
	}

	var userIds []int64
	if err := s.db.Model(&model.Platform{}).
		Distinct("user_id").
		Pluck("user_id", &userIds).Error; err != nil {
		log.Errorf("UpdateAll: query platform users failed: %v", err)
		return nil, InternalError
	}

	// 分批入队削峰；并发消费由 spider consumer 控制
	go s.spider.DoBatch(context.Background(), userIds, true, 30, 200*time.Millisecond)

	return &spider.UpdateAllRes{
		Code:    0,
		Message: fmt.Sprintf("已为 %d 名用户分批入队全量更新，后台并发抓取中", len(userIds)),
		Count:   int64(len(userIds)),
	}, nil
}

// UpdatePlatform 站管：按平台全量回填（如力扣比赛记录）。
// body: { "platform": "LeetCode" }
// 仅入队该平台已绑定用户的 needAll 任务，并强制清去重（避免与刚跑完的 update-all 撞 pending）。
func (s *SpiderService) UpdatePlatform(ctx context.Context, req *spider.UpdatePlatformReq) (*spider.UpdatePlatformRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return &spider.UpdatePlatformRes{Code: 1, Message: "仅站点管理员可操作"}, nil
	}
	adminId := int64(auth.GetCurrentUserId(ctx))
	if !s.allow(ctx, ratelimit.SpiderUpdateAllKey(adminId)+":plat", 2*time.Minute) {
		return &spider.UpdatePlatformRes{Code: 1, Message: "请求过于频繁，请稍后再试"}, nil
	}
	plat := strings.TrimSpace(req.Platform)
	if plat == "" {
		return &spider.UpdatePlatformRes{Code: 1, Message: "缺少 platform"}, nil
	}
	// 规范化已知平台名
	switch strings.ToLower(plat) {
	case "leetcode", "力扣":
		plat = spiderregistry.LeetCode
	case "codeforces", "cf":
		plat = spiderregistry.CodeForces
	case "atcoder":
		plat = spiderregistry.AtCoder
	case "luogu", "洛谷":
		plat = spiderregistry.LuoGu
	case "nowcoder", "牛客":
		plat = spiderregistry.NowCoder
	case "qoj":
		plat = spiderregistry.QOJ
	case "loj", "libreoj":
		plat = spiderregistry.LOJ
	case "uoj", "universaloj":
		plat = spiderregistry.UOJ
	}
	if _, ok := spiderregistry.Get(plat); !ok {
		return &spider.UpdatePlatformRes{Code: 1, Message: "不支持的平台: " + plat}, nil
	}
	users, published := 0, 0
	if s.spider != nil {
		users, published = s.spider.DoBatchPlatform(context.Background(), plat, true, true)
	}
	return &spider.UpdatePlatformRes{
		Code:      0,
		Message:   fmt.Sprintf("已为平台 %s 的 %d 名用户入队全量同步（published=%d），后台抓取中", plat, users, published),
		Count:     int32(users),
		Published: int32(published),
		Platform:  plat,
	}, nil
}

// RepairContestCells 幂等修复 AtCoder 赛时提交明细相关脏数据（仅站管）。
// 站内榜 cell-submits 脏数据修复（external_id / 赛后练习格 / relative_sec）。
func (s *SpiderService) RepairContestCells(ctx context.Context, _ *spider.RepairContestCellsReq) (*spider.RepairContestCellsRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return &spider.RepairContestCellsRes{Success: false, Message: "仅管理员可操作"}, nil
	}
	// 顺带规范日历 platform 大小写
	if s.db != nil {
		_ = dal.NewContestCalendarDalDB(s.db).NormalizeLegacyPlatformNames()
	}
	stats, err := bizservice.RepairContestCellSubmitData(s.db)
	if err != nil {
		log.Errorf("repair-contest-cells: %v", err)
		return &spider.RepairContestCellsRes{Success: false, Message: err.Error()}, nil
	}
	data := make(map[string]int32, len(stats))
	for k, v := range stats {
		data[k] = int32(v)
	}
	return &spider.RepairContestCellsRes{Success: true, Message: "ok", Data: data}, nil
}

func (s SpiderService) GetSpider(ctx context.Context, req *spider.GetSpiderReq) (*spider.GetSpiderRep, error) {
	var plats []model.Platform
	err := s.db.Where("user_id = ?", req.UserId).Find(&plats).Error
	if err != nil {
		return nil, InternalError
	}
	res := make([]*spider.GetSpiderRep_Data, 0)
	for _, v := range plats {
		item := &spider.GetSpiderRep_Data{
			Platform:  v.Platform,
			Username:  v.Username,
			Rating:    int32(v.Rating),
			HasRating: v.HasRating,
		}
		if s.spider != nil {
			ok, fail, errMsg := s.spider.GetPlatformSyncHealth(req.UserId, v.Platform)
			item.LastSyncAt = ok
			item.LastFailAt = fail
			item.LastError = errMsg
		}
		res = append(res, item)
	}
	var lastSync int64
	if s.spider != nil {
		lastSync = s.spider.GetLastOK(req.UserId)
	}
	return &spider.GetSpiderRep{
		LastSyncAt: lastSync,
		Data:       res,
	}, nil
}

func (s SpiderService) SetSpider(ctx context.Context, req *spider.SetSpiderReq) (*spider.SetSpiderRep, error) {
	if !auth.VerifySelfOrAbove(ctx, uint(req.UserId)) {
		return nil, SetForbidden
	}
	platformName := strings.TrimSpace(req.Platform)
	username := strings.TrimSpace(req.Username)
	if _, ok := spiderregistry.Get(platformName); !ok {
		return nil, errors.BadRequest("参数错误", "不支持该 OJ 平台")
	}
	if username == "" || len([]rune(username)) > 128 {
		return nil, errors.BadRequest("参数错误", "OJ 用户名不能为空且最多 128 个字符")
	}
	if !s.allow(ctx, ratelimit.SpiderSetKey(platformName, username), 30*time.Second) {
		return nil, RateLimitError
	}
	pending, err := s.prepareSpiderMaintenance(ctx, spiderSetMaintenanceScope(req.UserId, platformName), spiderMaintenanceSetBinding, spiderSetMaintenancePayload{
		UserID: req.UserId, Platform: platformName, Username: username,
	})
	if err != nil {
		log.Errorf("SetSpider prepare durable maintenance failed: %v", err)
		return nil, InternalError
	}
	if err := s.executeSetSpiderMaintenance(ctx, pending); err != nil {
		log.Errorf("SetSpider durable maintenance failed: %v", err)
		return nil, InternalError
	}
	// 站管已暂停该 OJ：绑定照常保存，但提示用户同步暂时停用（DoPlatform 内部不会入队）
	if task.IsPlatformPaused(s.rdb, platformName) {
		return &spider.SetSpiderRep{
			Code:    0,
			Message: fmt.Sprintf("绑定成功，但 %s 的同步已临时停用，恢复后会自动同步", platformName),
		}, nil
	}
	return &spider.SetSpiderRep{
		Code:    0,
		Message: fmt.Sprintf("绑定成功，正在同步 %s 的全量数据，请稍候", platformName),
	}, nil
}

func trySpiderPlatformWriteLock(ctx context.Context, rdb *redis.Client, userID int64, platform string) (func(), bool) {
	if rdb == nil {
		return func() {}, true
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return func() {}, false
	}
	token := hex.EncodeToString(tokenBytes)
	key := fmt.Sprintf("spider:writelock:%d:%s", userID, platform)
	for {
		ok, err := rdb.SetNX(ctx, key, token, 2*time.Minute).Result()
		if err != nil {
			return func() {}, false
		}
		if ok {
			return func() {
				_ = spiderPlatformWriteUnlockScript.Run(context.Background(), rdb, []string{key}, token).Err()
			}, true
		}
		select {
		case <-ctx.Done():
			return func() {}, false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func replaceSpiderBindingAfterGenerationBump(ctx context.Context, spiderTask *task.SpiderTask, db *gorm.DB, platform model.Platform) error {
	if spiderTask == nil {
		return fmt.Errorf("replace spider binding: spider task unavailable")
	}
	if _, err := spiderTask.BumpGeneration(platform.UserID, platform.Platform); err != nil {
		return err
	}
	return replaceSpiderBinding(ctx, db, platform)
}

func replaceSpiderBinding(ctx context.Context, db *gorm.DB, platform model.Platform) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := deleteSpiderPlatformData(ctx, tx, platform.UserID, platform.Platform); err != nil {
			return err
		}
		return tx.Create(&platform).Error
	})
}

func deleteSpiderPlatformData(ctx context.Context, db *gorm.DB, userID int64, platform string) error {
	for _, value := range []interface{}{&model.Platform{}, &model.SubmitLog{}, &model.ContestLog{}, &model.ContestUserProblem{}} {
		if !db.Migrator().HasTable(value) {
			continue
		}
		if err := db.WithContext(ctx).Where("user_id = ? AND platform = ?", userID, platform).Delete(value).Error; err != nil {
			return err
		}
	}
	if err := dal.DeletePlatformDailyStats(ctx, db, userID, platform); err != nil {
		return err
	}
	if err := dal.DeletePlatformUserAC(ctx, db, userID, platform); err != nil {
		return err
	}
	return db.WithContext(ctx).Where("user_id = ? AND platform = ?", userID, platform).Delete(&model.SpiderRepairState{}).Error
}

const purgeSubmitsConfirm = "PURGE_SUBMITS"

var purgeSubmitTables = []string{
	"submit_logs",
	"daily_user_stats",
	"user_ac_problems",
	"user_ac_problem_days",
	"contest_logs",
	"contest_calendar_notify_logs",
	"user_problem_status",
	"user_tag_ac_snapshots",
	"user_tag_ac",
	"problem_ability_stats",
	"ability_model_state",
	"ability_profile_schedule_runs",
}

var purgeSubmitTableSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(purgeSubmitTables))
	for _, table := range purgeSubmitTables {
		out[table] = struct{}{}
	}
	return out
}()

var purgeUserPlatforms = []string{
	spiderregistry.AtCoder,
	spiderregistry.CodeForces,
	spiderregistry.LeetCode,
	spiderregistry.LOJ,
	spiderregistry.LuoGu,
	spiderregistry.NowCoder,
	spiderregistry.POJ,
	spiderregistry.QOJ,
	spiderregistry.UOJ,
}

func withPurgeUserPlatformGuards(
	ctx context.Context,
	platforms []string,
	validate func(context.Context) error,
	bump func(string) error,
	lock func(context.Context, string) (func(), bool),
	remove func() error,
) error {
	validateStage := func() error {
		if validate == nil {
			return nil
		}
		return validate(ctx)
	}
	for _, platform := range platforms {
		if err := validateStage(); err != nil {
			return fmt.Errorf("validate profile fence before generation bump for %s: %w", platform, err)
		}
		if err := bump(platform); err != nil {
			return fmt.Errorf("bump purge generation for %s: %w", platform, err)
		}
	}
	unlocks := make([]func(), 0, len(platforms))
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, platform := range platforms {
		if err := validateStage(); err != nil {
			return fmt.Errorf("validate profile fence before write lock for %s: %w", platform, err)
		}
		unlock, ok := lock(ctx, platform)
		if !ok {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("acquire purge write lock for %s: %w", platform, err)
			}
			return fmt.Errorf("acquire purge write lock for %s", platform)
		}
		unlocks = append(unlocks, unlock)
	}
	if err := validateStage(); err != nil {
		return fmt.Errorf("validate profile fence before user purge: %w", err)
	}
	return remove()
}

// SubmitInventory 运维：真实入库提交库存（仅站点管理员）
// 结果在 Redis 短缓存（5 分钟），避免每次打开运维页对 67 万行做全表 COUNT。
func (s SpiderService) SubmitInventory(ctx context.Context, _ *spider.SubmitInventoryReq) (*spider.SubmitInventoryRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return nil, errors.Forbidden("权限不足", "仅站点管理员可查看提交库存")
	}
	const cacheKey = "ops:submit_inventory"
	const cacheTTL = 5 * time.Minute
	type inventory struct {
		Total     int64
		RealTotal int64
		Oldest    int64
		Newest    int64
	}
	if s.rdb != nil {
		if b, e := s.rdb.Get(ctx, cacheKey).Bytes(); e == nil && len(b) > 0 {
			var cached inventory
			if json.Unmarshal(b, &cached) == nil {
				return &spider.SubmitInventoryRes{
					Code:                0,
					Message:             "ok",
					SubmitLogsTotal:     cached.Total,
					SubmitLogsRealTotal: cached.RealTotal,
					// CountedSubmitIdsTotal 已废弃（账本表已删），固定 0 保持 wire 兼容
					CountedSubmitIdsTotal: 0,
					OldestTime:            cached.Oldest,
					NewestTime:            cached.Newest,
				}, nil
			}
		}
	}

	var total, realTotal int64
	if err := s.db.WithContext(ctx).Model(&model.SubmitLog{}).Count(&total).Error; err != nil {
		return nil, InternalError
	}
	if err := s.db.WithContext(ctx).Model(&model.SubmitLog{}).
		Where(model.SQLExcludeLeetCodeNonSubmit).
		Count(&realTotal).Error; err != nil {
		return nil, InternalError
	}
	var bounds struct {
		Oldest *time.Time
		Newest *time.Time
	}
	_ = s.db.WithContext(ctx).Model(&model.SubmitLog{}).
		Select("MIN(time) AS oldest, MAX(time) AS newest").
		Scan(&bounds).Error
	var oldest, newest int64
	if bounds.Oldest != nil {
		oldest = bounds.Oldest.Unix()
	}
	if bounds.Newest != nil {
		newest = bounds.Newest.Unix()
	}
	if s.rdb != nil {
		if b, e := json.Marshal(inventory{Total: total, RealTotal: realTotal, Oldest: oldest, Newest: newest}); e == nil {
			_ = s.rdb.Set(ctx, cacheKey, b, cacheTTL).Err()
		}
	}
	return &spider.SubmitInventoryRes{
		Code:                0,
		Message:             "ok",
		SubmitLogsTotal:     total,
		SubmitLogsRealTotal: realTotal,
		// CountedSubmitIdsTotal 已废弃（账本表已删），固定 0 保持 wire 兼容
		CountedSubmitIdsTotal: 0,
		OldestTime:            oldest,
		NewestTime:            newest,
	}, nil
}

// PurgeSubmitsAndRecrawl 运维：硬清训练数据并全量重爬（仅站管）。
//
// 保留：platforms（OJ 绑定）、problems/题库、bulletin/emergency、比赛日历赛程与订阅。
// 硬删：submit_logs（真假全删）、日汇总、AC 预聚合、contest_logs、提醒发送日志、
// 以及相关 Redis 缓存。用户账号在 user 库，本接口不动。
func (s SpiderService) PurgeSubmitsAndRecrawl(ctx context.Context, req *spider.PurgeSubmitsAndRecrawlReq) (*spider.PurgeSubmitsAndRecrawlRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return nil, errors.Forbidden("权限不足", "仅站点管理员可执行此运维操作")
	}
	if strings.TrimSpace(req.GetConfirm()) != purgeSubmitsConfirm {
		return &spider.PurgeSubmitsAndRecrawlRes{
			Code:    2,
			Message: "请输入确认口令 PURGE_SUBMITS",
		}, nil
	}
	adminID := int64(auth.GetCurrentUserId(ctx))
	const purgeLockKey = "ops:purge_submits"
	if s.rdb != nil {
		ok, err := s.rdb.SetNX(ctx, purgeLockKey, "1", 30*time.Minute).Result()
		if err != nil {
			log.Warnf("purge_submits lock redis: %v", err)
		} else if !ok {
			return &spider.PurgeSubmitsAndRecrawlRes{
				Code:    3,
				Message: "已有清空任务在进行，请稍后再试",
			}, nil
		} else {
			defer func() { _ = s.rdb.Del(context.Background(), purgeLockKey).Err() }()
		}
	}
	// 先统计行数再 TRUNCATE（硬删，最快且不留脏页）
	countTable := func(name string) (int64, error) {
		if !s.db.Migrator().HasTable(name) {
			return 0, nil
		}
		var n int64
		if err := s.db.WithContext(ctx).Table(name).Count(&n).Error; err != nil {
			return 0, err
		}
		return n, nil
	}
	deletedLogs, err := countTable("submit_logs")
	if err != nil {
		return nil, InternalError
	}
	deletedDaily, err := countTable("daily_user_stats")
	if err != nil {
		return nil, InternalError
	}
	deletedAcProblems, err := countTable("user_ac_problems")
	if err != nil {
		return nil, InternalError
	}
	deletedAcDays, err := countTable("user_ac_problem_days")
	if err != nil {
		return nil, InternalError
	}
	deletedAc := deletedAcProblems + deletedAcDays
	deletedContests, err := countTable("contest_logs")
	if err != nil {
		return nil, InternalError
	}
	pending, err := s.prepareSpiderMaintenance(ctx, spiderPurgeGlobalMaintenanceScope, spiderMaintenancePurgeGlobal, struct{}{})
	if err != nil {
		log.Errorf("purge prepare durable maintenance: %v", err)
		return nil, InternalError
	}

	if err := s.executePurgeGlobalMaintenance(ctx, pending); err != nil {
		log.Errorf("purge durable maintenance: %v", err)
		return nil, InternalError
	}
	var userIds []int64
	if err := s.db.Model(&model.Platform{}).
		Distinct("user_id").
		Pluck("user_id", &userIds).Error; err != nil {
		log.Errorf("purge recrawl list users: %v", err)
		return nil, InternalError
	}

	log.Warnf("ops purge-submits admin=%d logs=%d daily=%d ac=%d contests=%d enqueued=%d",
		adminID, deletedLogs, deletedDaily, deletedAc, deletedContests, len(userIds))

	return &spider.PurgeSubmitsAndRecrawlRes{
		Code: 0,
		Message: fmt.Sprintf(
			"已硬清提交/统计/比赛记录等训练数据（保留 OJ 绑定与题库），并为 %d 名用户触发全量重爬",
			len(userIds),
		),
		DeletedSubmitLogs: deletedLogs,
		// DeletedLedger 已废弃（账本表已删），固定 0 保持 wire 兼容
		DeletedLedger: 0,
		DeletedDaily:  deletedDaily,
		DeletedAc:     deletedAc,
		EnqueuedUsers: int64(len(userIds)),
	}, nil
}

// purgeTrainingCaches 清训练相关 Redis，避免 purge 后脏缓存
func (s SpiderService) purgeTrainingCaches(ctx context.Context, userIds []int64) error {
	if s.rdb == nil {
		return nil
	}
	if err := s.rdb.Incr(ctx, "statistic:heatmap:global:ver").Err(); err != nil {
		return err
	}
	if err := s.rdb.Incr(ctx, "statistic:period:global:ver").Err(); err != nil {
		return err
	}
	// 提交库存缓存失效，避免 purge 后运维页仍显示旧规模
	if err := s.rdb.Del(ctx, "ops:submit_inventory").Err(); err != nil {
		return err
	}

	plats := []string{"AtCoder", "Codeforces", "LuoGu", "NowCoder", "QOJ", "LeetCode", "CodeForces", "LOJ", "UOJ", "POJ"}
	const chunk = 200
	for i := 0; i < len(userIds); i += chunk {
		j := i + chunk
		if j > len(userIds) {
			j = len(userIds)
		}
		keys := make([]string, 0, (j-i)*12)
		for _, uid := range userIds[i:j] {
			keys = append(keys,
				fmt.Sprintf("core:submit_log:user:%d", uid),
				fmt.Sprintf("user:%d:lastSubmitTime", uid),
				fmt.Sprintf("statistic:user:%d:ver", uid),
				fmt.Sprintf("core:contest_log:user:%d:ver", uid),
				fmt.Sprintf("spider:pending:%d", uid),
				fmt.Sprintf("spider:inflight:%d", uid),
				fmt.Sprintf("spider:last_ok:%d", uid),
			)
			for _, p := range plats {
				keys = append(keys,
					fmt.Sprintf("spider:pending:%d:%s", uid, p),
					fmt.Sprintf("spider:inflight:%d:%s", uid, p),
					fmt.Sprintf("spider:writelock:%d:%s", uid, p),
					fmt.Sprintf("spider:gen:%d:%s", uid, p),
				)
			}
		}
		if len(keys) > 0 {
			if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ClearPurgeLock 启动时清除运维 purge 锁（进程挂掉时可能残留）
func ClearPurgeLock(rdb *redis.Client) {
	if rdb == nil {
		return
	}
	if err := rdb.Del(context.Background(), "ops:purge_submits").Err(); err != nil {
		log.Warnf("clear ops:purge_submits: %v", err)
	}
}

// deleteAllInBatches 分批清空表（TRUNCATE 失败时回退）
func deleteAllInBatches(ctx context.Context, db *gorm.DB, table string, batch int) (int64, error) {
	return deleteAllInBatchesValidated(ctx, db, table, batch, nil)
}

func deleteAllInBatchesValidated(ctx context.Context, db *gorm.DB, table string, batch int, validate func(context.Context) error) (int64, error) {
	if db == nil || table == "" {
		return 0, nil
	}
	if _, ok := purgeSubmitTableSet[table]; !ok {
		return 0, fmt.Errorf("refuse delete table %s", table)
	}
	if batch <= 0 {
		batch = 5000
	}
	var total int64
	idColumn := "ctid"
	if db.Dialector.Name() != "postgres" {
		idColumn = "rowid"
	}
	for {
		if validate != nil {
			if err := validate(ctx); err != nil {
				return total, err
			}
		}
		res := db.WithContext(ctx).Exec(fmt.Sprintf(`
			DELETE FROM %s
			WHERE %s IN (
				SELECT %s FROM %s LIMIT %d
			)
		`, table, idColumn, idColumn, table, batch))
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if res.RowsAffected == 0 {
			break
		}
	}
	return total, nil
}

func purgeSubmitData(ctx context.Context, db *gorm.DB, tables []string, validate func(context.Context) error, pending *model.AbilityMaintenancePending) (spiderMaintenanceTxOutcome, error) {
	if db == nil {
		return spiderMaintenanceUnknown, fmt.Errorf("purge training tables: nil database")
	}
	for _, table := range tables {
		if _, ok := purgeSubmitTableSet[table]; !ok {
			return spiderMaintenanceRolledBack, fmt.Errorf("refuse purge table %s", table)
		}
	}
	run := func(truncate bool) (spiderMaintenanceTxOutcome, error) {
		if validate != nil {
			if err := validate(ctx); err != nil {
				return spiderMaintenanceRolledBack, err
			}
		}
		mutation := func(tx *gorm.DB) error {
			var previous model.AbilityModelState
			query := tx.Where("id = ?", 1)
			if tx.Dialector.Name() == "postgres" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			err := query.First(&previous).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return err
			}
			if truncate {
				if err := tx.Exec("TRUNCATE TABLE " + strings.Join(tables, ", ") + " RESTART IDENTITY").Error; err != nil {
					return err
				}
			} else {
				for _, table := range tables {
					if _, err := deleteAllInBatchesValidated(ctx, tx, table, 5000, validate); err != nil {
						return err
					}
				}
			}
			if containsProfileEvidenceSourceTable(tables) {
				if err := dal.BumpProfileEvidenceDataset(ctx, tx); err != nil {
					return err
				}
			}
			if containsString(tables, "ability_model_state") {
				nextVersion, err := nextAbilityModelVersion(previous.ActiveVersion)
				if err != nil {
					return err
				}
				now := time.Now()
				state := model.AbilityModelState{ID: 1, ActiveVersion: nextVersion, BuiltAt: now, UpdatedAt: now}
				if err := tx.Create(&state).Error; err != nil {
					return err
				}
			}
			if pending != nil {
				return markSpiderMaintenanceFacts(ctx, tx, pending)
			}
			return nil
		}
		if pending == nil {
			if err := db.WithContext(ctx).Transaction(mutation); err != nil {
				return spiderMaintenanceRolledBack, err
			}
			return spiderMaintenanceCommitted, nil
		}
		return runSpiderMaintenanceFactsTransaction(ctx, db, pending, mutation)
	}
	outcome, err := run(true)
	if outcome == spiderMaintenanceCommitted {
		return outcome, nil
	}
	if outcome == spiderMaintenanceUnknown {
		return outcome, err
	}
	if err != nil {
		log.Warnf("purge TRUNCATE failed, falling back to transactional DELETE: %v", err)
	}
	return run(false)
}

func nextAbilityModelVersion(active uint64) (uint64, error) {
	if active >= uint64(math.MaxInt64) {
		return 0, fmt.Errorf("ability model version exhausted")
	}
	return active + 1, nil
}

func containsProfileEvidenceSourceTable(tables []string) bool {
	return containsString(tables, "submit_logs") || containsString(tables, "user_ac_problems") || containsString(tables, "platforms")
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// EnqueueUserSpider 服务间入队（休眠唤醒等）；无站管鉴权
func (s SpiderService) EnqueueUserSpider(ctx context.Context, req *spider.EnqueueUserSpiderReq) (*spider.EnqueueUserSpiderRes, error) {
	if req == nil || req.UserId <= 0 {
		return &spider.EnqueueUserSpiderRes{Code: 1, Message: "用户ID无效"}, nil
	}
	res := s.spider.Do(req.UserId, req.NeedAll)
	return &spider.EnqueueUserSpiderRes{
		Code:      0,
		Message:   "已入队",
		Published: int64(res.Published),
	}, nil
}

// PurgeUserData 硬删除用户在 core 库的全部关联数据（删除用户时调用）
func (s SpiderService) PurgeUserData(ctx context.Context, req *spider.PurgeUserDataReq) (*spider.PurgeUserDataRes, error) {
	if req.UserId <= 0 {
		return &spider.PurgeUserDataRes{Code: 1, Message: "用户ID无效"}, nil
	}
	uid := req.UserId
	pending, err := s.prepareSpiderMaintenance(ctx, spiderPurgeUserMaintenanceScope(uid), spiderMaintenancePurgeUser, spiderPurgeUserMaintenancePayload{UserID: uid})
	if err != nil {
		log.Errorf("PurgeUserData: prepare durable maintenance user=%d: %v", uid, err)
		return nil, InternalError
	}
	if err := s.executePurgeUserMaintenance(ctx, pending); err != nil {
		log.Errorf("PurgeUserData: durable maintenance user=%d: %v", uid, err)
		return nil, InternalError
	}
	return &spider.PurgeUserDataRes{Code: 0, Message: "已清空该用户的训练与绑定数据"}, nil
}

func (s SpiderService) purgeUserDataLocked(ctx context.Context, uid int64, validate func(context.Context) error) (bool, error) {
	validateStage := func() error {
		if validate == nil {
			return nil
		}
		return validate(ctx)
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateStage(); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", uid).Delete(&model.Platform{}).Error; err != nil {
			return fmt.Errorf("platform: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", uid).Delete(&model.SubmitLog{}).Error; err != nil {
			return fmt.Errorf("submit_log: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", uid).Delete(&model.ContestLog{}).Error; err != nil {
			return fmt.Errorf("contest_log: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := dal.DeleteUserPreagg(ctx, tx, uid); err != nil {
			return fmt.Errorf("preagg: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := purgeSpiderRepairStates(ctx, tx, uid); err != nil {
			return fmt.Errorf("spider repair state: %w", err)
		}
		return validateStage()
	}); err != nil {
		return false, err
	}
	if err := validateStage(); err != nil {
		return true, err
	}
	// 缓存 / 爬虫 inflight / 上次同步
	keys := []string{
		fmt.Sprintf("core:submit_log:user:%d", uid),
		fmt.Sprintf("spider:pending:%d", uid),
		fmt.Sprintf("spider:inflight:%d", uid),
		fmt.Sprintf("spider:last_ok:%d", uid),
		fmt.Sprintf("statistic:user:%d:ver", uid),
		"core:platforms:bound_users:v1",
		fmt.Sprintf("core:platforms:user:%d:v1", uid),
	}
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		log.Warnf("PurgeUserData: redis del user=%d: %v", uid, err)
	}
	if _, err := (&s).purgeLuoguSyncRedis(ctx, uid); err != nil {
		log.Warnf("PurgeUserData: luogu sync redis user=%d: %v", uid, err)
	}
	if err := validateStage(); err != nil {
		return true, err
	}
	_ = s.rdb.Incr(ctx, "statistic:period:global:ver").Err()
	// 按平台清除任务去重状态；写锁由 owner-safe unlock 释放，代数必须保留以拦截旧任务。
	for _, p := range purgeUserPlatforms {
		if err := validateStage(); err != nil {
			return true, err
		}
		_ = s.rdb.Del(ctx,
			fmt.Sprintf("spider:pending:%d:%s", uid, p),
			fmt.Sprintf("spider:inflight:%d:%s", uid, p),
		).Err()
	}
	return true, nil
}

func purgeSpiderRepairStates(ctx context.Context, db *gorm.DB, userID int64) error {
	return db.WithContext(ctx).Where("user_id = ?", userID).Delete(&model.SpiderRepairState{}).Error
}

func NewSpiderService(data *data.Data, spider *task.SpiderTask, reg *discovery.Register, importer *bizservice.SpiderUseCase) *SpiderService {
	// 进程启动清除残留 purge 锁（上次崩溃 / 未 defer 的旧版本）
	if data != nil {
		ClearPurgeLock(data.RDB)
	}
	service := &SpiderService{
		db:            data.DB,
		rdb:           data.RDB,
		spider:        spider,
		reg:           reg,
		luoguImporter: importer,
		luoguClock:    realLuoguSyncClock{},
	}
	go service.runLuoguCleanupRecovery()
	go service.runSpiderMaintenanceRecovery()
	return service
}

// ojCap 单个 OJ 的爬虫能力（提交/题库/比赛日历/全局账号）
type ojCap struct {
	platform string
	problem  bool
	contest  bool
	// account 关联的全局账号服务（sitesettings；空=无需账号）
	account string
}

// ojCaps 站管监控覆盖的全部 OJ 及其模块能力（与注册的爬虫 provider 对齐）
var ojCaps = []ojCap{
	{platform: "NowCoder", problem: true, contest: true},
	{platform: "AtCoder", problem: true, contest: true},
	{platform: "CodeForces", problem: true, contest: true},
	{platform: "LuoGu", problem: true, contest: true, account: sitesettings.ServiceLuoGu},
	{platform: "QOJ", problem: true, contest: true, account: sitesettings.ServiceQOJ},
	{platform: "LeetCode", problem: true, contest: true},
	{platform: "LOJ", problem: true},
	{platform: "UOJ", problem: true},
	{platform: "POJ", problem: true},
}

func hasSpiderMonitorReadPermission(claims *auth.JwtPayload) bool {
	return auth.PayloadHasPerm(claims, rbac.PermSiteConfigRead) ||
		auth.PayloadHasPerm(claims, rbac.PermSiteSpiderOps) ||
		auth.PayloadHasPerm(claims, rbac.PermSiteProblemOps)
}

// GetSpiderMonitor 运维：各 OJ 爬虫模块监控（提交/题库/比赛/账号）。
func (s SpiderService) GetSpiderMonitor(ctx context.Context, _ *spider.SpiderMonitorReq) (*spider.SpiderMonitorRes, error) {
	if !hasSpiderMonitorReadPermission(auth.GetCurrentUser(ctx)) {
		return &spider.SpiderMonitorRes{Code: 1, Message: "需要查看站点配置、爬虫运维或题库运维权限"}, nil
	}

	// DB 按平台一次 GROUP BY（空库/无该平台行 → 计 0）；平台名归一化，
	// 兼容日历里 cpolar 小写 id（uoj 等），避免与 ojCaps 规范名对不上
	countBy := func(table string) map[string]int64 {
		out := make(map[string]int64)
		if s.db == nil {
			return out
		}
		type row struct {
			Platform string
			N        int64
		}
		var rows []row
		if err := s.db.WithContext(ctx).Table(table).
			Select("platform, COUNT(*) AS n").
			Group("platform").Scan(&rows).Error; err != nil {
			log.Warnf("GetSpiderMonitor: count %s: %v", table, err)
			return out
		}
		for _, r := range rows {
			out[calspider.NormalizePlatform(r.Platform)] += r.N
		}
		return out
	}
	boundBy := countBy("platforms")
	submitBy := countBy("submit_logs")
	problemBy := countBy("problems")
	// 比赛数：与「比赛页（组织内出现过的比赛）」同源 contest_logs，按 平台+比赛 去重；
	// 原用 contest_calendars（公开赛程日历，12h 爬一次、7 天前自动清理），数字波动且与参赛记录口径对不上
	contestBy := func() map[string]int64 {
		out := make(map[string]int64)
		if s.db == nil {
			return out
		}
		type row struct {
			Platform string
		}
		var rows []row
		if err := s.db.WithContext(ctx).Table("contest_logs").
			Select("DISTINCT platform, contest_id").
			Scan(&rows).Error; err != nil {
			log.Warnf("GetSpiderMonitor: count contest_logs: %v", err)
			return out
		}
		for _, r := range rows {
			out[calspider.NormalizePlatform(r.Platform)]++
		}
		return out
	}()

	// 账号状态只取一次（洛谷/QOJ）；未配置账号时忽略 Redis 中可能残留的旧成功状态。
	accountStatus := sitesettings.GetAllServiceStatus(ctx, s.rdb)
	siteRuntime := sitesettings.Load(ctx, s.rdb, nil)

	now := time.Now()
	stats := make([]*spider.SpiderPlatformStat, 0, len(ojCaps))
	for _, cap := range ojCaps {
		today := opsmetrics.ReadSpiderPlatformToday(ctx, s.rdb, cap.platform)
		submitPaused := task.IsPlatformPaused(s.rdb, cap.platform)
		st := &spider.SpiderPlatformStat{
			Platform:           cap.platform,
			BoundUsers:         boundBy[cap.platform],
			SubmitCount:        submitBy[cap.platform],
			ProblemCount:       problemBy[cap.platform],
			ContestCount:       contestBy[cap.platform],
			TodayEnqueued:      today.Enqueued,
			TodayRows:          today.Rows,
			TodayOk:            today.OK,
			TodayFail:          today.Fail,
			HasSubmitFetcher:   true,
			HasProblemFetch:    cap.problem,
			HasContestCalendar: cap.contest,
			Paused:             submitPaused,
			SubmitPaused:       submitPaused,
			ProblemPaused:      task.IsProblemPlatformPaused(s.rdb, cap.platform),
		}
		if s.rdb != nil {
			// 最近同步（按 OJ 聚合）
			if v, err := s.rdb.Get(ctx, task.OjLastOKKey(cap.platform)).Int64(); err == nil {
				st.LastOkAt = v
			}
			if v, err := s.rdb.Get(ctx, task.OjLastFailKey(cap.platform)).Int64(); err == nil {
				st.LastFailAt = v
			}
			if v, err := s.rdb.Get(ctx, task.OjLastErrKey(cap.platform)).Result(); err == nil {
				st.LastError = v
			}
		}
		// 全局账号模块
		if cap.account != "" {
			acc, ok := accountStatus[cap.account]
			if !ok {
				acc = sitesettings.GetServiceStatus(ctx, s.rdb, cap.account)
			}
			if !crawlerAccountConfigured(siteRuntime, cap.account) {
				acc = sitesettings.ServiceStatus{Status: sitesettings.StatusUnchecked}
			}
			st.HasAccount = true
			st.AccountStatus = acc.Status
			st.AccountAt = acc.At
			st.AccountErr = acc.ErrMsg
		}
		stats = append(stats, st)
	}

	return &spider.SpiderMonitorRes{
		Code:        0,
		Message:     "success",
		Platforms:   stats,
		CollectedAt: now.Unix(),
	}, nil
}

// GetPlatformUsers 站管：某 OJ 的绑定用户列表（含站内展示名 + 绑定的 OJ 账号）
func (s SpiderService) GetPlatformUsers(ctx context.Context, req *spider.GetPlatformUsersReq) (*spider.GetPlatformUsersRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteConfigRead) {
		return &spider.GetPlatformUsersRes{Code: 1, Message: "需要查看站点配置权限"}, nil
	}
	platform := strings.TrimSpace(req.GetPlatform())
	if platform == "" {
		return &spider.GetPlatformUsersRes{Code: 1, Message: "platform 不能为空"}, nil
	}
	platform = calspider.NormalizePlatform(platform)
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := int(req.GetOffset())
	if offset < 0 {
		offset = 0
	}

	var total int64
	if err := s.db.WithContext(ctx).Model(&model.Platform{}).
		Where("platform = ?", platform).Count(&total).Error; err != nil {
		log.Warnf("GetPlatformUsers count %s: %v", platform, err)
		return &spider.GetPlatformUsersRes{Code: 1, Message: "查询失败"}, nil
	}
	var rows []model.Platform
	if err := s.db.WithContext(ctx).Where("platform = ?", platform).
		Order("id ASC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		log.Warnf("GetPlatformUsers list %s: %v", platform, err)
		return &spider.GetPlatformUsersRes{Code: 1, Message: "查询失败"}, nil
	}

	// 批量取站内展示名（user 服务 GetByIds，带当前组织）
	nameMap := map[int64]string{}
	if len(rows) > 0 && s.reg != nil {
		if cli, err := userrpc.ProfileClient(&s.reg.Reg); err == nil && cli != nil {
			ids := make([]int64, 0, len(rows))
			for _, r := range rows {
				ids = append(ids, r.UserID)
			}
			var orgID int64
			if pd := auth.GetCurrentUser(ctx); pd != nil {
				orgID = int64(pd.OrgID)
			}
			res, err := cli.GetByIds(ctx, &profile.GetByIdsReq{UserIds: ids, OrgId: orgID})
			if err != nil {
				log.Warnf("GetPlatformUsers GetByIds: %v", err)
			} else {
				for _, p := range res.Profiles {
					name := strings.TrimSpace(p.Name)
					if name == "" {
						name = strings.TrimSpace(p.Username)
					}
					nameMap[p.UserId] = name
				}
			}
		}
	}

	list := make([]*spider.PlatformUserItem, 0, len(rows))
	for _, r := range rows {
		item := &spider.PlatformUserItem{
			UserId:     r.UserID,
			Name:       nameMap[r.UserID],
			Username:   nameMap[r.UserID],
			OjUsername: r.Username,
			Rating:     int32(r.Rating),
			HasRating:  r.HasRating,
		}
		// 展示名与站内用户名分离：name 无展示名时回退 username（前端展示用 name）
		list = append(list, item)
	}
	return &spider.GetPlatformUsersRes{Code: 0, Message: "success", Total: total, List: list}, nil
}

// manualRefreshDailyLimit 每个用户每日手动刷新做题记录次数（全局默认；
// 站管可按用户覆盖：user 服务 daily_refresh_quota_override，0=禁止）
const manualRefreshDailyLimit = 2

// manualRefreshDefaultIntervalMin 自动同步间隔全局默认（分钟；订阅 60 / 站管覆盖 / 组织 MIN 生效时取更小）
const manualRefreshDefaultIntervalMin = 180

// manualRefreshLoc 手动刷新限流按上海自然日
var manualRefreshLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// manualRefreshKeyTTL 计数 key 存活到次日 0 点（+1 分钟缓冲），过期自动清零
func manualRefreshKeyTTL() time.Duration {
	now := time.Now().In(manualRefreshLoc)
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, manualRefreshLoc).AddDate(0, 0, 1)
	return time.Until(next) + time.Minute
}

// RefreshSpider 用户手动增量刷新自己的 OJ 做题记录（每日限 manualRefreshDailyLimit 次；
// 且 5 分钟内仅允许一次）。计数用 Redis INCR 原子自增；超出限额返回剩余 0 并拒绝入队。
func (s SpiderService) RefreshSpider(ctx context.Context, _ *spider.RefreshSpiderReq) (*spider.RefreshSpiderRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &spider.RefreshSpiderRes{Code: 1, Message: "请先登录"}, nil
	}
	if s.rdb == nil {
		return &spider.RefreshSpiderRes{Code: 1, Message: "服务未就绪，稍后再试"}, nil
	}
	// 每日配额：优先站管个人覆盖（user 服务），失败回落全局默认（保持旧行为）
	limit := manualRefreshDailyLimit
	if s.reg != nil {
		if cli, err := userrpc.ProfileClient(&s.reg.Reg); err == nil && cli != nil {
			if q, err := cli.GetRefreshQuota(ctx, &profile.GetRefreshQuotaReq{UserId: int64(uid)}); err == nil && q != nil {
				limit = int(q.GetQuota())
			}
		}
	}
	if limit <= 0 {
		return &spider.RefreshSpiderRes{Code: 1, Message: "该用户每日手动刷新次数已用完（配额 0）", Remaining: 0}, nil
	}
	day := time.Now().In(manualRefreshLoc).Format("20060102")
	dayKey := fmt.Sprintf("spider:manual_refresh:%d:%s", uid, day)

	// 1) 5 分钟间隔限流（SETNX 原子占位，失败不消耗每日次数）
	intervalKey := fmt.Sprintf("spider:manual_refresh_interval:%d", uid)
	ok, err := s.rdb.SetNX(ctx, intervalKey, "1", 5*time.Minute).Result()
	if err != nil {
		log.Warnf("RefreshSpider setnx user=%d: %v", uid, err)
		return &spider.RefreshSpiderRes{Code: 1, Message: "刷新失败，稍后再试"}, nil
	}
	if !ok {
		used, _ := s.rdb.Get(ctx, dayKey).Int64()
		remaining := limit - int(used)
		if remaining < 0 {
			remaining = 0
		}
		return &spider.RefreshSpiderRes{
			Code:      1,
			Message:   "刷新太频繁，5 分钟内只能刷新一次",
			Remaining: int32(remaining),
		}, nil
	}

	// 2) 每日次数计数
	used, err := s.rdb.Incr(ctx, dayKey).Result()
	if err != nil {
		log.Warnf("RefreshSpider incr user=%d: %v", uid, err)
		return &spider.RefreshSpiderRes{Code: 1, Message: "刷新失败，稍后再试"}, nil
	}
	if used == 1 {
		_ = s.rdb.Expire(ctx, dayKey, manualRefreshKeyTTL()).Err()
	}
	remaining := limit - int(used)
	if remaining < 0 {
		remaining = 0
	}
	if used > int64(limit) {
		// 已用完：不入队，提示剩余次数
		return &spider.RefreshSpiderRes{
			Code:      1,
			Message:   fmt.Sprintf("每个用户每日拥有 %d 次手动刷新做题记录次数，当前还剩下 %d 次", limit, remaining),
			Remaining: int32(remaining),
		}, nil
	}
	// 增量刷新：入队该用户全部已绑定平台（needAll=false，只拉增量）
	s.spider.Do(int64(uid), false)
	return &spider.RefreshSpiderRes{
		Code:      0,
		Message:   fmt.Sprintf("已开始刷新做题记录，今日剩余 %d 次", remaining),
		Remaining: int32(remaining),
	}, nil
}

// RefreshSpiderStatus 查询今日手动刷新做题记录状态（只读）：
// 有效配额（订阅/站管覆盖合并，失败回落全局默认）、今日剩余次数、5 分钟冷却截止时间。
// userId=0 查自己；站点管理员可传 userId 查询任意用户。
func (s SpiderService) RefreshSpiderStatus(ctx context.Context, req *spider.RefreshSpiderStatusReq) (*spider.RefreshSpiderStatusRes, error) {
	uid := int64(auth.GetCurrentUserId(ctx))
	if uid == 0 {
		return &spider.RefreshSpiderStatusRes{Code: 1, Message: "请先登录"}, nil
	}
	if req != nil && req.GetUserId() > 0 {
		if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
			return &spider.RefreshSpiderStatusRes{Code: 1, Message: "需要用户同步运维权限"}, nil
		}
		uid = req.GetUserId()
	}
	if s.rdb == nil {
		return &spider.RefreshSpiderStatusRes{Code: 1, Message: "服务未就绪，稍后再试"}, nil
	}
	// 有效配额 + 生效同步间隔：user 服务合并（订阅/站管覆盖/组织 MIN），失败回落默认
	limit := manualRefreshDailyLimit
	syncIntervalMin := manualRefreshDefaultIntervalMin
	if s.reg != nil {
		if cli, err := userrpc.ProfileClient(&s.reg.Reg); err == nil && cli != nil {
			if st, err := cli.GetSyncStatus(ctx, &profile.GetSyncStatusReq{UserId: int64(uid)}); err == nil && st != nil {
				if v := int(st.GetSpiderIntervalMin()); v > 0 {
					syncIntervalMin = v
				}
				limit = int(st.GetManualRefreshQuota())
			}
		}
	}
	if limit < 0 {
		limit = 0
	}
	day := time.Now().In(manualRefreshLoc).Format("20060102")
	dayKey := fmt.Sprintf("spider:manual_refresh:%d:%s", uid, day)
	used, _ := s.rdb.Get(ctx, dayKey).Int64()
	if used < 0 {
		used = 0
	}
	remaining := limit - int(used)
	if remaining < 0 {
		remaining = 0
	}
	// 5 分钟冷却：intervalKey 的存活期即下次可刷新时间
	var nextAvailableAt int64
	intervalKey := fmt.Sprintf("spider:manual_refresh_interval:%d", uid)
	if ttl, err := s.rdb.TTL(ctx, intervalKey).Result(); err == nil && ttl > 0 {
		nextAvailableAt = time.Now().Unix() + int64(ttl/time.Second)
	}
	return &spider.RefreshSpiderStatusRes{
		Code:            0,
		Message:         "success",
		Limit:           int32(limit),
		Remaining:       int32(remaining),
		NextAvailableAt: nextAvailableAt,
		SyncIntervalMin: int32(syncIntervalMin),
	}, nil
}

// TogglePlatform 站管：暂停 / 恢复某 OJ 的提交同步或题面抓取。
func (s SpiderService) TogglePlatform(ctx context.Context, req *spider.TogglePlatformReq) (*spider.TogglePlatformRes, error) {
	module := strings.ToLower(strings.TrimSpace(req.GetModule()))
	if module == "" {
		module = "submit"
	}
	var setter func(*redis.Client, string, bool) error
	switch module {
	case "submit":
		if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
			return &spider.TogglePlatformRes{Code: 1, Message: "需要爬虫运维权限"}, nil
		}
		setter = task.SetPlatformPaused
	case "problem":
		if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
			return &spider.TogglePlatformRes{Code: 1, Message: "需要题库运维权限"}, nil
		}
		setter = task.SetProblemPlatformPaused
	default:
		return &spider.TogglePlatformRes{Code: 1, Message: "module 仅支持 submit/problem"}, nil
	}
	plat := strings.TrimSpace(req.GetPlatform())
	if _, ok := spiderregistry.Get(plat); !ok {
		return &spider.TogglePlatformRes{Code: 1, Message: "不支持的平台: " + plat}, nil
	}
	if module == "submit" && req.GetEnabled() && plat == spiderregistry.LuoGu && !luoguCrawlerAccountConfigured(ctx, s.rdb) {
		return &spider.TogglePlatformRes{Code: 1, Message: "洛谷未配置爬虫账号，无法启用提交记录同步"}, nil
	}
	if err := setter(s.rdb, plat, !req.GetEnabled()); err != nil {
		log.Warnf("SpiderService: toggle platform=%s module=%s: %v", plat, module, err)
		return &spider.TogglePlatformRes{Code: 1, Message: "操作失败"}, nil
	}
	if req.GetEnabled() {
		log.Infof("SpiderService: platform %s module %s enabled", plat, module)
		return &spider.TogglePlatformRes{Code: 0, Message: "已启用"}, nil
	}
	log.Infof("SpiderService: platform %s module %s paused", plat, module)
	return &spider.TogglePlatformRes{Code: 0, Message: "已暂停"}, nil
}

func luoguCrawlerAccountConfigured(ctx context.Context, rdb *redis.Client) bool {
	return crawlerAccountConfigured(sitesettings.Load(ctx, rdb, nil), sitesettings.ServiceLuoGu)
}

func crawlerAccountConfigured(rt *sitesettings.Runtime, service string) bool {
	if rt == nil {
		return false
	}
	switch service {
	case sitesettings.ServiceLuoGu:
		return strings.TrimSpace(rt.OjLuoguUsername) != "" && strings.TrimSpace(rt.OjLuoguPassword) != ""
	case sitesettings.ServiceQOJ:
		return strings.TrimSpace(rt.OjQojUsername) != "" && strings.TrimSpace(rt.OjQojPassword) != ""
	default:
		return true
	}
}
