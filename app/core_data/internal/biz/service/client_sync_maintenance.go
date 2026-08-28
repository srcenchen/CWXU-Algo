package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// A single browser session can cover up to 100k records at the normal
	// Luogu page size while remaining strictly bounded across restarts.
	clientSyncMaxReceiptsPerSession = 5000
	clientSyncMaintenanceInterval   = time.Minute
	clientSyncEffectBatchSize       = 100
	clientSyncPostProcessBatchSize  = 16
	clientSyncReceiptCleanupBatch   = 500
	clientSyncJobLease              = 5 * time.Minute
	clientSyncCompletedJobRetention = 24 * time.Hour
)

// RunClientSyncMaintenance is a fixed-frequency worker. Fixed constants avoid
// adding another deployment config contract while keeping work per tick
// bounded. runForever in core_data main restarts it after panic/return.
func (uc *SpiderUseCase) RunClientSyncMaintenance(stop <-chan struct{}) {
	if stop == nil {
		return
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := uc.RunClientSyncMaintenanceOnce(ctx, time.Now().UTC()); err != nil {
			log.Errorf("client-sync maintenance failed: %v", err)
		}
	}
	run()
	ticker := time.NewTicker(clientSyncMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			run()
		}
	}
}

// RunClientSyncMaintenanceOnce replays committed receipt side effects, claims
// due dirty-session jobs with a cross-replica SQL lease, and deletes one
// bounded batch of retained rows. It continues independent phases so one
// transient Redis failure cannot block SQL retention forever.
func (uc *SpiderUseCase) RunClientSyncMaintenanceOnce(ctx context.Context, now time.Time) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil {
		return fmt.Errorf("client-sync maintenance is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now = now.UTC()
	var errs []error
	if err := uc.replayClientSyncReceiptEffects(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := uc.processClientSyncPostProcessJobs(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := uc.cleanupClientSyncReceipts(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := uc.cleanupClientSyncJobs(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (uc *SpiderUseCase) reserveClientSyncReceipt(ctx context.Context, tx *gorm.DB, userID int64, platform string, page ClientSyncPageImport, dirty bool) error {
	readyAt := page.ExpiresAt.UTC()
	if page.CompletionReason != "" {
		readyAt = page.CompletedAt.UTC()
	}
	var existingReceipts int64
	if err := tx.WithContext(ctx).Model(&model.ClientSyncPageReceipt{}).
		Where("session_id = ?", page.SessionID).Count(&existingReceipts).Error; err != nil {
		return err
	}
	if existingReceipts > clientSyncMaxReceiptsPerSession {
		existingReceipts = clientSyncMaxReceiptsPerSession
	}
	job := model.ClientSyncPostProcessJob{
		SessionID: page.SessionID, UserID: userID, Platform: platform,
		ReceiptCount: int32(existingReceipts), ReadyAt: readyAt,
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
		return err
	}
	reserved := tx.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
		Where("session_id = ? AND receipt_count < ?", page.SessionID, clientSyncMaxReceiptsPerSession).
		UpdateColumn("receipt_count", gorm.Expr("receipt_count + 1"))
	if reserved.Error != nil {
		return reserved.Error
	}
	if reserved.RowsAffected != 1 {
		return fmt.Errorf("client sync receipt limit: %w", errClientSyncReceiptLimit)
	}
	return tx.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
		Where("session_id = ?", page.SessionID).
		Updates(map[string]interface{}{
			"user_id": userID, "platform": platform, "ready_at": readyAt,
			"dirty": gorm.Expr("dirty OR ?", dirty),
		}).Error
}

var errClientSyncReceiptLimit = errors.New("client sync receipt limit reached")

func (uc *SpiderUseCase) MarkClientSyncSessionTerminated(ctx context.Context, sessionID string, at time.Time) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	return uc.data.DB.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
		Where("session_id = ? AND dirty = ? AND completed_at IS NULL", sessionID, true).
		Update("ready_at", at.UTC()).Error
}

func (uc *SpiderUseCase) applyClientSyncReceiptEffects(ctx context.Context, receipt *model.ClientSyncPageReceipt) error {
	if receipt == nil || receipt.EffectsAppliedAt != nil {
		return nil
	}
	if receipt.PageInserted > 0 {
		if err := uc.invalidateClientSyncCache(ctx, receipt.UserID); err != nil {
			return err
		}
	}
	if receipt.HasPending {
		if err := uc.schedulePendingVerdictRetryStrict(ctx, receipt.UserID, receipt.Platform); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	updated := uc.data.DB.WithContext(ctx).Model(&model.ClientSyncPageReceipt{}).
		Where("session_id = ? AND restart = ? AND page = ? AND effects_applied_at IS NULL", receipt.SessionID, receipt.Restart, receipt.Page).
		Update("effects_applied_at", now)
	if updated.Error != nil {
		return updated.Error
	}
	receipt.EffectsAppliedAt = &now
	return nil
}

func (uc *SpiderUseCase) invalidateClientSyncCache(ctx context.Context, userID int64) error {
	if uc == nil || uc.data == nil || uc.data.RDB == nil {
		return fmt.Errorf("client-sync cache invalidation unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rdb := uc.data.RDB
	_, err := rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx,
			fmt.Sprintf("core:submit_log:user:%d", userID),
			fmt.Sprintf("user:%d:lastSubmitTime", userID),
			fmt.Sprintf("core:contest_log:user:%d", userID),
		)
		pipe.Incr(ctx, fmt.Sprintf("statistic:user:%d:ver", userID))
		pipe.Incr(ctx, fmt.Sprintf("core:contest_log:user:%d:ver", userID))
		pipe.Incr(ctx, "statistic:heatmap:global:ver")
		pipe.Incr(ctx, "statistic:period:global:ver")
		pipe.Incr(ctx, "core:submit_feed:global:ver")
		pipe.Incr(ctx, "core:contest:list:global:ver")
		return nil
	})
	if err != nil {
		return fmt.Errorf("client-sync cache invalidation: %w", err)
	}
	return nil
}

func (uc *SpiderUseCase) schedulePendingVerdictRetryStrict(ctx context.Context, userID int64, platform string) error {
	if uc.spiderTask == nil {
		return nil
	}
	if uc.data == nil || uc.data.RDB == nil {
		return fmt.Errorf("pending-verdict redis unavailable")
	}
	rdb := uc.data.RDB
	ok, err := rdb.SetNX(ctx, pendingVerdictScheduleKey(userID, platform), "1", pendingVerdictScheduleTTL).Result()
	if err != nil || !ok {
		return err
	}
	round, err := rdb.Incr(ctx, pendingVerdictRoundKey(userID, platform)).Result()
	if err != nil {
		_ = rdb.Del(ctx, pendingVerdictScheduleKey(userID, platform)).Err()
		return err
	}
	_ = rdb.Expire(ctx, pendingVerdictRoundKey(userID, platform), pendingVerdictMaxAge).Err()
	if round > pendingVerdictMaxRounds {
		_ = rdb.Del(ctx, pendingVerdictScheduleKey(userID, platform)).Err()
		return nil
	}
	if err := rdb.ZAdd(ctx, pendingVerdictDueZKey, redis.Z{
		Score:  float64(time.Now().Add(pendingVerdictRetryDelay).Unix()),
		Member: pendingVerdictMember(userID, platform),
	}).Err(); err != nil {
		_ = rdb.Del(ctx, pendingVerdictScheduleKey(userID, platform)).Err()
		return err
	}
	return nil
}

func (uc *SpiderUseCase) replayClientSyncReceiptEffects(ctx context.Context, _ time.Time) error {
	var receipts []model.ClientSyncPageReceipt
	if err := uc.data.DB.WithContext(ctx).
		Where("(page_inserted > 0 OR has_pending = ?) AND effects_applied_at IS NULL", true).
		Order("created_at ASC").Limit(clientSyncEffectBatchSize).Find(&receipts).Error; err != nil {
		return err
	}
	var errs []error
	for i := range receipts {
		if err := uc.applyClientSyncReceiptEffects(ctx, &receipts[i]); err != nil {
			log.Warnf("client-sync receipt effects retry session=%s restart=%d page=%d: %v", receipts[i].SessionID, receipts[i].Restart, receipts[i].Page, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (uc *SpiderUseCase) processClientSyncPostProcessJobs(ctx context.Context, now time.Time) error {
	var jobs []model.ClientSyncPostProcessJob
	if err := uc.data.DB.WithContext(ctx).
		Where("dirty = ? AND completed_at IS NULL AND ready_at <= ? AND (lease_until IS NULL OR lease_until <= ?)", true, now, now).
		Order("ready_at ASC").Limit(clientSyncPostProcessBatchSize).Find(&jobs).Error; err != nil {
		return err
	}
	var errs []error
	for i := range jobs {
		leaseUntil := now.Add(clientSyncJobLease)
		claimed := uc.data.DB.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
			Where("session_id = ? AND dirty = ? AND completed_at IS NULL AND ready_at <= ? AND (lease_until IS NULL OR lease_until <= ?)", jobs[i].SessionID, true, now, now).
			Updates(map[string]interface{}{
				"lease_until": leaseUntil,
				"attempts":    gorm.Expr("attempts + 1"),
			})
		if claimed.Error != nil {
			errs = append(errs, claimed.Error)
			continue
		}
		if claimed.RowsAffected != 1 {
			continue
		}
		err := uc.runClientSyncPostProcess(jobs[i].UserID)
		if err == nil {
			completedAt := time.Now().UTC()
			if updateErr := uc.data.DB.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
				Where("session_id = ?", jobs[i].SessionID).
				Updates(map[string]interface{}{
					"completed_at": completedAt, "lease_until": nil, "last_error": "",
				}).Error; updateErr != nil {
				errs = append(errs, updateErr)
			}
			continue
		}
		retryAt := now.Add(clientSyncRetryDelay(jobs[i].Attempts + 1))
		lastError := err.Error()
		if len(lastError) > 512 {
			lastError = lastError[:512]
		}
		_ = uc.data.DB.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
			Where("session_id = ?", jobs[i].SessionID).
			Updates(map[string]interface{}{
				"ready_at": retryAt, "lease_until": nil, "last_error": lastError,
			}).Error
		log.Warnf("client-sync postprocess retry session=%s user=%d attempt=%d at=%s: %v", jobs[i].SessionID, jobs[i].UserID, jobs[i].Attempts+1, retryAt.Format(time.RFC3339), err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func clientSyncRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		attempt = 10
	}
	return time.Duration(attempt) * time.Minute
}

func (uc *SpiderUseCase) runClientSyncPostProcess(userID int64) error {
	if uc.problem == nil {
		return fmt.Errorf("problem postprocess is not configured")
	}
	return uc.problem.BindSubmitsAfterSpider(userID)
}

func (uc *SpiderUseCase) cleanupClientSyncReceipts(ctx context.Context, now time.Time) error {
	var receipts []model.ClientSyncPageReceipt
	if err := uc.data.DB.WithContext(ctx).
		Where("expires_at <= ? AND ((page_inserted = 0 AND has_pending = ?) OR effects_applied_at IS NOT NULL)", now, false).
		Order("expires_at ASC").Limit(clientSyncReceiptCleanupBatch).Find(&receipts).Error; err != nil {
		return err
	}
	if len(receipts) == 0 {
		return nil
	}
	err := uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range receipts {
			if err := tx.Where("session_id = ? AND restart = ? AND page = ?", receipts[i].SessionID, receipts[i].Restart, receipts[i].Page).
				Delete(&model.ClientSyncPageReceipt{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		log.Infof("client-sync receipt cleanup deleted=%d cutoff=%s", len(receipts), now.Format(time.RFC3339))
	}
	return err
}

func (uc *SpiderUseCase) cleanupClientSyncJobs(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-clientSyncCompletedJobRetention)
	condition := "(completed_at IS NOT NULL AND completed_at <= ?) OR (dirty = ? AND ready_at <= ?)"
	var sessionIDs []string
	if err := uc.data.DB.WithContext(ctx).Model(&model.ClientSyncPostProcessJob{}).
		Where(condition, cutoff, false, now).
		Order("ready_at ASC").Limit(clientSyncReceiptCleanupBatch).
		Pluck("session_id", &sessionIDs).Error; err != nil {
		return err
	}
	if len(sessionIDs) == 0 {
		return nil
	}
	deleted := uc.data.DB.WithContext(ctx).
		Where("session_id IN ?", sessionIDs).
		Where(condition, cutoff, false, now).
		Delete(&model.ClientSyncPostProcessJob{})
	if deleted.Error == nil && deleted.RowsAffected > 0 {
		log.Infof("client-sync job cleanup deleted=%d cutoff=%s", deleted.RowsAffected, now.Format(time.RFC3339))
	}
	return deleted.Error
}
