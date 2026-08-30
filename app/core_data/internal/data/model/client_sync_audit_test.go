package model

import (
	"reflect"
	"testing"
)

func TestClientSyncAuditContainsOnlySessionSummaryFields(t *testing.T) {
	typ := reflect.TypeOf(ClientSyncAudit{})
	want := []string{"SessionID", "AuthorizationID", "UserID", "Platform", "OJUID", "ClientKind", "ClientVersion", "Status", "CompletionReason", "StartedAt", "UpdatedAt", "TerminalAt", "RetentionUntil", "ProcessedPages", "RemoteCount", "Inserted", "RestartCount", "ErrorCode", "ErrorMessage"}
	if typ.NumField() != len(want) {
		t.Fatalf("field count = %d, want %d", typ.NumField(), len(want))
	}
	for i, name := range want {
		if typ.Field(i).Name != name {
			t.Fatalf("field %d = %s, want %s", i, typ.Field(i).Name, name)
		}
	}
	if field, _ := typ.FieldByName("RetentionUntil"); field.Tag.Get("gorm") != "index:idx_client_sync_audits_retention_terminal,priority:1" {
		t.Fatalf("retention_until gorm tag = %q, want index", field.Tag.Get("gorm"))
	}
	if field, _ := typ.FieldByName("TerminalAt"); field.Tag.Get("gorm") != "index:idx_client_sync_audits_retention_terminal,priority:2" {
		t.Fatalf("terminal_at gorm tag = %q, want composite retention index", field.Tag.Get("gorm"))
	}
}
