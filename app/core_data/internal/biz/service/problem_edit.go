package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/mailqueue"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// normalizeEditTags 去空白、去重、限长
func normalizeEditTags(tags []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len([]rune(t)) > 32 {
			t = string([]rune(t)[:32])
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func nonEmptyTags(tags model.StringArray) []string {
	return normalizeEditTags([]string(tags))
}

type userProfileInvalidationToken struct {
	userID int64
	token  ProfileInvalidationToken
}

const problemFactsDirtyPrefix = "ability_facts_dirty:"

var problemFactsTransaction = func(db *gorm.DB, fn func(*gorm.DB) error) error {
	return db.Transaction(fn)
}

func problemFactsDirtyFlags(message string) (tags, difficulty bool) {
	message = strings.TrimSpace(message)
	if !strings.HasPrefix(message, problemFactsDirtyPrefix) {
		return false, false
	}
	kind := strings.TrimPrefix(message, problemFactsDirtyPrefix)
	return strings.Contains(kind, "tags"), strings.Contains(kind, "difficulty")
}

func (uc *ProblemUseCase) markProblemFactsDirty(ctx context.Context, pending *model.AbilityMaintenancePending, problemID uint, tagsChanged, difficultyChanged bool) error {
	if pending == nil {
		return fmt.Errorf("problem facts dirty: missing maintenance owner")
	}
	parts := make([]string, 0, 2)
	if tagsChanged {
		parts = append(parts, "tags")
	}
	if difficultyChanged {
		parts = append(parts, "difficulty")
	}
	if len(parts) == 0 {
		parts = append(parts, "profile")
	}
	expected := *pending
	return uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAbilityMaintenanceParent(ctx, tx, &expected); err != nil {
			return err
		}
		res := tx.Model(&model.Problem{}).Where("id = ?", problemID).Updates(map[string]interface{}{
			"status": model.ProblemStatusTagging, "error_msg": problemFactsDirtyPrefix + strings.Join(parts, "+"),
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("problem facts dirty: expected one problem row, updated %d", res.RowsAffected)
		}
		return nil
	})
}

func problemMaintenanceScope(problemID uint) string {
	return fmt.Sprintf("problem:%d", problemID)
}

type problemMaintenancePayload struct {
	Updates           map[string]interface{} `json:"updates"`
	Tags              []string               `json:"tags"`
	TagsChanged       bool                   `json:"tagsChanged"`
	DifficultyChanged bool                   `json:"difficultyChanged"`
}

func decodeProblemMaintenancePayload(raw string) (problemMaintenancePayload, error) {
	var payload problemMaintenancePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return payload, err
	}
	if payload.Updates == nil {
		payload.Updates = map[string]interface{}{}
	}
	if _, ok := payload.Updates["tags"]; ok {
		payload.Updates["tags"] = model.StringArray(payload.Tags)
	}
	return payload, nil
}

func ensureAbilityMaintenancePending(ctx context.Context, tx *gorm.DB, pending model.AbilityMaintenancePending) (*model.AbilityMaintenancePending, bool, error) {
	if tx == nil || strings.TrimSpace(pending.Scope) == "" {
		return nil, false, fmt.Errorf("invalid ability maintenance pending")
	}
	if pending.OperationID == "" {
		pending.OperationID = uuid.NewString()
	}
	if pending.Phase == "" {
		pending.Phase = "intent"
	}
	if pending.Revision == 0 {
		pending.Revision = 1
	}
	res := tx.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "scope"}}, DoNothing: true}).Create(&pending)
	if res.Error != nil {
		return nil, false, res.Error
	}
	if res.RowsAffected == 1 {
		return &pending, true, nil
	}
	existing, err := loadAbilityMaintenancePending(ctx, tx, pending.Scope)
	return existing, false, err
}

func claimAbilityMaintenancePending(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, owner string) error {
	if pending == nil || owner == "" {
		return fmt.Errorf("invalid ability maintenance owner claim")
	}
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ?", pending.Scope, pending.OperationID, pending.Revision).
		Updates(map[string]interface{}{"lease_owner": owner, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ability maintenance owner changed")
	}
	pending.LeaseOwner = owner
	pending.Revision++
	return nil
}

func markAbilityMaintenanceFacts(ctx context.Context, tx *gorm.DB, pending *model.AbilityMaintenancePending, tagsChanged, difficultyChanged bool) error {
	res := tx.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
		Updates(map[string]interface{}{
			"phase": "facts", "tags_changed": tagsChanged, "difficulty_changed": difficultyChanged,
			"revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ability maintenance fact owner changed")
	}
	pending.Phase = "facts"
	pending.TagsChanged = tagsChanged
	pending.DifficultyChanged = difficultyChanged
	pending.Revision++
	return nil
}

func writeAbilityMaintenanceModelReady(ctx context.Context, db *gorm.DB, pending model.AbilityMaintenancePending, modelVersion uint64) error {
	if db == nil || modelVersion == 0 {
		return fmt.Errorf("invalid ability maintenance model ready transition")
	}
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ? AND phase = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision, "facts").
		Updates(map[string]interface{}{
			"phase": "model_ready", "target_model_version": modelVersion,
			"revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ability maintenance model phase owner changed")
	}
	return nil
}

func (uc *ProblemUseCase) refreshAbilityStatsForMaintenance(ctx context.Context, pending *model.AbilityMaintenancePending) (uint64, error) {
	if uc == nil || uc.abilityStats == nil || pending == nil {
		return 0, fmt.Errorf("ability maintenance: refresher unavailable")
	}
	refresher, ok := uc.abilityStats.(task.AbilityStatsMaintenanceRefresher)
	if !ok {
		return 0, fmt.Errorf("ability maintenance: atomic refresher unavailable")
	}
	expected := *pending
	version, refreshErr := refresher.RefreshForMaintenance(ctx, func(callbackCtx context.Context, tx *gorm.DB, modelVersion uint64) error {
		if tx == nil {
			tx = uc.data.DB
		}
		return writeAbilityMaintenanceModelReady(callbackCtx, tx, expected, modelVersion)
	})
	stored, loadErr := loadAbilityMaintenancePending(context.Background(), uc.data.DB, expected.Scope)
	if loadErr != nil {
		return 0, errors.Join(refreshErr, loadErr)
	}
	if stored != nil && stored.OperationID == expected.OperationID && stored.Phase == "model_ready" && stored.Revision == expected.Revision+1 && stored.TargetModelVersion > 0 {
		*pending = *stored
		return stored.TargetModelVersion, nil
	}
	if refreshErr != nil {
		return 0, refreshErr
	}
	return version, fmt.Errorf("ability maintenance: model switch committed without durable MODEL_READY")
}

