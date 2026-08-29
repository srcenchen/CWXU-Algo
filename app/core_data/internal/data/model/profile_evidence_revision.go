package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserProfileEvidenceVersion is the durable, per-user invalidation revision
// for profile evidence. Database triggers own its monotonic writes.
type UserProfileEvidenceVersion struct {
	UserID    int64     `gorm:"primaryKey;comment:用户ID"`
	Revision  uint64    `gorm:"not null;default:0;comment:画像证据版本"`
	UpdatedAt time.Time `gorm:"not null;comment:版本更新时间"`
}

func (UserProfileEvidenceVersion) TableName() string { return "user_profile_evidence_versions" }

// ProfileEvidenceDatasetState invalidates all profile evidence when a source
// table is truncated, where individual user rows cannot be enumerated.
type ProfileEvidenceDatasetState struct {
	ID            uint64    `gorm:"primaryKey;comment:固定单例ID"`
	Revision      uint64    `gorm:"not null;default:0;comment:全局证据版本"`
	SchemaVersion uint      `gorm:"not null;default:0;comment:证据版本模式"`
	Ready         bool      `gorm:"not null;default:false;comment:触发器及回填就绪"`
	UpdatedAt     time.Time `gorm:"not null;comment:版本更新时间"`
}

func (ProfileEvidenceDatasetState) TableName() string { return "profile_evidence_dataset_state" }

const CurrentProfileEvidenceSchemaVersion uint = 2

var profileEvidenceSourceTables = []string{"submit_logs", "user_ac_problems", "platforms"}
var profileEvidenceDatasetSourceTables = []string{"problem_tags"}

// InstallProfileEvidenceRevisionTriggers is owned by the migration layer so
// data can install it without importing dal (which intentionally depends on
// data elsewhere). It is fail-closed and safe for concurrent startup.
func InstallProfileEvidenceRevisionTriggers(db *gorm.DB) error {
	if db == nil || db.Dialector == nil {
		return errors.New("profile evidence revision: nil database")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(820260829)").Error; err != nil {
				return fmt.Errorf("lock profile evidence revision migration: %w", err)
			}
		}
		// These tables are deliberately migrated inside the same critical
		// section as trigger installation and backfill. Publishing their schema
		// before the triggers would leave a write window with no revision bump.
		if err := tx.AutoMigrate(&UserProfileEvidenceVersion{}, &ProfileEvidenceDatasetState{}); err != nil {
			return fmt.Errorf("migrate profile evidence revision tables: %w", err)
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ProfileEvidenceDatasetState{ID: 1}).Error; err != nil {
			return fmt.Errorf("initialize profile evidence dataset state: %w", err)
		}
		var statements []string
		switch tx.Dialector.Name() {
		case "postgres":
			statements = profileEvidencePostgresTriggerDDLStatements()
		case "sqlite":
			statements = profileEvidenceSQLiteTriggerDDLStatements()
		default:
			return fmt.Errorf("profile evidence revision: unsupported dialect %q", tx.Dialector.Name())
		}
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("install profile evidence revision trigger: %w", err)
			}
		}
		if err := tx.Exec(profileEvidenceBackfillSQL()).Error; err != nil {
			return fmt.Errorf("backfill profile evidence revisions: %w", err)
		}
		if err := tx.Model(&ProfileEvidenceDatasetState{}).Where("id = ?", 1).Updates(map[string]interface{}{
			"revision":       gorm.Expr("CASE WHEN ready = TRUE AND schema_version <> ? THEN revision + 1 ELSE revision END", CurrentProfileEvidenceSchemaVersion),
			"schema_version": CurrentProfileEvidenceSchemaVersion,
			"ready":          true,
		}).Error; err != nil {
			return fmt.Errorf("mark profile evidence dataset state ready: %w", err)
		}
		return nil
	})
}

func ProfileEvidencePostgresDatasetTriggerDDL() string {
	return strings.Join(profileEvidencePostgresTriggerDDLStatements(), "\n")
}

func profileEvidenceBackfillSQL() string {
	return `INSERT INTO user_profile_evidence_versions (user_id, revision, updated_at)
		SELECT user_id, 0, CURRENT_TIMESTAMP FROM (
			SELECT user_id FROM submit_logs WHERE user_id > 0
			UNION SELECT user_id FROM user_ac_problems WHERE user_id > 0
			UNION SELECT user_id FROM platforms WHERE user_id > 0
		) profile_evidence_users WHERE TRUE ON CONFLICT(user_id) DO NOTHING`
}

func profileEvidenceBumpUserSQL(userRef string) string {
	return `INSERT INTO user_profile_evidence_versions (user_id, revision, updated_at)
		SELECT ` + userRef + `.user_id, 1, CURRENT_TIMESTAMP WHERE ` + userRef + `.user_id > 0
		ON CONFLICT(user_id) DO UPDATE SET revision = user_profile_evidence_versions.revision + 1, updated_at = CURRENT_TIMESTAMP`
}

func profileEvidenceBumpDatasetSQL() string {
	return `INSERT INTO profile_evidence_dataset_state (id, revision, updated_at)
		VALUES (1, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET revision = profile_evidence_dataset_state.revision + 1, updated_at = CURRENT_TIMESTAMP`
}

