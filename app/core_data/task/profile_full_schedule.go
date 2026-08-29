package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
)

const profileFullScheduleAdvisoryKey int64 = 0x476f416c676f

type profileFullScheduleGate interface {
	Run(context.Context, string, uint64, func(*gorm.DB) error) (bool, error)
}

type dbProfileFullScheduleGate struct{ db *gorm.DB }

func newDBProfileFullScheduleGate(db *gorm.DB) profileFullScheduleGate {
	return &dbProfileFullScheduleGate{db: db}
}

// PostgreSQL's transaction-scoped advisory lock is connection-owned and is
// automatically released on commit, rollback, cancellation, or process death.
// It serializes the whole enumerate/policy/publish batch without holding the
// ability_model_state row. SQLite tests use the process mutex fallback.
func (g *dbProfileFullScheduleGate) Run(ctx context.Context, period string, modelVersion uint64, work func(*gorm.DB) error) (ran bool, err error) {
	if g == nil || g.db == nil {
		return false, errors.New("profile full schedule: nil database")
	}
	period = strings.TrimSpace(period)
	if period == "" {
		return false, errors.New("profile full schedule: empty period")
	}
	if work == nil {
		return false, errors.New("profile full schedule: nil work")
	}
	if g.db.Dialector.Name() != "postgres" {
		profileFullScheduleProcessMu.Lock()
		defer profileFullScheduleProcessMu.Unlock()
		return runProfileFullSchedule(ctx, g.db, period, modelVersion, work)
	}
	err = g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", profileFullScheduleAdvisoryKey).Error; err != nil {
			return err
		}
		var innerErr error
		ran, innerErr = runProfileFullSchedule(ctx, tx, period, modelVersion, work)
		return innerErr
	})
	return ran, err
}

var profileFullScheduleProcessMu sync.Mutex

func runProfileFullSchedule(ctx context.Context, db *gorm.DB, period string, modelVersion uint64, work func(*gorm.DB) error) (bool, error) {
	var completed model.AbilityProfileScheduleRun
	err := db.WithContext(ctx).First(&completed, "period = ?", period).Error
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	if err := work(db); err != nil {
		return false, err
	}
	marker := model.AbilityProfileScheduleRun{
		Period: period, ModelVersion: modelVersion, CompletedAt: time.Now(),
	}
	if err := db.WithContext(ctx).Create(&marker).Error; err != nil {
		return false, err
	}
	return true, nil
}