func loadAbilityMaintenancePending(ctx context.Context, db *gorm.DB, scope string) (*model.AbilityMaintenancePending, error) {
	if db == nil {
		return nil, fmt.Errorf("ability maintenance: nil database")
	}
	if !db.Migrator().HasTable(&model.AbilityMaintenancePending{}) {
		return nil, nil
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

func clearAbilityMaintenancePending(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) error {
	if pending == nil {
		return fmt.Errorf("invalid ability maintenance clear")
	}
	res := db.WithContext(ctx).Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
		Delete(&model.AbilityMaintenancePending{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ability maintenance clear owner changed")
	}
	return nil
}

func advanceAbilityMaintenancePhase(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, phase string) error {
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ? AND lease_owner = ?", pending.Scope, pending.OperationID, pending.Revision, pending.LeaseOwner).
		Updates(map[string]interface{}{"phase": phase, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ability maintenance phase owner changed")
	}
	pending.Phase = phase
	pending.Revision++
	return nil
}

func lockAbilityMaintenanceParent(ctx context.Context, tx *gorm.DB, expected *model.AbilityMaintenancePending) error {
	if tx == nil || expected == nil {
		return fmt.Errorf("ability maintenance: invalid parent owner")
	}
	var current model.AbilityMaintenancePending
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ? AND phase = ?", expected.Scope, expected.OperationID, expected.LeaseOwner, expected.Revision, expected.Phase).
		First(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("ability maintenance parent owner changed")
	}
	return err
}

// AdvanceAbilityMaintenanceTarget binds a target transition to the exact
// current parent attempt. A takeover increments parent revision, so an old
// owner can never commit progress after the new owner has claimed the intent.
func AdvanceAbilityMaintenanceTarget(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, target *model.AbilityMaintenanceTarget, nextTargetState, nextParentPhase, messagePayload string) error {
	if db == nil || pending == nil || target == nil || nextTargetState == "" {
		return fmt.Errorf("ability maintenance: invalid target transition")
	}
	expectedParent := *pending
	expectedTarget := *target
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAbilityMaintenanceParent(ctx, tx, &expectedParent); err != nil {
			return err
		}
		updates := map[string]interface{}{
			"state": nextTargetState, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
		}
		if messagePayload != "" {
			updates["message_payload"] = messagePayload
		}
		res := tx.Model(&model.AbilityMaintenanceTarget{}).
			Where("intent_id = ? AND user_id = ? AND revision = ? AND state = ?", expectedTarget.IntentID, expectedTarget.UserID, expectedTarget.Revision, expectedTarget.State).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("ability maintenance target owner changed")
		}
		if nextParentPhase != "" {
			res = tx.Model(&model.AbilityMaintenancePending{}).
				Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ? AND phase = ?", expectedParent.Scope, expectedParent.OperationID, expectedParent.LeaseOwner, expectedParent.Revision, expectedParent.Phase).
				Updates(map[string]interface{}{"phase": nextParentPhase, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("ability maintenance phase owner changed")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	target.State = nextTargetState
	target.Revision++
	if messagePayload != "" {
		target.MessagePayload = messagePayload
	}
	if nextParentPhase != "" {
		pending.Phase = nextParentPhase
		pending.Revision++
	}
	return nil
}

func prepareAbilityMaintenanceRebuildTargets(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, userIDs []int64) error {
	expected := *pending
	next := expected
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAbilityMaintenanceParent(ctx, tx, &expected); err != nil {
			return err
		}
		for _, userID := range userIDs {
			target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: userID, Revision: 1, State: "pending"}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&target).Error; err != nil {
				return err
			}
		}
		return advanceAbilityMaintenancePhase(ctx, tx, &next, "targets_ready")
	})
	if err == nil {
		*pending = next
	}
	return err
}

func rebuildPendingAbilityMaintenanceTargets(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, validate func() error, rebuild func(int64) error) error {
	if pending == nil || rebuild == nil {
		return fmt.Errorf("ability maintenance: invalid target rebuild")
	}
	var targets []model.AbilityMaintenanceTarget
	if err := db.WithContext(ctx).Where("intent_id = ? AND state = ?", pending.OperationID, "pending").Order("user_id ASC").Find(&targets).Error; err != nil {
		return err
	}
	for i := range targets {
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		if err := rebuild(targets[i].UserID); err != nil {
			return err
		}
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		if err := AdvanceAbilityMaintenanceTarget(ctx, db, pending, &targets[i], "rebuilt", "", ""); err != nil {
			return err
		}
	}
	return nil
}

func stageRebuiltAbilityMaintenanceTargets(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) error {
	expected := *pending
	next := expected
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAbilityMaintenanceParent(ctx, tx, &expected); err != nil {
			return err
		}
		var remaining int64
		if err := tx.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ? AND state <> ?", pending.OperationID, "rebuilt").Count(&remaining).Error; err != nil {
			return err
		}
		if remaining != 0 {
			return fmt.Errorf("ability maintenance targets remain unrebuilt")
		}
		var targets []model.AbilityMaintenanceTarget
		if err := tx.Where("intent_id = ? AND state = ?", pending.OperationID, "rebuilt").Order("user_id ASC").Find(&targets).Error; err != nil {
			return err
		}
		for i := range targets {
			payload, err := json.Marshal(event.UserProfileEvent{UserId: targets[i].UserID, Force: true, IntentID: pending.OperationID})
			if err != nil {
				return err
			}
			res := tx.Model(&model.AbilityMaintenanceTarget{}).
				Where("intent_id = ? AND user_id = ? AND revision = ? AND state = ?", targets[i].IntentID, targets[i].UserID, targets[i].Revision, "rebuilt").
				Updates(map[string]interface{}{
					"state": "outbox_ready", "message_payload": string(payload),
					"revision": gorm.Expr("revision + 1"), "updated_at": time.Now(),
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("ability maintenance target stage owner changed")
			}
		}
		return advanceAbilityMaintenancePhase(ctx, tx, &next, "derived_ready")
	})
	if err == nil {
		*pending = next
	}
	return err
}

const abilityMaintenanceDeliveryLease = 24 * time.Hour

const abilityMaintenanceRelayLease = 5 * time.Minute