func profileEvidenceSQLiteTriggerDDLStatements() []string {
	statements := make([]string, 0, len(profileEvidenceSourceTables)*6+len(profileEvidenceDatasetSourceTables)*6)
	for _, table := range profileEvidenceSourceTables {
		prefix := "profile_evidence_revision_" + table
		for _, suffix := range []string{"insert", "update", "delete"} {
			statements = append(statements, "DROP TRIGGER IF EXISTS "+prefix+"_"+suffix)
		}
		statements = append(statements,
			`CREATE TRIGGER `+prefix+`_insert AFTER INSERT ON `+table+` BEGIN `+profileEvidenceBumpUserSQL("NEW")+`; END`,
			`CREATE TRIGGER `+prefix+`_update AFTER UPDATE ON `+table+` BEGIN `+profileEvidenceBumpUserSQL("OLD")+`; `+profileEvidenceBumpUserSQL("NEW")+`; END`,
			`CREATE TRIGGER `+prefix+`_delete AFTER DELETE ON `+table+` BEGIN `+profileEvidenceBumpUserSQL("OLD")+`; END`,
		)
	}
	for _, table := range profileEvidenceDatasetSourceTables {
		prefix := "profile_evidence_dataset_" + table
		for _, suffix := range []string{"insert", "update", "delete"} {
			statements = append(statements, "DROP TRIGGER IF EXISTS "+prefix+"_"+suffix)
		}
		statements = append(statements,
			`CREATE TRIGGER `+prefix+`_insert AFTER INSERT ON `+table+` BEGIN `+profileEvidenceBumpDatasetSQL()+`; END`,
			`CREATE TRIGGER `+prefix+`_update AFTER UPDATE ON `+table+` BEGIN `+profileEvidenceBumpDatasetSQL()+`; END`,
			`CREATE TRIGGER `+prefix+`_delete AFTER DELETE ON `+table+` BEGIN `+profileEvidenceBumpDatasetSQL()+`; END`,
		)
	}
	return statements
}

func profileEvidencePostgresTriggerDDLStatements() []string {
	statements := []string{
		`CREATE OR REPLACE FUNCTION profile_evidence_bump_user_revision_insert_trigger() RETURNS trigger AS $$
		BEGIN
			INSERT INTO user_profile_evidence_versions (user_id, revision, updated_at)
			SELECT user_id, 1, CURRENT_TIMESTAMP FROM (SELECT DISTINCT user_id FROM new_rows WHERE user_id > 0) users
			ON CONFLICT (user_id) DO UPDATE SET revision = user_profile_evidence_versions.revision + 1, updated_at = CURRENT_TIMESTAMP;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION profile_evidence_bump_user_revision_delete_trigger() RETURNS trigger AS $$
		BEGIN
			INSERT INTO user_profile_evidence_versions (user_id, revision, updated_at)
			SELECT user_id, 1, CURRENT_TIMESTAMP FROM (SELECT DISTINCT user_id FROM old_rows WHERE user_id > 0) users
			ON CONFLICT (user_id) DO UPDATE SET revision = user_profile_evidence_versions.revision + 1, updated_at = CURRENT_TIMESTAMP;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION profile_evidence_bump_user_revision_update_trigger() RETURNS trigger AS $$
		BEGIN
			INSERT INTO user_profile_evidence_versions (user_id, revision, updated_at)
			SELECT user_id, 1, CURRENT_TIMESTAMP FROM (
				SELECT DISTINCT user_id FROM old_rows WHERE user_id > 0
				UNION SELECT DISTINCT user_id FROM new_rows WHERE user_id > 0
			) users ON CONFLICT (user_id) DO UPDATE SET revision = user_profile_evidence_versions.revision + 1, updated_at = CURRENT_TIMESTAMP;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql`,
		`CREATE OR REPLACE FUNCTION profile_evidence_bump_dataset_revision_trigger() RETURNS trigger AS $$
		BEGIN
			INSERT INTO profile_evidence_dataset_state (id, revision, updated_at) VALUES (1, 1, CURRENT_TIMESTAMP)
			ON CONFLICT (id) DO UPDATE SET revision = profile_evidence_dataset_state.revision + 1, updated_at = CURRENT_TIMESTAMP;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql`,
	}
	for _, table := range profileEvidenceSourceTables {
		name := "profile_evidence_revision_" + table
		statements = append(statements,
			"DROP TRIGGER IF EXISTS "+name+"_insert ON "+table,
			"DROP TRIGGER IF EXISTS "+name+"_update ON "+table,
			"DROP TRIGGER IF EXISTS "+name+"_delete ON "+table,
			"CREATE TRIGGER "+name+"_insert AFTER INSERT ON "+table+" REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_user_revision_insert_trigger()",
			"CREATE TRIGGER "+name+"_update AFTER UPDATE ON "+table+" REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_user_revision_update_trigger()",
			"CREATE TRIGGER "+name+"_delete AFTER DELETE ON "+table+" REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_user_revision_delete_trigger()",
			"DROP TRIGGER IF EXISTS profile_evidence_dataset_"+table+" ON "+table,
			"CREATE TRIGGER profile_evidence_dataset_"+table+" AFTER TRUNCATE ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_dataset_revision_trigger()",
		)
	}
	for _, table := range profileEvidenceDatasetSourceTables {
		name := "profile_evidence_dataset_" + table
		statements = append(statements,
			"DROP TRIGGER IF EXISTS "+name+"_insert ON "+table,
			"DROP TRIGGER IF EXISTS "+name+"_update ON "+table,
			"DROP TRIGGER IF EXISTS "+name+"_delete ON "+table,
			"CREATE TRIGGER "+name+"_insert AFTER INSERT ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_dataset_revision_trigger()",
			"CREATE TRIGGER "+name+"_update AFTER UPDATE ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_dataset_revision_trigger()",
			"CREATE TRIGGER "+name+"_delete AFTER DELETE ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_dataset_revision_trigger()",
			"DROP TRIGGER IF EXISTS "+name+"_truncate ON "+table,
			"CREATE TRIGGER "+name+"_truncate AFTER TRUNCATE ON "+table+" FOR EACH STATEMENT EXECUTE FUNCTION profile_evidence_bump_dataset_revision_trigger()",
		)
	}
	return statements
}
