package dal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
)

type AbilityMaintenanceOperationCount struct {
	Operation string
	Count     int64
}

func LoadAbilityMaintenanceRecoveryBatch(ctx context.Context, db *gorm.DB, operations []string, limit int) ([]model.AbilityMaintenancePending, error) {
	if db == nil || len(operations) == 0 || limit <= 0 {
		return nil, fmt.Errorf("invalid ability maintenance recovery batch")
	}
	var pending []model.AbilityMaintenancePending
	err := db.WithContext(ctx).
		Where("operation IN ?", operations).
		Order("updated_at ASC, created_at ASC, scope ASC").
		Limit(limit).
		Find(&pending).Error
	return pending, err
}

func TouchAbilityMaintenanceRecoveryAttempt(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, attemptedAt time.Time) (bool, error) {
	if db == nil || pending == nil || strings.TrimSpace(pending.Scope) == "" || strings.TrimSpace(pending.OperationID) == "" || attemptedAt.IsZero() {
		return false, fmt.Errorf("invalid ability maintenance recovery attempt")
	}
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ?", pending.Scope, pending.OperationID).
		UpdateColumn("updated_at", attemptedAt)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 1 {
		pending.UpdatedAt = attemptedAt
		return true, nil
	}
	return false, nil
}

func ListUnknownAbilityMaintenanceOperations(ctx context.Context, db *gorm.DB, knownOperations []string, limit int) ([]AbilityMaintenanceOperationCount, error) {
	if db == nil || len(knownOperations) == 0 || limit <= 0 {
		return nil, fmt.Errorf("invalid ability maintenance unknown-operation query")
	}
	var rows []AbilityMaintenanceOperationCount
	err := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Select("operation, COUNT(*) AS count").
		Where("operation NOT IN ?", knownOperations).
		Group("operation").
		Order("count DESC, operation ASC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}