// claimAbilityMaintenanceRelay serializes the MQ publication pass for one
// durable parent. The revision fence prevents a stale scanner from publishing
// the same ready target after another scanner has acquired the relay lease.
func claimAbilityMaintenanceRelay(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) (bool, error) {
	if db == nil || pending == nil {
		return false, fmt.Errorf("invalid ability maintenance relay lease")
	}
	now := time.Now()
	owner := uuid.NewString()
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ?", pending.Scope, pending.OperationID, pending.Revision).
		Where("relay_lease_until IS NULL OR relay_lease_until <= ?", now).
		Updates(map[string]interface{}{
			"relay_lease_owner": owner, "relay_lease_until": now.Add(abilityMaintenanceRelayLease),
			"revision": gorm.Expr("revision + 1"), "updated_at": now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	pending.RelayLeaseOwner = owner
	pending.RelayLeaseUntil = now.Add(abilityMaintenanceRelayLease)
	pending.Revision++
	return true, nil
}

func releaseAbilityMaintenanceRelay(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) error {
	if db == nil || pending == nil || pending.RelayLeaseOwner == "" {
		return nil
	}
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ? AND relay_lease_owner = ?", pending.Scope, pending.OperationID, pending.Revision, pending.RelayLeaseOwner).
		Updates(map[string]interface{}{"relay_lease_owner": "", "relay_lease_until": time.Time{}, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	pending.RelayLeaseOwner = ""
	pending.RelayLeaseUntil = time.Time{}
	return nil
}

// renewAbilityMaintenanceRelay is called before every target publish. It
// proves the scanner still owns the exact parent revision and extends only its
// own short lease; a takeover stops the old scanner before the next message.
func renewAbilityMaintenanceRelay(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending) error {
	if db == nil || pending == nil || pending.RelayLeaseOwner == "" {
		return fmt.Errorf("invalid ability maintenance relay owner")
	}
	now := time.Now()
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ? AND relay_lease_owner = ? AND relay_lease_until > ?", pending.Scope, pending.OperationID, pending.Revision, pending.RelayLeaseOwner, now).
		Updates(map[string]interface{}{"relay_lease_until": now.Add(abilityMaintenanceRelayLease), "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ability maintenance relay owner changed")
	}
	pending.RelayLeaseUntil = now.Add(abilityMaintenanceRelayLease)
	return nil
}

func (uc *ProblemUseCase) publishAbilityMaintenanceTargets(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	var targets []model.AbilityMaintenanceTarget
	now := time.Now()
	if err := uc.data.DB.WithContext(ctx).
		Where("intent_id = ? AND (state = ? OR (state = ? AND next_retry_at <= ?))", pending.OperationID, "outbox_ready", "delivered", now).
		Order("user_id ASC").Find(&targets).Error; err != nil {
		return err
	}
	if len(targets) > 0 && uc.profileTask == nil {
		return fmt.Errorf("ability maintenance: profile publisher unavailable")
	}
	for i := range targets {
		if pending.RelayLeaseOwner != "" {
			if err := renewAbilityMaintenanceRelay(ctx, uc.data.DB, pending); err != nil {
				return err
			}
		}
		result := uc.profileTask.DoMaintenanceForce(targets[i].UserID, targets[i].IntentID)
		if result.Failed {
			res := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
				Where("intent_id = ? AND user_id = ? AND revision = ? AND state = ?", targets[i].IntentID, targets[i].UserID, targets[i].Revision, targets[i].State).
				Updates(map[string]interface{}{
					"publish_attempts": gorm.Expr("publish_attempts + 1"), "last_error": "profile publish failed",
					"next_retry_at": now.Add(time.Minute), "revision": gorm.Expr("revision + 1"),
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				var current model.AbilityMaintenanceTarget
				if err := uc.data.DB.WithContext(ctx).First(&current, "intent_id = ? AND user_id = ?", targets[i].IntentID, targets[i].UserID).Error; err != nil {
					return err
				}
				if current.State != "consumed" {
					return fmt.Errorf("ability maintenance target retry owner changed")
				}
			}
			if res.RowsAffected == 1 {
				return fmt.Errorf("ability maintenance: profile publish failed for user %d", targets[i].UserID)
			}
			continue
		}
		res := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
			Where("intent_id = ? AND user_id = ? AND revision = ? AND state = ?", targets[i].IntentID, targets[i].UserID, targets[i].Revision, targets[i].State).
			Updates(map[string]interface{}{
				"state": "delivered", "publish_attempts": gorm.Expr("publish_attempts + 1"), "last_error": "",
				"next_retry_at": now.Add(abilityMaintenanceDeliveryLease), "revision": gorm.Expr("revision + 1"), "updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			var current model.AbilityMaintenanceTarget
			if err := uc.data.DB.WithContext(ctx).First(&current, "intent_id = ? AND user_id = ?", targets[i].IntentID, targets[i].UserID).Error; err != nil {
				return err
			}
			if current.State != "consumed" {
				return fmt.Errorf("ability maintenance target owner changed")
			}
		}
	}
	return nil
}

func (uc *ProblemUseCase) completeAbilityMaintenanceTargets(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	return uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var remaining int64
		if err := tx.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ? AND state <> ?", pending.OperationID, "consumed").Count(&remaining).Error; err != nil {
			return err
		}
		if remaining != 0 {
			return fmt.Errorf("ability maintenance targets remain unconsumed")
		}
		if err := tx.Where("intent_id = ?", pending.OperationID).Delete(&model.AbilityMaintenanceTarget{}).Error; err != nil {
			return err
		}
		return clearAbilityMaintenancePending(ctx, tx, pending)
	})
}

func (uc *ProblemUseCase) abilityMaintenanceTargetsConsumed(ctx context.Context, pending *model.AbilityMaintenancePending) (bool, error) {
	var remaining int64
	if err := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
		Where("intent_id = ? AND state <> ?", pending.OperationID, "consumed").Count(&remaining).Error; err != nil {
		return false, err
	}
	return remaining == 0, nil
}

// ConfirmAbilityMaintenanceTarget records the consumer's successful build. It
// accepts an ack that races the producer's delivered transition and accepts
// duplicates after the parent has already been cleaned up.
func (uc *ProblemUseCase) ConfirmAbilityMaintenanceTarget(ctx context.Context, intentID string, userID int64) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil || intentID == "" || userID <= 0 {
		return fmt.Errorf("invalid ability maintenance confirmation")
	}
	res := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
		Where("intent_id = ? AND user_id = ? AND state IN ?", intentID, userID, []string{"outbox_ready", "delivered"}).
		Updates(map[string]interface{}{"state": "consumed", "last_error": "", "next_retry_at": time.Time{}, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 0 {
		return nil
	}
	var target model.AbilityMaintenanceTarget
	err := uc.data.DB.WithContext(ctx).First(&target, "intent_id = ? AND user_id = ?", intentID, userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && target.State == "consumed") {
		return nil
	}
	return err
}

// MarkAbilityMaintenanceTargetDue is invoked immediately before MQ discards a
// failed maintenance message. Its durable deadline lets the DB scanner replay
// that exact intent, while normal queued messages retain their long lease.
func (uc *ProblemUseCase) MarkAbilityMaintenanceTargetDue(ctx context.Context, intentID string, userID int64) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil || intentID == "" || userID <= 0 {
		return fmt.Errorf("invalid ability maintenance retry")
	}
	res := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
		Where("intent_id = ? AND user_id = ? AND state IN ?", intentID, userID, []string{"outbox_ready", "delivered"}).
		Updates(map[string]interface{}{"next_retry_at": time.Now(), "last_error": "profile consumer retries exhausted", "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 0 {
		return nil
	}
	var target model.AbilityMaintenanceTarget
	err := uc.data.DB.WithContext(ctx).First(&target, "intent_id = ? AND user_id = ?", intentID, userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && target.State == "consumed") {
		return nil
	}
	return err
}

func (uc *ProblemUseCase) relayAbilityMaintenanceTargets(ctx context.Context, pending *model.AbilityMaintenancePending) (bool, error) {
	claimed, err := claimAbilityMaintenanceRelay(ctx, uc.data.DB, pending)
	if err != nil || !claimed {
		return false, err
	}
	defer func() {
		if err := releaseAbilityMaintenanceRelay(context.Background(), uc.data.DB, pending); err != nil {
			log.Warnf("ability maintenance relay release scope=%s intent=%s: %v", pending.Scope, pending.OperationID, err)
		}
	}()
	if err := uc.publishAbilityMaintenanceTargets(ctx, pending); err != nil {
		return false, err
	}
	completed, err := uc.abilityMaintenanceTargetsConsumed(ctx, pending)
	if err != nil || !completed {
		return completed, err
	}
	return true, uc.completeAbilityMaintenanceTargets(ctx, pending)
}

// RelayAbilityMaintenanceTargets exposes the durable outbox relay to narrow
// maintenance callers. It owns the relay lease, delivery deadline, consumer
// acknowledgement race, and parent completion protocol.
func (uc *ProblemUseCase) RelayAbilityMaintenanceTargets(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil || pending == nil {
		return fmt.Errorf("ability maintenance relay unavailable")
	}
	var current model.AbilityMaintenancePending
	err := uc.data.DB.WithContext(ctx).
		Where("scope = ? AND operation_id = ? AND revision = ? AND operation = ? AND phase = ?", pending.Scope, pending.OperationID, pending.Revision, "luogu_cleanup", "tail_finalized").
		First(&current).Error
	if err != nil {
		return fmt.Errorf("ability maintenance relay parent changed: %w", err)
	}
	var targetCount, intentTargetCount int64
	if err := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
		Where("intent_id = ? AND user_id = ?", current.OperationID, current.UserID).Count(&targetCount).Error; err != nil {
		return err
	}
	if err := uc.data.DB.WithContext(ctx).Model(&model.AbilityMaintenanceTarget{}).
		Where("intent_id = ?", current.OperationID).Count(&intentTargetCount).Error; err != nil {
		return err
	}
	if targetCount != 1 || intentTargetCount != 1 {
		return fmt.Errorf("ability maintenance relay target changed")
	}
	_, err = uc.relayAbilityMaintenanceTargets(ctx, &current)
	return err
}

func (uc *ProblemUseCase) completeProblemMaintenanceTail(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if pending == nil || pending.Operation != "problem" {
		return fmt.Errorf("invalid problem maintenance tail")
	}
	if pending.Phase == "fence_finalized" {
		expected := *pending
		next := expected
		if err := uc.data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := lockAbilityMaintenanceParent(ctx, tx, &expected); err != nil {
				return err
			}
			if err := tx.Model(&model.Problem{}).Where("id = ?", pending.ProblemID).
				Where("error_msg LIKE ?", problemFactsDirtyPrefix+"%").Update("error_msg", "").Error; err != nil {
				return err
			}
			return advanceAbilityMaintenancePhase(ctx, tx, &next, "dirty_cleared")
		}); err != nil {
			return err
		}
		*pending = next
	}
	if pending.Phase == "dirty_cleared" {
		if pending.TagsChanged && uc.data != nil && uc.data.RDB != nil {
			if _, err := uc.data.RDB.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Incr(ctx, problemTagsVerKey)
				pipe.Incr(ctx, problemListVerKey)
				return nil
			}); err != nil {
				return err
			}
		}
		if err := advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "cache_tail_done"); err != nil {
			return err
		}
	}
	if pending.Phase == "cache_tail_done" {
		_, err := uc.relayAbilityMaintenanceTargets(ctx, pending)
		return err
	}
	return fmt.Errorf("problem maintenance tail: unexpected phase %q", pending.Phase)
}

