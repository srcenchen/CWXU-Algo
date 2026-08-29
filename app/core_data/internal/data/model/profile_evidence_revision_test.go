package model

import (
	"strings"
	"testing"
)

func TestProfileEvidenceBackfillSQLUsesPortableBooleanDisambiguator(t *testing.T) {
	sql := profileEvidenceBackfillSQL()
	if strings.Contains(sql, "WHERE 1") {
		t.Fatalf("PostgreSQL-incompatible WHERE 1 in backfill SQL: %s", sql)
	}
	if !strings.Contains(sql, "WHERE TRUE") {
		t.Fatalf("backfill SQL must use portable boolean disambiguator: %s", sql)
	}
}
