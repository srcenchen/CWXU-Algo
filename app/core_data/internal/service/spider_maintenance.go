package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	spiderMaintenanceSetBinding  = "spider_set"
	spiderMaintenancePurgeUser   = "spider_purge_user"
	spiderMaintenancePurgeGlobal = "spider_purge_global"
)

type spiderMaintenanceTxOutcome uint8

const (
	spiderMaintenanceRolledBack spiderMaintenanceTxOutcome = iota
	spiderMaintenanceCommitted
	spiderMaintenanceUnknown
)

var spiderMaintenanceTransaction = func(db *gorm.DB, fn func(*gorm.DB) error) error {
	return db.Transaction(fn)
}

type spiderSetMaintenancePayload struct {
	UserID   int64  `json:"userId"`
	Platform string `json:"platform"`
	Username string `json:"username"`
}

type spiderPurgeUserMaintenancePayload struct {
	UserID int64 `json:"userId"`
}

func spiderSetMaintenanceScope(userID int64, platform string) string {
	return fmt.Sprintf("spider:set:%d:%s", userID, platform)
}

func spiderPurgeUserMaintenanceScope(userID int64) string {
	return fmt.Sprintf("spider:purge-user:%d", userID)
}

const spiderPurgeGlobalMaintenanceScope = "spider:purge-global"

func loadSpiderMaintenancePending(ctx context.Context, db *gorm.DB, scope string) (*model.AbilityMaintenancePending, error) {
	if db == nil {
		return nil, fmt.Errorf("spider maintenance: nil database")
	}
	var pending model.AbilityMaintenancePending
	err := db.WithContext(ctx).Where("scope = ?", scope).First(&pending).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pending, nil
}

func prepareSpiderMaintenancePending(ctx context.Context, db *gorm.DB, scope, operation, payload string) (*model.AbilityMaintenancePending, error) {
	if db == nil || strings.TrimSpace(scope) == "" || strings.TrimSpace(operation) == "" {
		return nil, fmt.Errorf("invalid spider maintenance intent")
	}
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: scope, OperationID: uuid.NewString(), Revision: 1, Phase: "intent",
		Operation: operation, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	res := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "scope"}}, DoNothing: true,
	}).Create(&pending)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 1 {
		return &pending, nil
	}
	existing, err := loadSpiderMaintenancePending(ctx, db, scope)
	if err != nil {
		return nil, err
	}
	if existing == nil || existing.Operation != operation || existing.Payload != payload {
		return nil, fmt.Errorf("spider maintenance scope %q has another immutable intent", scope)
	}
	return existing, nil
}