func sameNormalizedTags(a, b []string) bool {
	aa, bb := dal.NormalizeTags(a), dal.NormalizeTags(b)
	if len(aa) != len(bb) {
		return false
	}
	seen := make(map[string]struct{}, len(aa))
	for _, tag := range aa {
		seen[tag] = struct{}{}
	}
	for _, tag := range bb {
		if _, ok := seen[tag]; !ok {
			return false
		}
	}
	return true
}

func (uc *ProblemUseCase) beginUserProfileInvalidations(ctx context.Context, userIDs []int64, intentID string) ([]userProfileInvalidationToken, error) {
	tokens := make([]userProfileInvalidationToken, 0, len(userIDs))
	ownerID := uuid.NewString()
	for _, userID := range userIDs {
		token, err := beginProfileInvalidationForAttemptWithTTL(ctx, uc.data.RDB, profileUserGenerationKey(userID), intentID, ownerID, profileInvalidationLeaseTTL)
		if err != nil {
			_ = uc.finishUserProfileInvalidations(context.Background(), tokens)
			return nil, err
		}
		tokens = append(tokens, userProfileInvalidationToken{userID: userID, token: token})
	}
	return tokens, nil
}

func beginGlobalProfileInvalidationWithRetry(ctx context.Context, rdb *redis.Client, intentID string) (ProfileInvalidationToken, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		token, err := beginGlobalProfileInvalidationForIntent(ctx, rdb, intentID)
		if err == nil {
			return token, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "profile invalidation intent changed") && !strings.Contains(err.Error(), "already in progress") {
			return ProfileInvalidationToken{}, err
		}
		select {
		case <-ctx.Done():
			return ProfileInvalidationToken{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ProfileInvalidationToken{}, lastErr
}

func (uc *ProblemUseCase) finishUserProfileInvalidations(ctx context.Context, tokens []userProfileInvalidationToken) error {
	var errs []error
	for _, item := range tokens {
		if err := FinishUserProfileInvalidation(ctx, uc.data.RDB, item.userID, item.token); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (uc *ProblemUseCase) abandonUserProfileInvalidations(ctx context.Context, tokens []userProfileInvalidationToken) error {
	var errs []error
	for _, item := range tokens {
		if err := AbandonUserProfileInvalidation(ctx, uc.data.RDB, item.userID, item.token); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (uc *ProblemUseCase) allCanonicalACUsers(ctx context.Context) ([]int64, error) {
	var userIDs []int64
	err := uc.data.DB.WithContext(ctx).Model(&model.UserACProblem{}).Distinct("user_id").Order("user_id ASC").Pluck("user_id", &userIDs).Error
	return userIDs, err
}

func (uc *ProblemUseCase) applyProblemFactUpdates(ctx context.Context, p *model.Problem, updates map[string]interface{}, tags []string, tagsChanged, difficultyChanged bool) error {
	return uc.applyProblemFactUpdatesWithPending(ctx, p, updates, tags, tagsChanged, difficultyChanged, nil)
}

func (uc *ProblemUseCase) applyProblemFactUpdatesWithPending(ctx context.Context, p *model.Problem, updates map[string]interface{}, tags []string, tagsChanged, difficultyChanged bool, prepared *model.AbilityMaintenancePending) error {
	if p == nil || p.ID == 0 {
		return fmt.Errorf("invalid problem facts")
	}
	if !tagsChanged && !difficultyChanged {
		res := uc.data.DB.WithContext(ctx).Model(&model.Problem{}).Where("id = ?", p.ID).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("problem facts: expected one problem row, updated %d", res.RowsAffected)
		}
		return nil
	}
	pending := prepared
	var err error
	if pending == nil {
		payloadBytes, err := json.Marshal(problemMaintenancePayload{Updates: updates, Tags: tags, TagsChanged: tagsChanged, DifficultyChanged: difficultyChanged})
		if err != nil {
			return err
		}
		var created bool
		pending, created, err = ensureAbilityMaintenancePending(ctx, uc.data.DB, model.AbilityMaintenancePending{
			Scope: problemMaintenanceScope(p.ID), ProblemID: p.ID, Operation: "problem", Payload: string(payloadBytes),
			TagsChanged: tagsChanged, DifficultyChanged: difficultyChanged,
		})
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("problem maintenance already pending")
		}
	}
	tagsChanged = tagsChanged || pending.TagsChanged
	difficultyChanged = difficultyChanged || pending.DifficultyChanged
	var userIDs []int64
	globalToken, err := beginGlobalProfileInvalidationWithRetry(ctx, uc.data.RDB, pending.OperationID)
	if err != nil {
		return err
	}
	abandonCommitted := func() error {
		return AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, globalToken)
	}
	finishWithoutCommit := func() error {
		return FinishGlobalProfileInvalidation(context.Background(), uc.data.RDB, globalToken)
	}
	owner := globalToken.Owner
	if owner == "" {
		owner = uuid.NewString()
	}
	if err := claimAbilityMaintenancePending(ctx, uc.data.DB, pending, owner); err != nil {
		return errors.Join(err, abandonCommitted())
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	if globalToken.lease != nil {
		go func() {
			select {
			case <-globalToken.Context().Done():
				cancelWork()
			case <-workCtx.Done():
			}
		}()
	}
	validateOwner := func(checkCtx context.Context) error {
		return validateProfileInvalidation(checkCtx, uc.data.RDB, profileGlobalGenerationKey, globalToken)
	}
	if err := validateOwner(workCtx); err != nil {
		return errors.Join(err, abandonCommitted())
	}
	failCommitted := func(cause error) error {
		markerErr := uc.markProblemFactsDirty(context.Background(), pending, p.ID, tagsChanged, difficultyChanged)
		return errors.Join(cause, markerErr, abandonCommitted())
	}
	if pending.Phase == "intent" {
		expected := *pending
		txPending := expected
		txErr := problemFactsTransaction(uc.data.DB.WithContext(workCtx), func(tx *gorm.DB) error {
			res := tx.Model(&model.Problem{}).Where("id = ?", p.ID).Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("problem facts: expected one problem row, updated %d", res.RowsAffected)
			}
			if tagsChanged {
				if _, _, err := dal.SyncProblemTags(workCtx, tx, p.ID, tags); err != nil {
					return err
				}
			}
			return markAbilityMaintenanceFacts(workCtx, tx, &txPending, tagsChanged, difficultyChanged)
		})
		if txErr != nil {
			stored, loadErr := loadAbilityMaintenancePending(context.Background(), uc.data.DB, pending.Scope)
			if loadErr != nil {
				return errors.Join(txErr, loadErr, abandonCommitted())
			}
			if stored != nil && stored.OperationID == expected.OperationID && stored.LeaseOwner == expected.LeaseOwner && stored.Phase == "facts" && stored.Revision == expected.Revision+1 {
				*pending = *stored
			} else if stored != nil && stored.OperationID == expected.OperationID && stored.LeaseOwner == expected.LeaseOwner && stored.Phase == "intent" && stored.Revision == expected.Revision {
				if ownerErr := validateOwner(context.Background()); ownerErr != nil {
					return errors.Join(txErr, ownerErr, abandonCommitted())
				}
				finishErr := finishWithoutCommit()
				if finishErr != nil {
					return errors.Join(txErr, finishErr)
				}
				clearErr := clearAbilityMaintenancePending(context.Background(), uc.data.DB, &expected)
				return errors.Join(txErr, clearErr)
			} else {
				return errors.Join(txErr, abandonCommitted())
			}
		} else {
			*pending = txPending
		}
	}
	if err := validateOwner(workCtx); err != nil {
		return failCommitted(err)
	}
	if difficultyChanged && pending.Phase == "facts" {
		if uc.abilityStats == nil {
			return failCommitted(fmt.Errorf("problem facts: ability refresher unavailable"))
		}
		modelVersion, err := uc.refreshAbilityStatsForMaintenance(workCtx, pending)
		if err != nil {
			return failCommitted(err)
		}
		if err := validateOwner(workCtx); err != nil {
			return failCommitted(err)
		}
		_ = modelVersion
	}
	if pending.Phase == "facts" || pending.Phase == "model_ready" {
		if difficultyChanged {
			userIDs, err = uc.allCanonicalACUsers(workCtx)
		} else {
			userIDs, err = dal.ListUsersACProblem(workCtx, uc.data.DB, p.ID)
		}
		if err != nil {
			return failCommitted(err)
		}
		if err := prepareAbilityMaintenanceRebuildTargets(workCtx, uc.data.DB, pending, userIDs); err != nil {
			return failCommitted(err)
		}
	}
	if pending.Phase == "targets_ready" {
		if err := rebuildPendingAbilityMaintenanceTargets(workCtx, uc.data.DB, pending, func() error {
			return validateOwner(workCtx)
		}, func(userID int64) error {
			return dal.RebuildUserTagACForUser(workCtx, uc.data.DB, userID)
		}); err != nil {
			return failCommitted(err)
		}
		if err := validateOwner(workCtx); err != nil {
			return failCommitted(err)
		}
		if err := stageRebuiltAbilityMaintenanceTargets(workCtx, uc.data.DB, pending); err != nil {
			return failCommitted(err)
		}
	}
	if err := validateOwner(workCtx); err != nil {
		return failCommitted(err)
	}
	err = FinishGlobalProfileInvalidation(workCtx, uc.data.RDB, globalToken)
	if err != nil {
		return failCommitted(err)
	}
	if err := advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "fence_finalized"); err != nil {
		return err
	}
	return uc.completeProblemMaintenanceTail(ctx, pending)
}

func (uc *ProblemUseCase) recoverProblemMaintenance(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	if pending == nil || pending.Operation != "problem" {
		return fmt.Errorf("invalid problem maintenance recovery")
	}
	if pending.Phase == "fence_finalized" || pending.Phase == "dirty_cleared" || pending.Phase == "cache_tail_done" {
		return uc.completeProblemMaintenanceTail(ctx, pending)
	}
	if pending.Phase == "derived_ready" {
		token, err := beginGlobalProfileInvalidationForIntent(ctx, uc.data.RDB, pending.OperationID)
		if err != nil {
			return err
		}
		if err := claimAbilityMaintenancePending(ctx, uc.data.DB, pending, token.Owner); err != nil {
			return errors.Join(err, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, token))
		}
		if err := FinishGlobalProfileInvalidation(token.Context(), uc.data.RDB, token); err != nil {
			return errors.Join(err, AbandonGlobalProfileInvalidation(context.Background(), uc.data.RDB, token))
		}
		if err := advanceAbilityMaintenancePhase(ctx, uc.data.DB, pending, "fence_finalized"); err != nil {
			return err
		}
		return uc.completeProblemMaintenanceTail(ctx, pending)
	}
	payload, err := decodeProblemMaintenancePayload(pending.Payload)
	if err != nil {
		return err
	}
	var p model.Problem
	if err := uc.data.DB.WithContext(ctx).First(&p, pending.ProblemID).Error; err != nil {
		return err
	}
	return uc.applyProblemFactUpdatesWithPending(ctx, &p, payload.Updates, payload.Tags, payload.TagsChanged, payload.DifficultyChanged, pending)
}

func (uc *ProblemUseCase) recoverAbilityMaintenancePending(ctx context.Context) {
	if uc == nil || uc.data == nil || uc.data.DB == nil || !uc.data.DB.Migrator().HasTable(&model.AbilityMaintenancePending{}) {
		return
	}
	generalOperations := []string{"problem", "rebuild", "reset"}
	knownOperations := []string{"problem", "rebuild", "reset", "luogu_cleanup", "spider_set", "spider_purge_user", "spider_purge_global"}
	if unknown, err := dal.ListUnknownAbilityMaintenanceOperations(ctx, uc.data.DB, knownOperations, 10); err != nil {
		log.Warnf("ability maintenance unknown-operation scan: %v", err)
	} else {
		for _, item := range unknown {
			log.Warnf("ability maintenance unknown operation=%q pending=%d (isolated)", item.Operation, item.Count)
		}
	}
	pending, err := dal.LoadAbilityMaintenanceRecoveryBatch(ctx, uc.data.DB, generalOperations, 50)
	if err != nil {
		log.Warnf("ability maintenance recovery scan: %v", err)
		return
	}
	for i := range pending {
		claimed, touchErr := dal.TouchAbilityMaintenanceRecoveryAttempt(ctx, uc.data.DB, &pending[i], time.Now())
		if touchErr != nil {
			log.Warnf("ability maintenance recovery touch scope=%s intent=%s: %v", pending[i].Scope, pending[i].OperationID, touchErr)
			continue
		}
		if !claimed {
			continue
		}
		var recoverErr error
		switch pending[i].Operation {
		case "problem":
			recoverErr = uc.recoverProblemMaintenance(ctx, &pending[i])
		case "rebuild":
			_, _, recoverErr = uc.RebuildAllUserProfiles(ctx)
		case "reset":
			_, _, _, _, recoverErr = uc.ResetAll(false)
		default:
			recoverErr = fmt.Errorf("unknown ability maintenance operation %q", pending[i].Operation)
		}
		if recoverErr != nil {
			log.Warnf("ability maintenance recovery scope=%s intent=%s: %v", pending[i].Scope, pending[i].OperationID, recoverErr)
		}
	}
}

func (uc *ProblemUseCase) runAbilityMaintenanceRecovery() {
	uc.recoverAbilityMaintenancePending(context.Background())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		uc.recoverAbilityMaintenancePending(context.Background())
	}
}

