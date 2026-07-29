package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/spider"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/ratelimit"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	spiderregistry "cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

var (
	SetForbidden    = errors.Forbidden("权限错误", "权限不允许，设置失败")
	InternalError   = errors.InternalServer("内部错误", "内部错误，操作失败")
	UpdateForbidden = errors.Forbidden("权限错误", "仅站点管理员可手动同步 OJ 数据")
	RateLimitError  = errors.New(429, "TOO_MANY_REQUESTS", "请求过于频繁，请稍后再试")
)

type SpiderService struct {
	spider.UnimplementedSpiderServer
	db     *gorm.DB
	rdb    *redis.Client
	spider *task.SpiderTask
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

// RegisterSpiderExtraRoutes 站管：按平台全量回填（如力扣比赛记录）
func RegisterSpiderExtraRoutes(srv *khttp.Server, s *SpiderService) {
	if srv == nil || s == nil {
		return
	}
	r := srv.Route("/")
	r.POST("/v1/core/spider/update-platform", s.handleUpdatePlatform)
	// 站内榜 cell-submits 脏数据修复（external_id / 赛后练习格 / relative_sec）
	r.POST("/v1/core/spider/repair-contest-cells", s.handleRepairContestCells)
}

// handleRepairContestCells 幂等修复 AtCoder 赛时提交明细相关脏数据（仅站管）。
func (s *SpiderService) handleRepairContestCells(ctx khttp.Context) error {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		writeSpiderJSON(ctx, 403, map[string]interface{}{"success": false, "message": "仅管理员可操作"})
		return nil
	}
	// 顺带规范日历 platform 大小写
	if s.db != nil {
		_ = dal.NewContestCalendarDalDB(s.db).NormalizeLegacyPlatformNames()
	}
	stats, err := bizservice.RepairContestCellSubmitData(s.db)
	if err != nil {
		log.Errorf("repair-contest-cells: %v", err)
		writeSpiderJSON(ctx, 500, map[string]interface{}{"success": false, "message": err.Error()})
		return nil
	}
	writeSpiderJSON(ctx, 200, map[string]interface{}{
		"success": true,
		"message": "ok",
		"data":    stats,
	})
	return nil
}

// handleUpdatePlatform body: { "platform": "LeetCode" }
// 仅入队该平台已绑定用户的 needAll 任务，并强制清去重（避免与刚跑完的 update-all 撞 pending）。
func (s *SpiderService) handleUpdatePlatform(ctx khttp.Context) error {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		writeSpiderJSON(ctx, 403, map[string]interface{}{"code": 1, "message": "仅站点管理员可操作", "count": 0})
		return nil
	}
	adminId := int64(auth.GetCurrentUserId(ctx))
	if !s.allow(ctx, ratelimit.SpiderUpdateAllKey(adminId)+":plat", 2*time.Minute) {
		writeSpiderJSON(ctx, 429, map[string]interface{}{"code": 1, "message": "请求过于频繁，请稍后再试", "count": 0})
		return nil
	}
	var req struct {
		Platform string `json:"platform"`
	}
	body, _ := io.ReadAll(ctx.Request().Body)
	_ = json.Unmarshal(body, &req)
	plat := strings.TrimSpace(req.Platform)
	if plat == "" {
		writeSpiderJSON(ctx, 400, map[string]interface{}{"code": 1, "message": "缺少 platform", "count": 0})
		return nil
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
		writeSpiderJSON(ctx, 400, map[string]interface{}{"code": 1, "message": "不支持的平台: " + plat, "count": 0})
		return nil
	}
	users, published := 0, 0
	if s.spider != nil {
		users, published = s.spider.DoBatchPlatform(context.Background(), plat, true, true)
	}
	writeSpiderJSON(ctx, 200, map[string]interface{}{
		"code":      0,
		"message":   fmt.Sprintf("已为平台 %s 的 %d 名用户入队全量同步（published=%d），后台抓取中", plat, users, published),
		"count":     users,
		"published": published,
		"platform":  plat,
	})
	return nil
}