func claimSpiderMaintenancePending(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, owner string) error {
	if pending == nil || owner == "" {
		return fmt.Errorf("invalid spider maintenance owner claim")
	}
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ?", pending.Scope, pending.OperationID, pending.Revision).
		Updates(map[string]interface{}{
			"lease_owner": owner, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("spider maintenance owner changed")
	}
	pending.LeaseOwner = owner
	pending.Revision++
	return nil
}

// markSpiderMaintenanceFacts is called inside the same transaction as the
// destructive fact mutation. The caller updates its in-memory revision only
// after that transaction commits.
func markSpiderMaintenanceFacts(ctx context.Context, tx *gorm.DB, pending *model.AbilityMaintenancePending) error {
	if pending == nil || pending.LeaseOwner == "" {
		return fmt.Errorf("invalid spider maintenance facts phase")
	}
	res := tx.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
		Updates(map[string]interface{}{
			"phase": "facts", "revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("spider maintenance fact owner changed")
	}
	return nil
}

func runSpiderMaintenanceFactsTransaction(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, fn func(*gorm.DB) error) (spiderMaintenanceTxOutcome, error) {
	if db == nil || pending == nil || fn == nil {
		return spiderMaintenanceUnknown, fmt.Errorf("spider maintenance: invalid facts transaction")
	}
	expected := *pending
	txErr := spiderMaintenanceTransaction(db.WithContext(ctx), fn)
	if txErr == nil {
		pending.Phase = "facts"
		pending.Revision++
		return spiderMaintenanceCommitted, nil
	}
	stored, loadErr := loadSpiderMaintenancePending(context.Background(), db, expected.Scope)
	if loadErr != nil {
		return spiderMaintenanceUnknown, errors.Join(txErr, loadErr)
	}
	if stored != nil && stored.OperationID == expected.OperationID && stored.LeaseOwner == expected.LeaseOwner && stored.Phase == "facts" && stored.Revision == expected.Revision+1 {
		*pending = *stored
		return spiderMaintenanceCommitted, nil
	}
	if stored != nil && stored.OperationID == expected.OperationID && stored.LeaseOwner == expected.LeaseOwner && stored.Phase == expected.Phase && stored.Revision == expected.Revision {
		return spiderMaintenanceRolledBack, txErr
	}
	return spiderMaintenanceUnknown, txErr
}

func advanceSpiderMaintenancePhase(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, phase string) error {
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
		Updates(map[string]interface{}{
			"phase": phase, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("spider maintenance phase owner changed")
	}
	pending.Phase = phase
	pending.Revision++
	return nil
}

func clearSpiderMaintenancePending(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) error {
	res := db.WithContext(ctx).
		Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
		Delete(&model.AbilityMaintenancePending{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("spider maintenance clear owner changed")
	}
	return nil
}

func (s *SpiderService) prepareSpiderMaintenance(ctx context.Context, scope, operation string, payload interface{}) (*model.AbilityMaintenancePending, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if existing, err := loadSpiderMaintenancePending(ctx, s.db, scope); err != nil {
		return nil, err
	} else if existing != nil && (existing.Operation != operation || existing.Payload != string(encoded)) {
		if err := s.recoverSpiderMaintenance(ctx, existing); err != nil {
			return nil, err
		}
	}
	return prepareSpiderMaintenancePending(ctx, s.db, scope, operation, string(encoded))
}

func (s *SpiderService) settleFailedSpiderMaintenance(pending *model.AbilityMaintenancePending, token bizservice.ProfileInvalidationToken, outcome spiderMaintenanceTxOutcome, claimed, global bool) {
	if !claimed {
		if global {
			_ = bizservice.AbandonGlobalProfileInvalidation(context.Background(), s.rdb, token)
		} else {
			_ = bizservice.AbandonUserProfileInvalidation(context.Background(), s.rdb, pending.UserID, token)
		}
		return
	}
	if outcome != spiderMaintenanceRolledBack {
		if global {
			_ = bizservice.AbandonGlobalProfileInvalidation(context.Background(), s.rdb, token)
		} else {
			_ = bizservice.AbandonUserProfileInvalidation(context.Background(), s.rdb, pending.UserID, token)
		}
		return
	}
	var err error
	if global {
		err = bizservice.FinishGlobalProfileInvalidation(context.Background(), s.rdb, token)
	} else {
		err = bizservice.FinishUserProfileInvalidation(context.Background(), s.rdb, pending.UserID, token)
	}
	if err == nil {
		_ = clearSpiderMaintenancePending(context.Background(), s.db, pending)
	}
}

func (s *SpiderService) executeSetSpiderMaintenance(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	var payload spiderSetMaintenancePayload
	if err := json.Unmarshal([]byte(pending.Payload), &payload); err != nil {
		return err
	}
	pending.UserID = payload.UserID
	pending.Platform = payload.Platform
	if pending.Phase == "fence_finalized" {
		if err := claimSpiderMaintenancePending(ctx, s.db, pending, uuid.NewString()); err != nil {
			return err
		}
		return s.finishSetSpiderMaintenanceTail(ctx, pending, payload)
	}
	profileToken, err := bizservice.BeginUserProfileInvalidationForIntent(ctx, s.rdb, payload.UserID, pending.OperationID)
	if err != nil {
		return err
	}
	outcome := spiderMaintenanceRolledBack
	if pending.Phase == "facts" {
		outcome = spiderMaintenanceCommitted
	}
	claimed := false
	completed := false
	defer func() {
		if !completed {
			s.settleFailedSpiderMaintenance(pending, profileToken, outcome, claimed, false)
		}
	}()
	if err := claimSpiderMaintenancePending(ctx, s.db, pending, profileToken.Owner); err != nil {
		return err
	}
	claimed = true
	workCtx := profileToken.Context()
	validate := func(checkCtx context.Context) error {
		return bizservice.ValidateUserProfileInvalidation(checkCtx, s.rdb, payload.UserID, profileToken)
	}
	if pending.Phase == "intent" {
		if err := validate(workCtx); err != nil {
			return err
		}
		if s.spider == nil {
			return fmt.Errorf("set spider maintenance: spider task unavailable")
		}
		if _, err := bizservice.BumpUserProfileOwnedGeneration(workCtx, s.rdb, payload.UserID, profileToken, task.GenerationKey(payload.UserID, payload.Platform), 7*24*time.Hour); err != nil {
			return err
		}
		if err := validate(workCtx); err != nil {
			return err
		}
		writeUnlock, writeLocked := trySpiderPlatformWriteLock(workCtx, s.rdb, payload.UserID, payload.Platform)
		if !writeLocked {
			return fmt.Errorf("set spider maintenance: acquire platform write lock")
		}
		txOutcome, err := runSpiderMaintenanceFactsTransaction(workCtx, s.db, pending, func(tx *gorm.DB) error {
			if err := deleteSpiderPlatformData(workCtx, tx, payload.UserID, payload.Platform); err != nil {
				return err
			}
			platform := model.Platform{UserID: payload.UserID, Platform: payload.Platform, Username: payload.Username}
			if err := tx.Create(&platform).Error; err != nil {
				return err
			}
			if err := validate(workCtx); err != nil {
				return err
			}
			return markSpiderMaintenanceFacts(workCtx, tx, pending)
		})
		writeUnlock()
		outcome = txOutcome
		if err != nil {
			return err
		}
	}
	if err := validate(workCtx); err != nil {
		return err
	}
	if err := bizservice.FinishUserProfileInvalidation(workCtx, s.rdb, payload.UserID, profileToken); err != nil {
		return err
	}
	completed = true
	if err := advanceSpiderMaintenancePhase(ctx, s.db, pending, "fence_finalized"); err != nil {
		return err
	}
	return s.finishSetSpiderMaintenanceTail(ctx, pending, payload)
}

func (s *SpiderService) finishSetSpiderMaintenanceTail(ctx context.Context, pending *model.AbilityMaintenancePending, payload spiderSetMaintenancePayload) error {
	if s.spider == nil {
		return fmt.Errorf("set spider maintenance: spider task unavailable")
	}
	if s.rdb != nil {
		if err := s.rdb.Del(ctx,
			fmt.Sprintf("core:submit_log:user:%d", payload.UserID),
			fmt.Sprintf("user:%d:lastSubmitTime", payload.UserID),
			"core:platforms:bound_users:v1",
			fmt.Sprintf("core:platforms:user:%d:v1", payload.UserID),
		).Err(); err != nil {
			return err
		}
		if err := s.rdb.Incr(ctx, fmt.Sprintf("core:contest_log:user:%d:ver", payload.UserID)).Err(); err != nil {
			return err
		}
		if err := s.rdb.Incr(ctx, fmt.Sprintf("statistic:user:%d:ver", payload.UserID)).Err(); err != nil {
			return err
		}
		if err := s.rdb.Incr(ctx, "statistic:period:global:ver").Err(); err != nil {
			return err
		}
		// The forced profile rebuild is consumed only by this platform's
		// post-crawl binding pass. Keeping it out of the generic user key avoids
		// another OJ's incremental sync rebuilding too early.
		if err := task.MarkProfileRebuildAfterBinding(s.rdb, payload.UserID, payload.Platform); err != nil {
			return err
		}
	}
	s.spider.ResetDedup(payload.UserID, payload.Platform)
	result := s.spider.DoPlatform(payload.UserID, payload.Platform, true)
	if result.Failed > 0 {
		return fmt.Errorf("set spider maintenance: recrawl publish failed")
	}
	return clearSpiderMaintenancePending(ctx, s.db, pending)
}

func (s *SpiderService) executePurgeUserMaintenance(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	var payload spiderPurgeUserMaintenancePayload
	if err := json.Unmarshal([]byte(pending.Payload), &payload); err != nil {
		return err
	}
	pending.UserID = payload.UserID
	if pending.Phase == "fence_finalized" {
		if err := claimSpiderMaintenancePending(ctx, s.db, pending, uuid.NewString()); err != nil {
			return err
		}
		return clearSpiderMaintenancePending(ctx, s.db, pending)
	}
	profileToken, err := bizservice.BeginUserProfileInvalidationForIntent(ctx, s.rdb, payload.UserID, pending.OperationID)
	if err != nil {
		return err
	}
	outcome := spiderMaintenanceRolledBack
	if pending.Phase == "facts" {
		outcome = spiderMaintenanceCommitted
	}
	claimed := false
	completed := false
	defer func() {
		if !completed {
			s.settleFailedSpiderMaintenance(pending, profileToken, outcome, claimed, false)
		}
	}()
	if err := claimSpiderMaintenancePending(ctx, s.db, pending, profileToken.Owner); err != nil {
		return err
	}
	claimed = true
	workCtx := profileToken.Context()
	validate := func(checkCtx context.Context) error {
		return bizservice.ValidateUserProfileInvalidation(checkCtx, s.rdb, payload.UserID, profileToken)
	}
	if s.spider == nil {
		return fmt.Errorf("purge user maintenance: spider task unavailable")
	}
	err = withPurgeUserPlatformGuards(workCtx, purgeUserPlatforms, validate,
		func(platform string) error {
			_, bumpErr := bizservice.BumpUserProfileOwnedGeneration(workCtx, s.rdb, payload.UserID, profileToken, task.GenerationKey(payload.UserID, platform), 7*24*time.Hour)
			return bumpErr
		},
		func(lockCtx context.Context, platform string) (func(), bool) {
			return trySpiderPlatformWriteLock(lockCtx, s.rdb, payload.UserID, platform)
		},
		func() error {
			if pending.Phase == "intent" {
				txOutcome, err := s.deletePurgeUserFacts(workCtx, pending, payload.UserID, validate)
				outcome = txOutcome
				if err != nil {
					return err
				}
			}
			return s.purgeUserMaintenanceCaches(workCtx, payload.UserID, validate)
		},
	)
	if err != nil {
		return err
	}
	if err := bizservice.FinishUserProfileInvalidation(workCtx, s.rdb, payload.UserID, profileToken); err != nil {
		return err
	}
	completed = true
	if err := advanceSpiderMaintenancePhase(ctx, s.db, pending, "fence_finalized"); err != nil {
		return err
	}
	return clearSpiderMaintenancePending(ctx, s.db, pending)
}

func (s *SpiderService) deletePurgeUserFacts(ctx context.Context, pending *model.AbilityMaintenancePending, userID int64, validate func(context.Context) error) (spiderMaintenanceTxOutcome, error) {
	validateStage := func() error {
		if validate == nil {
			return nil
		}
		return validate(ctx)
	}
	return runSpiderMaintenanceFactsTransaction(ctx, s.db, pending, func(tx *gorm.DB) error {
		if err := validateStage(); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", userID).Delete(&model.Platform{}).Error; err != nil {
			return fmt.Errorf("platform: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", userID).Delete(&model.SubmitLog{}).Error; err != nil {
			return fmt.Errorf("submit_log: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", userID).Delete(&model.ContestLog{}).Error; err != nil {
			return fmt.Errorf("contest_log: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := dal.DeleteUserPreagg(ctx, tx, userID); err != nil {
			return fmt.Errorf("preagg: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		if err := purgeSpiderRepairStates(ctx, tx, userID); err != nil {
			return fmt.Errorf("spider repair state: %w", err)
		}
		if err := validateStage(); err != nil {
			return err
		}
		return markSpiderMaintenanceFacts(ctx, tx, pending)
	})
}

func (s *SpiderService) purgeUserMaintenanceCaches(ctx context.Context, userID int64, validate func(context.Context) error) error {
	validateStage := func() error {
		if validate == nil {
			return nil
		}
		return validate(ctx)
	}
	if s.rdb == nil {
		return nil
	}
	if err := validateStage(); err != nil {
		return err
	}
	if err := s.rdb.Del(ctx,
		fmt.Sprintf("core:submit_log:user:%d", userID),
		fmt.Sprintf("spider:pending:%d", userID),
		fmt.Sprintf("spider:inflight:%d", userID),
		fmt.Sprintf("spider:last_ok:%d", userID),
		fmt.Sprintf("statistic:user:%d:ver", userID),
		"core:platforms:bound_users:v1",
		fmt.Sprintf("core:platforms:user:%d:v1", userID),
	).Err(); err != nil {
		return err
	}
	if err := validateStage(); err != nil {
		return err
	}
	if err := s.rdb.Incr(ctx, "statistic:period:global:ver").Err(); err != nil {
		return err
	}
	for _, platform := range purgeUserPlatforms {
		if err := validateStage(); err != nil {
			return err
		}
		if err := s.rdb.Del(ctx,
			fmt.Sprintf("spider:pending:%d:%s", userID, platform),
			fmt.Sprintf("spider:inflight:%d:%s", userID, platform),
		).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *SpiderService) executePurgeGlobalMaintenance(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if pending.Phase == "fence_finalized" {
		if err := claimSpiderMaintenancePending(ctx, s.db, pending, uuid.NewString()); err != nil {
			return err
		}
		return s.finishPurgeGlobalMaintenanceTail(ctx, pending)
	}
	profileToken, err := bizservice.BeginGlobalProfileInvalidationForIntent(ctx, s.rdb, pending.OperationID)
	if err != nil {
		return err
	}
	outcome := spiderMaintenanceRolledBack
	if pending.Phase == "facts" {
		outcome = spiderMaintenanceCommitted
	}
	claimed := false
	completed := false
	defer func() {
		if !completed {
			s.settleFailedSpiderMaintenance(pending, profileToken, outcome, claimed, true)
		}
	}()
	if err := claimSpiderMaintenancePending(ctx, s.db, pending, profileToken.Owner); err != nil {
		return err
	}
	claimed = true
	workCtx := profileToken.Context()
	validate := func(checkCtx context.Context) error {
		return bizservice.ValidateGlobalProfileInvalidation(checkCtx, s.rdb, profileToken)
	}
	if pending.Phase == "intent" {
		var existing []string
		for _, table := range purgeSubmitTables {
			if s.db.Migrator().HasTable(table) {
				existing = append(existing, table)
			}
		}
		if len(existing) > 0 {
			txOutcome, err := purgeSubmitData(workCtx, s.db, existing, validate, pending)
			outcome = txOutcome
			if err != nil {
				return err
			}
		} else {
			txOutcome, err := runSpiderMaintenanceFactsTransaction(workCtx, s.db, pending, func(tx *gorm.DB) error {
				return markSpiderMaintenanceFacts(workCtx, tx, pending)
			})
			outcome = txOutcome
			if err != nil {
				return err
			}
		}
	}
	if err := validate(workCtx); err != nil {
		return err
	}
	if err := bizservice.FinishGlobalProfileInvalidation(workCtx, s.rdb, profileToken); err != nil {
		return err
	}
	completed = true
	if err := advanceSpiderMaintenancePhase(ctx, s.db, pending, "fence_finalized"); err != nil {
		return err
	}
	return s.finishPurgeGlobalMaintenanceTail(ctx, pending)
}

func (s *SpiderService) finishPurgeGlobalMaintenanceTail(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if s.spider == nil {
		return fmt.Errorf("purge global maintenance: spider task unavailable")
	}
	var userIDs []int64
	if err := s.db.WithContext(ctx).Model(&model.Platform{}).Distinct("user_id").Pluck("user_id", &userIDs).Error; err != nil {
		return err
	}
	if err := s.purgeTrainingCaches(ctx, userIDs); err != nil {
		return err
	}
	type binding struct {
		UserID   int64
		Platform string
	}
	var bindings []binding
	if err := s.db.WithContext(ctx).Model(&model.Platform{}).Select("user_id, platform").Order("user_id, platform").Find(&bindings).Error; err != nil {
		return err
	}
	for _, binding := range bindings {
		s.spider.ResetDedup(binding.UserID, binding.Platform)
		result := s.spider.DoPlatform(binding.UserID, binding.Platform, true)
		if result.Failed > 0 {
			return fmt.Errorf("purge global maintenance: recrawl publish failed user=%d platform=%s", binding.UserID, binding.Platform)
		}
	}
	return clearSpiderMaintenancePending(ctx, s.db, pending)
}

func (s *SpiderService) recoverSpiderMaintenance(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if pending == nil {
		return nil
	}
	switch pending.Operation {
	case spiderMaintenanceSetBinding:
		return s.executeSetSpiderMaintenance(ctx, pending)
	case spiderMaintenancePurgeUser:
		return s.executePurgeUserMaintenance(ctx, pending)
	case spiderMaintenancePurgeGlobal:
		return s.executePurgeGlobalMaintenance(ctx, pending)
	default:
		return fmt.Errorf("unknown spider maintenance operation %q", pending.Operation)
	}
}

func (s *SpiderService) recoverPendingSpiderMaintenance(ctx context.Context) {
	if s == nil || s.db == nil || !s.db.Migrator().HasTable(&model.AbilityMaintenancePending{}) {
		return
	}
	pending, err := dal.LoadAbilityMaintenanceRecoveryBatch(ctx, s.db, []string{
		spiderMaintenanceSetBinding, spiderMaintenancePurgeUser, spiderMaintenancePurgeGlobal,
	}, 50)
	if err != nil {
		log.Warnf("spider maintenance recovery scan: %v", err)
		return
	}
	for i := range pending {
		claimed, touchErr := dal.TouchAbilityMaintenanceRecoveryAttempt(ctx, s.db, &pending[i], time.Now())
		if touchErr != nil {
			log.Warnf("spider maintenance recovery touch scope=%s intent=%s: %v", pending[i].Scope, pending[i].OperationID, touchErr)
			continue
		}
		if !claimed {
			continue
		}
		if recoverErr := s.recoverSpiderMaintenance(ctx, &pending[i]); recoverErr != nil {
			log.Warnf("spider maintenance recovery scope=%s intent=%s: %v", pending[i].Scope, pending[i].OperationID, recoverErr)
		}
	}
}

func (s *SpiderService) runSpiderMaintenanceRecovery() {
	s.recoverPendingSpiderMaintenance(context.Background())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.recoverPendingSpiderMaintenance(context.Background())
	}
}