// normalizeEditDifficulty 校验难度：简单|中等|困难；空串表示清空。
func normalizeEditDifficulty(d string) (string, error) {
	d = strings.TrimSpace(d)
	switch d {
	case "", "简单", "中等", "困难":
		return d, nil
	default:
		return "", fmt.Errorf("难度须为 简单 / 中等 / 困难")
	}
}

// ApplyProblemFields 应用标签/题面/难度修改，并按规则更新状态与 AI 入队。
// updateTags / updateContent / updateDifficulty 为 true 时才写入对应字段（允许清空标签/难度）。
// 规则：
//   - 标签非空 + 有题面 → COMPLETED（后续 AI 跳过）
//   - 有题面 + 标签空 → TAGGING 并入队分析
//   - 仅标签无题面 → 保留/回到 PENDING，不入队 AI
func (uc *ProblemUseCase) ApplyProblemFields(problemID uint, updateTags bool, tags []string, updateContent bool, contentMD, title string, updateDifficulty bool, difficulty string) (*model.Problem, error) {
	if !updateTags && !updateContent && strings.TrimSpace(title) == "" && !updateDifficulty {
		return nil, fmt.Errorf("没有需要修改的内容")
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return nil, fmt.Errorf("题目不存在")
	}

	updates := map[string]interface{}{}
	tagsChanged := false
	if updateTags {
		tags = normalizeEditTags(tags)
		tagsChanged = !sameNormalizedTags([]string(p.Tags), tags)
		updates["tags"] = model.StringArray(tags)
	}
	if updateContent {
		updates["content_md"] = strings.TrimSpace(contentMD)
	}
	if t := strings.TrimSpace(title); t != "" {
		updates["title"] = t
	}
	if updateDifficulty {
		d, err := normalizeEditDifficulty(difficulty)
		if err != nil {
			return nil, err
		}
		updates["difficulty"] = d
	}
	if len(updates) == 0 {
		return &p, nil
	}
	oldStatus := p.Status
	dirtyTags, dirtyDifficulty := problemFactsDirtyFlags(p.ErrorMsg)
	pending, err := loadAbilityMaintenancePending(context.Background(), uc.data.DB, problemMaintenanceScope(p.ID))
	if err != nil {
		return nil, err
	}
	if pending != nil {
		if err := uc.recoverProblemMaintenance(context.Background(), pending); err != nil {
			return nil, err
		}
		if err := uc.data.DB.First(&p, problemID).Error; err != nil {
			return nil, err
		}
		dirtyTags, dirtyDifficulty = problemFactsDirtyFlags(p.ErrorMsg)
		if updateTags {
			tagsChanged = !sameNormalizedTags([]string(p.Tags), tags)
		}
	}
	tagsChanged = tagsChanged || dirtyTags
	difficultyChanged := (updateDifficulty && strings.TrimSpace(p.Difficulty) != strings.TrimSpace(fmt.Sprint(updates["difficulty"]))) || dirtyDifficulty
	if tagsChanged && !updateTags {
		tags = []string(p.Tags)
	}
	if err := uc.applyProblemFactUpdates(context.Background(), &p, updates, tags, tagsChanged, difficultyChanged); err != nil {
		return nil, err
	}
	// 重新加载
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return nil, err
	}

	hasTags := len(nonEmptyTags(p.Tags)) > 0
	hasContent := strings.TrimSpace(p.ContentMD) != ""
	statusUpdates := map[string]interface{}{}
	needAnalyze := false
	newStatus := oldStatus

	switch {
	case hasContent && hasTags:
		// 人工已补齐：完成，后续 AI 跳过
		statusUpdates["status"] = model.ProblemStatusCompleted
		statusUpdates["error_msg"] = ""
		newStatus = model.ProblemStatusCompleted
	case hasContent && !hasTags:
		// 有题面无标签：仍需 AI 分析
		if p.Status != model.ProblemStatusSkipped {
			statusUpdates["status"] = model.ProblemStatusTagging
			statusUpdates["error_msg"] = ""
			needAnalyze = true
			newStatus = model.ProblemStatusTagging
		}
	case !hasContent && hasTags:
		// 仅有标签：等题面；不强制 COMPLETED
		if p.Status == model.ProblemStatusFailed || p.Status == model.ProblemStatusFailedPerm ||
			p.Status == model.ProblemStatusTagging || p.Status == model.ProblemStatusCompleted {
			// 题面仍缺：回到待爬取（若平台可爬）
			if p.Status != model.ProblemStatusSkipped {
				statusUpdates["status"] = model.ProblemStatusPending
				statusUpdates["error_msg"] = "标签已人工填写，待题面"
				newStatus = model.ProblemStatusPending
			}
		}
	}

	if len(statusUpdates) > 0 {
		if err := uc.data.DB.Model(&p).Updates(statusUpdates).Error; err != nil {
			return nil, err
		}
		for k, v := range statusUpdates {
			switch k {
			case "status":
				p.Status = v.(string)
			case "error_msg":
				p.ErrorMsg = v.(string)
			}
		}
	}

	if needAnalyze {
		if err := uc.enqueueAnalyze(p.ID); err != nil {
			log.Warnf("ApplyProblemFields enqueue analyze id=%d: %v", p.ID, err)
		}
	}
	uc.BumpProblemDetailVer(p.ID)
	if newStatus != oldStatus {
		uc.progressMoveStatus(oldStatus, newStatus)
	}
	return &p, nil
}