func writeSpiderJSON(ctx khttp.Context, status int, v interface{}) {
	w := ctx.Response()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
	if !s.allow(ctx, ratelimit.SpiderSetKey(req.UserId), 30*time.Second) {
		return nil, RateLimitError
	}
	platformName := strings.TrimSpace(req.Platform)
	username := strings.TrimSpace(req.Username)
	if _, ok := spiderregistry.Get(platformName); !ok {
		return nil, errors.BadRequest("参数错误", "不支持该 OJ 平台")
	}
	if username == "" || len([]rune(username)) > 128 {
		return nil, errors.BadRequest("参数错误", "OJ 用户名不能为空且最多 128 个字符")
	}
	platform := model.Platform{
		UserID:   req.UserId,
		Platform: platformName,
		Username: username,
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND platform = ?", req.UserId, platformName).Delete(&model.Platform{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND platform = ?", req.UserId, platformName).Delete(&model.SubmitLog{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND platform = ?", req.UserId, platformName).Delete(&model.ContestLog{}).Error; err != nil {
			return err
		}
		// 按平台剪枝预聚合，再全量重爬该平台
		if err := dal.DeletePlatformDailyStats(ctx, tx, req.UserId, platformName); err != nil {
			return err
		}
		if err := dal.DeletePlatformUserAC(ctx, tx, req.UserId, platformName); err != nil {
			return err
		}
		return tx.Create(&platform).Error
	}); err != nil {
		log.Errorf("SetSpider transaction failed: %v", err)
		return nil, InternalError
	}
	// 缓存与统计版本：立即让首页题量/热力读到重建后的数
	if err := s.rdb.Del(ctx,
		fmt.Sprintf("core:submit_log:user:%d", req.UserId),
		fmt.Sprintf("user:%d:lastSubmitTime", req.UserId),
		"core:platforms:bound_users:v1",
		fmt.Sprintf("core:platforms:user:%d:v1", req.UserId),
	).Err(); err != nil {
		log.Errorf("SetSpider: redis del failed: %v", err)
	}
	_ = s.rdb.Incr(ctx, fmt.Sprintf("core:contest_log:user:%d:ver", req.UserId)).Err()
	_ = s.rdb.Incr(ctx, fmt.Sprintf("statistic:user:%d:ver", req.UserId)).Err()
	// 递增代数：正在跑的旧全量任务写入前会发现代数过期并丢弃，避免把已删数据写回叠统计
	s.spider.BumpGeneration(req.UserId, platformName)
	// 强制允许本次入队（旧全量任务可能仍占 pending/inflight）
	s.spider.ResetDedup(req.UserId, platformName)
	// 只全量抓取刚绑定的这一平台，避免重绑 CF 时把其它 OJ 再扫一遍
	// 后台入队：MQ confirm 可达秒级，绑定 HTTP 路径不阻塞
	go s.spider.DoPlatform(req.UserId, platformName, true)
	return &spider.SetSpiderRep{
		Code:    0,
		Message: fmt.Sprintf("绑定成功，正在同步 %s 的全量数据，请稍候", platformName),
	}, nil
}

const purgeSubmitsConfirm = "PURGE_SUBMITS"

