package dal

import (
	"context"
	"cwxu-algo/app/core_data/internal/data/model"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProfileEvidenceIdentity is a stable cache identity made from the global
// dataset epoch and the affected user's durable evidence revision.
type ProfileEvidenceIdentity struct {
	DatasetRevision uint64
	UserRevision    uint64
}

func (v ProfileEvidenceIdentity) String() string {
	return fmt.Sprintf("d%d:u%d", v.DatasetRevision, v.UserRevision)
}

func (v ProfileEvidenceIdentity) CacheKey() string { return v.String() }

const CurrentProfileEvidenceSchemaVersion = model.CurrentProfileEvidenceSchemaVersion

// ReadProfileEvidenceIdentity performs only keyed revision-state reads. A
// missing user row is atomically initialized at zero without overwriting a
// concurrent trigger bump. Missing dataset state is intentionally an error.
func ReadProfileEvidenceIdentity(ctx context.Context, db *gorm.DB, userID int64) (ProfileEvidenceIdentity, error) {
	if db == nil || userID <= 0 {
		return ProfileEvidenceIdentity{}, errors.New("invalid profile evidence identity request")
	}
	type identityRow struct {
		DatasetRevision uint64 `gorm:"column:dataset_revision"`
		SchemaVersion   uint   `gorm:"column:schema_version"`
		Ready           bool   `gorm:"column:ready"`
		UserRevision    uint64 `gorm:"column:user_revision"`
		UserPresent     bool   `gorm:"column:user_present"`
	}
	read := func(tx *gorm.DB) (identityRow, error) {
		var row identityRow
		result := tx.Raw(`SELECT d.revision AS dataset_revision, d.schema_version, d.ready,
			COALESCE(u.revision, 0) AS user_revision,
			CASE WHEN u.user_id IS NULL THEN FALSE ELSE TRUE END AS user_present
			FROM profile_evidence_dataset_state d
			LEFT JOIN user_profile_evidence_versions u ON u.user_id = ?
			WHERE d.id = ?`, userID, 1).Scan(&row)
		if result.Error != nil {
			return identityRow{}, result.Error
		}
		if result.RowsAffected == 0 {
			return identityRow{}, errors.New("profile evidence dataset state is missing")
		}
		if !row.Ready || row.SchemaVersion != CurrentProfileEvidenceSchemaVersion {
			return identityRow{}, errors.New("profile evidence dataset state is not ready")
		}
		return row, nil
	}
	var identity ProfileEvidenceIdentity
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := read(tx)
		if err != nil {
			return err
		}
		if !row.UserPresent {
			// Conflict-do-nothing is essential: a trigger that wins the race
			// inserts revision=1, and this initialization must not reset it to 0.
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.UserProfileEvidenceVersion{UserID: userID}).Error; err != nil {
				return fmt.Errorf("initialize profile evidence revision: %w", err)
			}
			row, err = read(tx)
			if err != nil {
				return err
			}
		}
		identity = ProfileEvidenceIdentity{DatasetRevision: row.DatasetRevision, UserRevision: row.UserRevision}
		return nil
	})
	return identity, err
}

// InstallProfileEvidenceRevisionTriggers installs idempotent, transactionally
// coupled revision triggers after AutoMigrate. It must succeed before startup
// continues: a missing trigger would allow a permanently stale profile cache.
func InstallProfileEvidenceRevisionTriggers(db *gorm.DB) error {
	return model.InstallProfileEvidenceRevisionTriggers(db)
}

// BumpProfileEvidenceDataset invalidates all profile evidence after a caller's
// global source-data operation (such as a purge) in that same transaction.
func BumpProfileEvidenceDataset(ctx context.Context, tx *gorm.DB) error {
	if tx == nil {
		return errors.New("profile evidence dataset bump: nil database")
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return errors.New("profile evidence dataset bump: transaction required")
	}
	result := tx.WithContext(ctx).Exec(`UPDATE profile_evidence_dataset_state
		SET revision = revision + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND ready = TRUE AND schema_version = ?`, 1, CurrentProfileEvidenceSchemaVersion)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("profile evidence dataset bump: dataset state is not ready")
	}
	return nil
}