// ProposeProblemEdit 用户提交审核（同题仅允许一条 pending）。
// autoApprove=true 时（站管/资源审核员）创建记录后立即通过：写 approved、应用字段、贡献统计计入；
// 申请人=审核人时跳过感谢站内信/邮件，避免自己通知自己。
func (uc *ProblemUseCase) ProposeProblemEdit(userID, problemID uint, updateTags bool, tags []string, updateContent bool, contentMD, title, note string, updateDifficulty bool, difficulty string, autoApprove bool) (uint, error) {
	if userID == 0 {
		return 0, fmt.Errorf("请先登录")
	}
	title = strings.TrimSpace(title)
	if !updateTags && !updateContent && title == "" && !updateDifficulty {
		return 0, fmt.Errorf("请至少修改标签、题面、标题或难度")
	}
	if updateTags {
		tags = normalizeEditTags(tags)
		// 允许清空标签（站管审核后 AI 会补）
	}
	if updateContent {
		contentMD = strings.TrimSpace(contentMD)
		if contentMD == "" {
			return 0, fmt.Errorf("题面内容不能为空")
		}
		if len(contentMD) > 200_000 {
			return 0, fmt.Errorf("题面过长")
		}
	}
	if updateDifficulty {
		d, err := normalizeEditDifficulty(difficulty)
		if err != nil {
			return 0, err
		}
		difficulty = d
	}
	var p model.Problem
	if err := uc.data.DB.First(&p, problemID).Error; err != nil {
		return 0, fmt.Errorf("题目不存在")
	}

	var existing model.ProblemEditRequest
	err := uc.data.DB.Where("problem_id = ? AND user_id = ? AND status = ?", problemID, userID, model.ProblemEditPending).
		First(&existing).Error
	if err == nil {
		// 合并到已有 pending（分次改标签/题面/难度不互相覆盖）
		if updateTags {
			existing.HasTags = true
			existing.ProposedTags = model.StringArray(tags)
		}
		if updateContent {
			existing.HasContent = true
			existing.ProposedContentMD = contentMD
		}
		if t := strings.TrimSpace(title); t != "" {
			existing.ProposedTitle = t
		}
		if updateDifficulty {
			existing.HasDifficulty = true
			existing.ProposedDifficulty = difficulty
		}
		if n := strings.TrimSpace(note); n != "" {
			existing.Note = n
		}
		if err := uc.data.DB.Save(&existing).Error; err != nil {
			return 0, err
		}
		if autoApprove {
			if err := uc.ReviewProblemEdit(existing.ID, userID, true, "特权账号自动通过"); err != nil {
				return 0, err
			}
		}
		return existing.ID, nil
	}
	if err != gorm.ErrRecordNotFound {
		return 0, err
	}

	req := model.ProblemEditRequest{
		ProblemID:          problemID,
		UserID:             userID,
		HasTags:            updateTags,
		HasContent:         updateContent,
		HasDifficulty:      updateDifficulty,
		ProposedTags:       model.StringArray(tags),
		ProposedContentMD:  contentMD,
		ProposedTitle:      strings.TrimSpace(title),
		ProposedDifficulty: difficulty,
		Note:               strings.TrimSpace(note),
		Status:             model.ProblemEditPending,
	}
	if !updateTags {
		req.ProposedTags = model.StringArray{}
	}
	if !updateContent {
		req.ProposedContentMD = ""
	}
	if !updateDifficulty {
		req.ProposedDifficulty = ""
	}
	if err := uc.data.DB.Create(&req).Error; err != nil {
		return 0, err
	}
	if autoApprove {
		if err := uc.ReviewProblemEdit(req.ID, userID, true, "特权账号自动通过"); err != nil {
			return 0, err
		}
		return req.ID, nil
	}
	// 首次提交待审核：通知站管∪资源审核员（站内信 + 可配置邮件）
	uc.notifyReviewPendingProblemEdit(userID, problemID, req.ID, &p, &req)
	return req.ID, nil
}

// notifyReviewPendingProblemEdit 题面/标签修改进入待审
func (uc *ProblemUseCase) notifyReviewPendingProblemEdit(userID, problemID, editID uint, p *model.Problem, req *model.ProblemEditRequest) {
	if uc.data == nil || uc.data.UserDB == nil || userID == 0 {
		return
	}
	titleLabel := ""
	if p != nil {
		titleLabel = strings.TrimSpace(p.Title)
	}
	body := problemEditPendingSummary(titleLabel, req)
	payload := fmt.Sprintf(`{"editRequestId":%d,"problemId":%d,"problemTitle":%q}`, editID, problemID, titleLabel)
	applicant := lookupUserBrief(uc.data.UserDB, userID)
	emailSubj := "有内容待审核"
	if titleLabel != "" {
		emailSubj = "内容待审核 · " + titleLabel
	}
	html := problemEditPendingEmailHTML(titleLabel, problemID, editID, applicant, req)
	notify.NotifySiteAdminsWithEmail(uc.data.UserDB, notify.AdminNotif{
		Type:      notify.TypeReviewPending,
		Title:     "有内容待审核",
		Body:      body,
		ActorID:   userID,
		RefType:   "problem_edit",
		RefID:     editID,
		ProblemID: problemID,
		Payload:   payload,
	}, emailSubj, html)
}

