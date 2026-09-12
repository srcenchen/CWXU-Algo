package service

import (
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestClientSyncAuditOrderClauseSortsBySemanticVersion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []model.ClientSyncAudit{
		{SessionID: "9", ClientVersion: "0.1.9", Platform: "luogu", ClientKind: "userscript", Status: "completed", StartedAt: now, UpdatedAt: now},
		{SessionID: "13", ClientVersion: "0.1.13", Platform: "luogu", ClientKind: "userscript", Status: "completed", StartedAt: now.Add(-time.Hour), UpdatedAt: now},
		{SessionID: "beta", ClientVersion: "0.2.7-beta", Platform: "luogu", ClientKind: "userscript", Status: "completed", StartedAt: now.Add(-2 * time.Hour), UpdatedAt: now},
		{SessionID: "one", ClientVersion: "1", Platform: "luogu", ClientKind: "userscript", Status: "completed", StartedAt: now.Add(-3 * time.Hour), UpdatedAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := db.Model(&model.ClientSyncAudit{}).
		Order(clientSyncAuditOrderClause(db.Dialector.Name())).
		Pluck("client_version", &got).Error; err != nil {
		t.Fatal(err)
	}
	want := []string{"1", "0.2.7-beta", "0.1.13", "0.1.9"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestClientSyncAuditOrderClausePostgresGuardsNonNumericParts(t *testing.T) {
	clause := clientSyncAuditOrderClause("postgres")
	for _, part := range []string{
		"COALESCE(NULLIF(regexp_replace(split_part(client_version, '.', 1), '[^0-9]', '', 'g'), '')::int, 0) DESC",
		"COALESCE(NULLIF(regexp_replace(split_part(client_version, '.', 2), '[^0-9]', '', 'g'), '')::int, 0) DESC",
		"COALESCE(NULLIF(regexp_replace(split_part(client_version, '.', 3), '[^0-9]', '', 'g'), '')::int, 0) DESC",
		"started_at DESC",
	} {
		if !strings.Contains(clause, part) {
			t.Fatalf("clause %q missing %q", clause, part)
		}
	}
	if strings.Contains(clause, "split_part(client_version, '.', 2)::int") ||
		strings.Contains(clause, "split_part(client_version, '.', 3)::int") {
		t.Fatalf("clause has unguarded ::int cast: %q", clause)
	}
}
