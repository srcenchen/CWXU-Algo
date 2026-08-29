package dal

import (
	"context"
	"cwxu-algo/app/core_data/internal/data/model"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProfileCacheIdentity is the complete durable identity for one user's
// published ability profile cache.
type ProfileCacheIdentity struct {
	ModelVersion uint64
	Evidence     ProfileEvidenceIdentity
}

// ReadProfileCacheIdentity reads the active ability model, global evidence
// dataset state, and the user's evidence revision in one consistent statement.
// A missing user revision is zero; incomplete global state fails closed.
func ReadProfileCacheIdentity(ctx context.Context, db *gorm.DB, userID int64) (ProfileCacheIdentity, error) {
	if db == nil || userID <= 0 {
		return ProfileCacheIdentity{}, errors.New("invalid profile cache identity request")
	}
	type identityRow struct {
		ModelVersion    uint64 `gorm:"column:model_version"`
		DatasetRevision uint64 `gorm:"column:dataset_revision"`
		SchemaVersion   uint   `gorm:"column:schema_version"`
		Ready           bool   `gorm:"column:ready"`
		UserRevision    uint64 `gorm:"column:user_revision"`
	}
	var row identityRow
	result := db.WithContext(ctx).Raw(`SELECT a.active_version AS model_version,
		d.revision AS dataset_revision, d.schema_version, d.ready,
		COALESCE(u.revision, 0) AS user_revision
		FROM ability_model_state a
		JOIN profile_evidence_dataset_state d ON d.id = ?
		LEFT JOIN user_profile_evidence_versions u ON u.user_id = ?
		WHERE a.id = ?`, 1, userID, 1).Scan(&row)
	if result.Error != nil {
		return ProfileCacheIdentity{}, result.Error
	}
	if result.RowsAffected != 1 {
		return ProfileCacheIdentity{}, errors.New("profile cache identity state is missing")
	}
	if row.ModelVersion == 0 {
		return ProfileCacheIdentity{}, errors.New("profile cache ability model state is not ready")
	}
	if !row.Ready || row.SchemaVersion != CurrentProfileEvidenceSchemaVersion {
		return ProfileCacheIdentity{}, errors.New("profile cache evidence dataset state is not ready")
	}
	return ProfileCacheIdentity{
		ModelVersion: row.ModelVersion,
		Evidence: ProfileEvidenceIdentity{
			DatasetRevision: row.DatasetRevision,
			UserRevision:    row.UserRevision,
		},
	}, nil
}

// EnsureProfileCacheIdentityForBuild initializes the lockable per-user
// revision tombstone before a background build captures its identity. The HTTP
// hot path intentionally keeps using ReadProfileCacheIdentity and remains
// read-only.
func EnsureProfileCacheIdentityForBuild(ctx context.Context, db *gorm.DB, userID int64) (ProfileCacheIdentity, error) {
	if db == nil || userID <= 0 {
		return ProfileCacheIdentity{}, errors.New("invalid profile cache build identity request")
	}
	var identity ProfileCacheIdentity
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.UserProfileEvidenceVersion{UserID: userID}).Error; err != nil {
			return fmt.Errorf("initialize profile cache build identity: %w", err)
		}
		var err error
		identity, err = ReadProfileCacheIdentity(ctx, tx, userID)
		return err
	})
	return identity, err
}

// lockProfileCacheIdentity takes the publication fences in a fixed order.
// PostgreSQL source-data triggers need an UPDATE lock on the evidence rows, so
// these shared locks make the final compare-and-publish safe across replicas.
func lockProfileCacheIdentity(ctx context.Context, tx *gorm.DB, userID int64) (ProfileCacheIdentity, error) {
	if tx == nil || userID <= 0 {
		return ProfileCacheIdentity{}, errors.New("invalid locked profile cache identity request")
	}
	var ability model.AbilityModelState
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "SHARE"}).First(&ability, 1).Error; err != nil {
		return ProfileCacheIdentity{}, err
	}
	if ability.ActiveVersion == 0 {
		return ProfileCacheIdentity{}, errors.New("profile cache ability model state is not ready")
	}
	var dataset model.ProfileEvidenceDatasetState
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "SHARE"}).First(&dataset, 1).Error; err != nil {
		return ProfileCacheIdentity{}, err
	}
	if !dataset.Ready || dataset.SchemaVersion != CurrentProfileEvidenceSchemaVersion {
		return ProfileCacheIdentity{}, errors.New("profile cache evidence dataset state is not ready")
	}
	var user model.UserProfileEvidenceVersion
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "SHARE"}).First(&user, "user_id = ?", userID).Error; err != nil {
		return ProfileCacheIdentity{}, err
	}
	return ProfileCacheIdentity{
		ModelVersion: ability.ActiveVersion,
		Evidence: ProfileEvidenceIdentity{
			DatasetRevision: dataset.Revision,
			UserRevision:    user.Revision,
		},
	}, nil
}