// problemEditPendingSummary 给管理员的待审通知摘要；完整正文仍在审核详情中查看。
func problemEditPendingSummary(problemTitle string, req *model.ProblemEditRequest) string {
	prefix := "有用户提交了题目修改"
	if problemTitle = strings.TrimSpace(problemTitle); problemTitle != "" {
		prefix = fmt.Sprintf("有用户提交了题目「%s」的修改", problemTitle)
	}
	if req == nil {
		return prefix + "，等待审核"
	}
	details := make([]string, 0, 4)
	if title := strings.TrimSpace(req.ProposedTitle); title != "" {
		details = append(details, fmt.Sprintf("标题改为「%s」", truncateNotificationText(title, 80)))
	}
	if req.HasContent {
		details = append(details, fmt.Sprintf("题面内容（%d 字）", len([]rune(strings.TrimSpace(req.ProposedContentMD)))))
	}
	if req.HasTags {
		tags := nonEmptyTags(req.ProposedTags)
		if len(tags) == 0 {
			details = append(details, "清空题目标签")
		} else {
			details = append(details, "标签改为「"+strings.Join(tags, "、")+"」")
		}
	}
	if req.HasDifficulty {
		d := strings.TrimSpace(req.ProposedDifficulty)
		if d == "" {
			details = append(details, "清空难度")
		} else {
			details = append(details, "难度改为「"+d+"」")
		}
	}
	if note := strings.TrimSpace(req.Note); note != "" {
		details = append(details, "修改说明："+truncateNotificationText(note, 120))
	}
	if len(details) == 0 {
		return prefix + "，等待审核"
	}
	return prefix + "。修改内容：" + strings.Join(details, "；") + "。请审核"
}

func truncateNotificationText(s string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(s))
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "…"
}

// ListProblemEditRequests 审核列表
func (uc *ProblemUseCase) ListProblemEditRequests(page, pageSize int64, status string) ([]model.ProblemEditRequest, int64, map[uint]*model.Problem, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	q := uc.data.DB.Model(&model.ProblemEditRequest{})
	status = strings.TrimSpace(status)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, nil, err
	}
	var list []model.ProblemEditRequest
	if err := q.Order("id desc").Offset(int((page - 1) * pageSize)).Limit(int(pageSize)).Find(&list).Error; err != nil {
		return nil, 0, nil, err
	}
	pids := make([]uint, 0, len(list))
	seen := map[uint]struct{}{}
	for _, r := range list {
		if _, ok := seen[r.ProblemID]; ok {
			continue
		}
		seen[r.ProblemID] = struct{}{}
		pids = append(pids, r.ProblemID)
	}
	probMap := map[uint]*model.Problem{}
	if len(pids) > 0 {
		var probs []model.Problem
		if err := uc.data.DB.Where("id IN ?", pids).Find(&probs).Error; err == nil {
			for i := range probs {
				probMap[probs[i].ID] = &probs[i]
			}
		}
	}
	return list, total, probMap, nil
}

// ReviewProblemEdit 站管通过/驳回
func (uc *ProblemUseCase) ReviewProblemEdit(requestID, reviewerID uint, approve bool, reviewNote string) error {
	var req model.ProblemEditRequest
	if err := uc.data.DB.First(&req, requestID).Error; err != nil {
		return fmt.Errorf("申请不存在")
	}
	if req.Status != model.ProblemEditPending {
		return fmt.Errorf("该申请已处理")
	}
	if !approve {
		rid := reviewerID
		if err := uc.data.DB.Model(&req).Updates(map[string]interface{}{
			"status":      model.ProblemEditRejected,
			"reviewer_id": rid,
			"review_note": strings.TrimSpace(reviewNote),
		}).Error; err != nil {
			return err
		}
		uc.notifyProblemEditResult(&req, false, strings.TrimSpace(reviewNote), reviewerID)
		return nil
	}
	// 通过：应用修改
	_, err := uc.ApplyProblemFields(
		req.ProblemID,
		req.HasTags, []string(req.ProposedTags),
		req.HasContent, req.ProposedContentMD,
		req.ProposedTitle,
		req.HasDifficulty, req.ProposedDifficulty,
	)
	if err != nil {
		return err
	}
	rid := reviewerID
	if err := uc.data.DB.Model(&req).Updates(map[string]interface{}{
		"status":      model.ProblemEditApproved,
		"reviewer_id": rid,
		"review_note": strings.TrimSpace(reviewNote),
	}).Error; err != nil {
		return err
	}
	uc.notifyProblemEditResult(&req, true, strings.TrimSpace(reviewNote), reviewerID)
	return nil
}

// notifyProblemEditResult 审核结果站内信（写 user.notifications）。
// 通过：额外给申请人邮箱发感谢信；驳回：仅站内信，不发邮件。
// 申请人与审核人为同一人（特权账号自动通过）时跳过站内信与邮件。
func (uc *ProblemUseCase) notifyProblemEditResult(req *model.ProblemEditRequest, approved bool, note string, reviewerID uint) {
	if req == nil || req.UserID == 0 {
		return
	}
	// 自审通过：流水已写入，无需再通知自己
	if approved && reviewerID != 0 && reviewerID == req.UserID {
		return
	}
	typ := notify.TypeProblemEditRejected
	title := "题面修改申请未通过"
	body := "你的题面/标签修改申请已被驳回"
	if approved {
		typ = notify.TypeProblemEditApproved
		title = "你的内容贡献已通过审核"
		body = problemEditApprovalThankYou(uc.data.DB, req)
	}
	if note != "" {
		body = body + "。备注：" + note
	}
	if err := notify.Create(uc.data.UserDB, notify.Row{
		UserID:    req.UserID,
		Type:      typ,
		Title:     title,
		Body:      body,
		ActorID:   reviewerID,
		RefType:   "problem_edit",
		RefID:     req.ID,
		ProblemID: req.ProblemID,
	}); err != nil {
		log.Warnf("notifyProblemEditResult: %v", err)
	}
	// 仅审核通过发邮件感谢信；驳回不打扰邮箱
	if !approved || uc.data == nil || uc.data.UserDB == nil {
		return
	}
	mailHTML := problemEditThankYouEmailHTML(uc.data.DB, req, note)
	mailSubj := "感谢你的内容贡献 · 已生效"
	// 异步入队：SMTP 发送由 mail consumer 消费，不再阻塞审核接口
	if to := notify.LookupUserEmail(uc.data.UserDB, req.UserID); to != "" {
		if !mailqueue.Enqueue(uc.mq, to, mailSubj, mailHTML) {
			log.Warnf("notifyProblemEditResult: approval email enqueue failed user=%d", req.UserID)
		}
	}
}

// htmlEscapePlain 将纯文本放入 HTML 段落（换行保留为 <br>）。
func htmlEscapePlain(s string) string {
	return mail.Paragraphs(s)
}

type userBrief struct {
	ID       uint
	Username string
	Name     string
}

func lookupUserBrief(db *gorm.DB, userID uint) userBrief {
	out := userBrief{ID: userID}
	if db == nil || userID == 0 {
		return out
	}
	var row struct {
		ID       uint   `gorm:"column:id"`
		Username string `gorm:"column:username"`
		Name     string `gorm:"column:name"`
	}
	if err := db.Table("users").Select("id, username, name").Where("id = ?", userID).Scan(&row).Error; err == nil {
		out.ID = row.ID
		out.Username = strings.TrimSpace(row.Username)
		out.Name = strings.TrimSpace(row.Name)
	}
	return out
}

