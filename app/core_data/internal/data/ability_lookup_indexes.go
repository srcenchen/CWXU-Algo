package data

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// abilityLookupIndexSpec is deliberately separate from GORM model tags. These
// optional read-path indexes can scan large tables and must not be built by the
// blocking CREATE INDEX statements emitted from startup AutoMigrate.
type abilityLookupIndexSpec struct {
	name              string
	table             string
	columns           string
	postgresColumns   string
	portableColumns   string
	postgresPredicate string
	portablePredicate string
}

const (
	postgresAbilityExternalEmbeddedPlatform = "BTRIM(SUBSTRING(problem_key FROM 3 FOR GREATEST(POSITION(':' IN SUBSTRING(problem_key FROM 3)) - 1, 0)))"
	postgresAbilityExternalID               = "BTRIM(SUBSTRING(problem_key FROM 3 + POSITION(':' IN SUBSTRING(problem_key FROM 3))))"
	postgresAbilityExternalPredicate        = "LEFT(problem_key, 2) = 'e:' AND POSITION(':' IN SUBSTRING(problem_key FROM 3)) > 0" +
		" AND " + postgresAbilityExternalEmbeddedPlatform + " <> '' AND " + postgresAbilityExternalID + " <> ''" +
		" AND NOT (LOWER(" + postgresAbilityExternalEmbeddedPlatform + ") = 'leetcode' AND LEFT(LOWER(" + postgresAbilityExternalID + "), 3) = 'ac-')"

	portableAbilityExternalEmbeddedPlatform = "TRIM(SUBSTR(problem_key, 3, INSTR(SUBSTR(problem_key, 3), ':') - 1))"
	portableAbilityExternalID               = "TRIM(SUBSTR(problem_key, 3 + INSTR(SUBSTR(problem_key, 3), ':')))"
	portableAbilityExternalPredicate        = "SUBSTR(problem_key, 1, 2) = 'e:' AND INSTR(SUBSTR(problem_key, 3), ':') > 0" +
		" AND " + portableAbilityExternalEmbeddedPlatform + " <> '' AND " + portableAbilityExternalID + " <> ''" +
		" AND NOT (LOWER(" + portableAbilityExternalEmbeddedPlatform + ") = 'leetcode' AND SUBSTR(LOWER(" + portableAbilityExternalID + "), 1, 3) = 'ac-')"
)

var abilityLookupIndexes = []abilityLookupIndexSpec{
	{name: "idx_uac_problem_key", table: "user_ac_problems", columns: "problem_key"},
	{name: "idx_uac_ability_platform_key", table: "user_ac_problems", columns: "LOWER(TRIM(platform)), problem_key"},
	{
		name:  "idx_uac_ability_external_identity",
		table: "user_ac_problems",
		postgresColumns: "LOWER(BTRIM(platform)), " +
			"LOWER(" + postgresAbilityExternalEmbeddedPlatform + "), " + postgresAbilityExternalID,
		portableColumns: "LOWER(TRIM(platform)), " +
			"LOWER(" + portableAbilityExternalEmbeddedPlatform + "), " + portableAbilityExternalID,
		postgresPredicate: postgresAbilityExternalPredicate,
		portablePredicate: portableAbilityExternalPredicate,
	},
	{name: "idx_problem_ability_external_lookup", table: "problems", columns: "LOWER(TRIM(platform)), TRIM(external_id)"},
	{name: "idx_submit_user_problem", table: "submit_logs", columns: "user_id, problem_id"},
	{name: "idx_submit_user_ability_external", table: "submit_logs", columns: "user_id, LOWER(TRIM(platform)), TRIM(external_id)"},
}

func (spec abilityLookupIndexSpec) postgresDDL() string {
	columns := spec.columns
	if spec.postgresColumns != "" {
		columns = spec.postgresColumns
	}
	ddl := fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s)", spec.name, spec.table, columns)
	if spec.postgresPredicate != "" {
		ddl += " WHERE " + spec.postgresPredicate
	}
	return ddl
}

func (spec abilityLookupIndexSpec) portableDDL() string {
	columns := spec.columns
	if spec.portableColumns != "" {
		columns = spec.portableColumns
	}
	ddl := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", spec.name, spec.table, columns)
	if spec.portablePredicate != "" {
		ddl += " WHERE " + spec.portablePredicate
	}
	return ddl
}

type postgresAbilityLookupIndexState struct {
	Exists bool `gorm:"column:index_exists"`
	Valid  bool `gorm:"column:valid"`
}

func loadPostgresAbilityLookupIndexState(db *gorm.DB, name string) (postgresAbilityLookupIndexState, error) {
	var state postgresAbilityLookupIndexState
	err := db.Raw(`SELECT TRUE AS index_exists, i.indisvalid AS valid
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = ? AND n.nspname = current_schema()
		LIMIT 1`, name).Scan(&state).Error
	return state, err
}

func ensurePostgresAbilityLookupIndex(db *gorm.DB, spec abilityLookupIndexSpec) error {
	state, err := loadPostgresAbilityLookupIndexState(db, spec.name)
	if err != nil {
		return err
	}
	if state.Exists && state.Valid {
		return nil
	}
	if state.Exists {
		if err := db.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + spec.name).Error; err != nil {
			return err
		}
	}
	return db.Exec(spec.postgresDDL()).Error
}

// migrateAbilityLookupIndexes runs only after AutoMigrate has returned. A
// failed optional performance index remains observable to the caller, while
// the remaining indexes are still attempted and core-data startup can proceed.
func migrateAbilityLookupIndexes(db *gorm.DB) error {
	if db == nil || db.Dialector == nil {
		return errors.New("ability lookup indexes: nil database")
	}
	var errs []error
	for _, spec := range abilityLookupIndexes {
		var err error
		if db.Dialector.Name() == "postgres" {
			err = ensurePostgresAbilityLookupIndex(db, spec)
		} else {
			err = db.Exec(spec.portableDDL()).Error
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.name, err))
		}
	}
	return errors.Join(errs...)
}