// SubmitInventory 运维：真实入库提交库存（仅站点管理员）
func (s SpiderService) SubmitInventory(ctx context.Context, _ *spider.SubmitInventoryReq) (*spider.SubmitInventoryRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteSpiderOps) {
		return nil, errors.Forbidden("权限不足", "仅站点管理员可查看提交库存")
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
	countTable := func(name string) int64 {
		if !s.db.Migrator().HasTable(name) {
			return 0
		}
		var n int64
		_ = s.db.WithContext(ctx).Table(name).Count(&n).Error
		return n
	}
	deletedLogs := countTable("submit_logs")
	deletedDaily := countTable("daily_user_stats")
	deletedAc := countTable("user_ac_problems") + countTable("user_ac_problem_days")
	deletedContests := countTable("contest_logs")

	// 仅允许白名单表名，防注入
	toTruncate := []string{
		"submit_logs",
		"daily_user_stats",
		"user_ac_problems",
		"user_ac_problem_days",
		"contest_logs",
		"contest_calendar_notify_logs",
	}
	var existing []string
	for _, t := range toTruncate {
		if s.db.Migrator().HasTable(t) {
			existing = append(existing, t)
		}
	}
	if len(existing) > 0 {
		// TRUNCATE 硬删 + 重置序列
		sql := "TRUNCATE TABLE " + strings.Join(existing, ", ") + " RESTART IDENTITY"
		if err := s.db.WithContext(ctx).Exec(sql).Error; err != nil {
			log.Errorf("purge TRUNCATE failed: %v", err)
			// 回退分批 DELETE
			for _, t := range existing {
				if _, err := deleteAllInBatches(ctx, s.db, t, 5000); err != nil {
					log.Errorf("purge delete %s: %v", t, err)
					return nil, InternalError
				}
			}
		}
	}
	var userIds []int64
	if err := s.db.Model(&model.Platform{}).
		Distinct("user_id").
		Pluck("user_id", &userIds).Error; err != nil {
		log.Errorf("purge recrawl list users: %v", err)
		return nil, InternalError
	}

	// Redis：全局 ver + 每用户训练相关缓存/爬虫锁
	s.purgeTrainingCaches(ctx, userIds)

	// 全部入队全量重爬（写路径会重新灌 submit_logs + daily/user_ac）
	go s.spider.DoBatch(context.Background(), userIds, true, 30, 200*time.Millisecond)

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
func (s SpiderService) purgeTrainingCaches(ctx context.Context, userIds []int64) {
	if s.rdb == nil {
		return
	}
	_ = s.rdb.Incr(ctx, "statistic:heatmap:global:ver").Err()
	_ = s.rdb.Incr(ctx, "statistic:period:global:ver").Err()

	plats := []string{"AtCoder", "Codeforces", "LuoGu", "NowCoder", "QOJ", "LeetCode", "CodeForces", "LOJ", "UOJ"}
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
				log.Warnf("purge redis del chunk: %v", err)
			}
		}
	}
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
	if db == nil || table == "" {
		return 0, nil
	}
	// 白名单
	switch table {
	case "submit_logs", "daily_user_stats",
		"user_ac_problems", "user_ac_problem_days", "contest_logs",
		"contest_calendar_notify_logs":
	default:
		return 0, fmt.Errorf("refuse delete table %s", table)
	}
	if batch <= 0 {
		batch = 5000
	}
	var total int64
	for {
		res := db.WithContext(ctx).Exec(fmt.Sprintf(`
			DELETE FROM %s
			WHERE ctid IN (
				SELECT ctid FROM %s LIMIT %d
			)
		`, table, table, batch))
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
	if err := s.db.WithContext(ctx).Where("user_id = ?", uid).Delete(&model.Platform{}).Error; err != nil {
		log.Errorf("PurgeUserData: platform user=%d: %v", uid, err)
		return nil, InternalError
	}
	if err := s.db.WithContext(ctx).Where("user_id = ?", uid).Delete(&model.SubmitLog{}).Error; err != nil {
		log.Errorf("PurgeUserData: submit_log user=%d: %v", uid, err)
		return nil, InternalError
	}
	if err := s.db.WithContext(ctx).Where("user_id = ?", uid).Delete(&model.ContestLog{}).Error; err != nil {
		log.Errorf("PurgeUserData: contest_log user=%d: %v", uid, err)
		return nil, InternalError
	}
	if err := dal.DeleteUserPreagg(ctx, s.db, uid); err != nil {
		log.Errorf("PurgeUserData: preagg user=%d: %v", uid, err)
		return nil, InternalError
	}
	// 缓存 / 爬虫 inflight / 上次同步
	keys := []string{
		fmt.Sprintf("core:submit_log:user:%d", uid),
		fmt.Sprintf("spider:pending:%d", uid),
		fmt.Sprintf("spider:inflight:%d", uid),
		fmt.Sprintf("spider:last_ok:%d", uid),
		fmt.Sprintf("user:%d:profile", uid),
		fmt.Sprintf("statistic:user:%d:ver", uid),
		"core:platforms:bound_users:v1",
		fmt.Sprintf("core:platforms:user:%d:v1", uid),
	}
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		log.Warnf("PurgeUserData: redis del user=%d: %v", uid, err)
	}
	// 按平台的 pending/inflight 键（扫描可能较多，仅删常见平台前缀）
	for _, p := range []string{"AtCoder", "Codeforces", "LuoGu", "NowCoder", "QOJ", "LeetCode", "LOJ", "UOJ"} {
		_ = s.rdb.Del(ctx,
			fmt.Sprintf("spider:pending:%d:%s", uid, p),
			fmt.Sprintf("spider:inflight:%d:%s", uid, p),
			fmt.Sprintf("spider:writelock:%d:%s", uid, p),
		).Err()
	}
	return &spider.PurgeUserDataRes{Code: 0, Message: "已清空该用户的训练与绑定数据"}, nil
}

func NewSpiderService(data *data.Data, spider *task.SpiderTask) *SpiderService {
	// 进程启动清除残留 purge 锁（上次崩溃 / 未 defer 的旧版本）
	if data != nil {
		ClearPurgeLock(data.RDB)
	}
	return &SpiderService{
		db:     data.DB,
		rdb:    data.RDB,
		spider: spider,
	}
}