func (u userBrief) display() string {
	if u.Name != "" && u.Username != "" {
		return fmt.Sprintf("%s（@%s）", u.Name, u.Username)
	}
	if u.Name != "" {
		return u.Name
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.ID > 0 {
		return fmt.Sprintf("用户 #%d", u.ID)
	}
	return "未知用户"
}

func problemEditApprovedItems(req *model.ProblemEditRequest) []string {
	items := make([]string, 0, 4)
	if req == nil {
		return []string{"题目修改"}
	}
	if strings.TrimSpace(req.ProposedTitle) != "" {
		items = append(items, "题目标题")
	}
	if req.HasContent {
		items = append(items, "题面内容")
	}
	if req.HasTags {
		items = append(items, "题目标签")
	}
	if req.HasDifficulty {
		items = append(items, "题目难度")
	}
	if len(items) == 0 {
		items = append(items, "题目修改")
	}
	return items
}

func problemTitleFromDB(db *gorm.DB, problemID uint) string {
	if db == nil || problemID == 0 {
		return ""
	}
	var p model.Problem
	if err := db.Select("title").First(&p, problemID).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(p.Title)
}

// problemEditPendingEmailHTML 管理员待审邮件（品牌壳 + 结构化字段）
func problemEditPendingEmailHTML(problemTitle string, problemID, editID uint, applicant userBrief, req *model.ProblemEditRequest) string {
	titleShow := strings.TrimSpace(problemTitle)
	if titleShow == "" {
		titleShow = fmt.Sprintf("题目 #%d", problemID)
	}
	rows := []struct{ k, v string }{
		{"申请人", applicant.display()},
		{"题目", titleShow},
		{"题目 ID", fmt.Sprintf("%d", problemID)},
		{"申请 ID", fmt.Sprintf("%d", editID)},
	}
	if req != nil {
		if t := strings.TrimSpace(req.ProposedTitle); t != "" {
			rows = append(rows, struct{ k, v string }{"新标题", truncateNotificationText(t, 120)})
		}
		if req.HasContent {
			n := len([]rune(strings.TrimSpace(req.ProposedContentMD)))
			rows = append(rows, struct{ k, v string }{"题面", fmt.Sprintf("已修改（约 %d 字）", n)})
			preview := truncateNotificationText(strings.TrimSpace(req.ProposedContentMD), 200)
			if preview != "" {
				rows = append(rows, struct{ k, v string }{"题面摘要", preview})
			}
		}
		if req.HasTags {
			tags := nonEmptyTags(req.ProposedTags)
			if len(tags) == 0 {
				rows = append(rows, struct{ k, v string }{"标签", "清空全部标签"})
			} else {
				rows = append(rows, struct{ k, v string }{"标签", strings.Join(tags, "、")})
			}
		}
		if note := strings.TrimSpace(req.Note); note != "" {
			rows = append(rows, struct{ k, v string }{"修改说明", truncateNotificationText(note, 200)})
		}
	}
	var b strings.Builder
	b.WriteString(`<p style="margin:0 0 14px;">有用户提交了题目修改，请尽快审核。</p>`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;font-size:14px;">`)
	for _, r := range rows {
		fmt.Fprintf(&b, `<tr><td style="padding:6px 12px 6px 0;color:#737373;vertical-align:top;width:88px;">%s</td><td style="padding:6px 0;color:#0a0a0a;">%s</td></tr>`,
			mail.Escape(r.k), mail.Escape(r.v))
	}
	b.WriteString(`</table>`)
	b.WriteString(`<p style="margin:16px 0 0;font-size:13px;color:#0a0a0a;">请登录站点 → 打开管理端「内容审核 / 题库」处理该申请。通过后修改将立即生效；驳回不会给用户发邮件。</p>`)
	return mail.Wrap(mail.LayoutOpts{
		Brand:     mail.DefaultBrand,
		Title:     "内容待审核",
		Preheader: problemEditPendingSummary(problemTitle, req),
	}, b.String())
}

// problemEditThankYouEmailHTML 贡献者审核通过感谢信
func problemEditThankYouEmailHTML(db *gorm.DB, req *model.ProblemEditRequest, reviewNote string) string {
	items := problemEditApprovedItems(req)
	problemTitle := ""
	problemID := uint(0)
	if req != nil {
		problemID = req.ProblemID
		problemTitle = problemTitleFromDB(db, req.ProblemID)
	}
	var b strings.Builder
	b.WriteString(`<p style="margin:0 0 12px;">你好，</p>`)
	if problemTitle != "" {
		fmt.Fprintf(&b, `<p style="margin:0 0 12px;">你为题目「<strong>%s</strong>」提交的内容贡献<strong>已通过审核并生效</strong>。</p>`, mail.Escape(problemTitle))
	} else {
		b.WriteString(`<p style="margin:0 0 12px;">你的内容贡献<strong>已通过审核并生效</strong>。</p>`)
	}
	b.WriteString(`<p style="margin:0 0 8px;color:#737373;font-size:13px;">本次生效内容：</p><ul style="margin:0 0 14px;padding-left:20px;color:#0a0a0a;">`)
	for _, it := range items {
		fmt.Fprintf(&b, `<li style="margin:4px 0;">%s</li>`, mail.Escape(it))
	}
	b.WriteString(`</ul>`)
	if req != nil {
		if t := strings.TrimSpace(req.ProposedTitle); t != "" {
			fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;"><span style="color:#737373;">新标题：</span>%s</p>`, mail.Escape(t))
		}
		if req.HasTags {
			tags := nonEmptyTags(req.ProposedTags)
			if len(tags) > 0 {
				fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;"><span style="color:#737373;">标签：</span>%s</p>`, mail.Escape(strings.Join(tags, "、")))
			}
		}
	}
	if reviewNote != "" {
		fmt.Fprintf(&b, `<p style="margin:12px 0 8px;font-size:13px;"><span style="color:#737373;">审核备注：</span>%s</p>`, mail.Escape(reviewNote))
	}
	b.WriteString(`<p style="margin:16px 0 0;">感谢你为 GoAlgo 作出贡献！站内通知中也有同一条消息。</p>`)
	if problemID > 0 {
		b.WriteString(`<p style="margin:14px 0 0;">`)
		b.WriteString(mail.BtnPrimary(fmt.Sprintf("%s/question-bank/detail/%d", mail.SiteHomeURL, problemID), "查看题目"))
		b.WriteString(`</p>`)
	}
	return mail.Wrap(mail.LayoutOpts{
		Brand:     mail.DefaultBrand,
		Title:     "感谢你的内容贡献",
		Preheader: "你的修改已通过审核并生效",
	}, b.String())
}

// problemEditApprovalThankYou 生成面向贡献者的审核通过站内信正文，并明确本次生效内容。
func problemEditApprovalThankYou(db *gorm.DB, req *model.ProblemEditRequest) string {
	if req == nil {
		return "你的内容贡献已通过审核并生效。感谢你为 GoAlgo 作出贡献！"
	}
	items := problemEditApprovedItems(req)
	problemTitle := problemTitleFromDB(db, req.ProblemID)
	prefix := "你的内容贡献已通过审核并生效"
	if problemTitle != "" {
		prefix = fmt.Sprintf("你为题目「%s」提交的内容贡献已通过审核并生效", problemTitle)
	}
	return fmt.Sprintf("%s。本次通过：%s。感谢你为 GoAlgo 作出贡献！", prefix, strings.Join(items, "、"))
}

// ListProblemContributors 审核通过的贡献者 user_id（按首次通过时间升序，去重）。
// 仅统计 problem_edit_requests.status=approved，不含站管直改。
func (uc *ProblemUseCase) ListProblemContributors(problemID uint) ([]uint, error) {
	if problemID == 0 || uc == nil || uc.data == nil || uc.data.DB == nil {
		return nil, nil
	}
	var out []uint
	// 用 MIN(updated_at) 作为「首次通过」近似（通过时会写 status+updated_at）
	// 只 SELECT user_id，避免 SQLite 聚合时间类型扫描问题
	err := uc.data.DB.Model(&model.ProblemEditRequest{}).
		Select("user_id").
		Where("problem_id = ? AND status = ?", problemID, model.ProblemEditApproved).
		Group("user_id").
		Order("MIN(updated_at) ASC").
		Pluck("user_id", &out).Error
	if err != nil {
		return nil, err
	}
	// 过滤 0
	clean := make([]uint, 0, len(out))
	for _, id := range out {
		if id > 0 {
			clean = append(clean, id)
		}
	}
	return clean, nil
}

// MyPendingProblemEdit 当前用户对该题的待审申请
func (uc *ProblemUseCase) MyPendingProblemEdit(userID, problemID uint) (*model.ProblemEditRequest, error) {
	if userID == 0 || problemID == 0 {
		return nil, nil
	}
	var req model.ProblemEditRequest
	err := uc.data.DB.Where("problem_id = ? AND user_id = ? AND status = ?", problemID, userID, model.ProblemEditPending).
		First(&req).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}
