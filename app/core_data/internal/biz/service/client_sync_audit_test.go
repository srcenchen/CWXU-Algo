package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newClientSyncAuditUseCase(t *testing.T) (*SpiderUseCase, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	return &SpiderUseCase{data: &coredata.Data{DB: db}}, db
}

func TestClientSyncAuditLifecycleIsIdempotentAndRetainedSevenDays(t *testing.T) {
	uc, db := newClientSyncAuditUseCase(t)
	ctx := context.Background()
	started := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	start := ClientSyncAuditStart{SessionID: "session-1", AuthorizationID: 12, UserID: 34, Platform: "luogu", OJUID: "998877", ClientKind: "userscript", ClientVersion: "0.2.0", StartedAt: started}
	if err := uc.StartClientSyncAudit(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := uc.StartClientSyncAudit(ctx, start); err != nil {
		t.Fatalf("replayed start: %v", err)
	}
	page := ClientSyncAuditProgress{SessionID: "session-1", ProcessedPages: 2, RemoteCount: 41, Inserted: 5, RestartCount: 1, UpdatedAt: started.Add(time.Minute)}
	if err := uc.UpdateClientSyncAudit(ctx, page); err != nil {
		t.Fatal(err)
	}
	if err := uc.UpdateClientSyncAudit(ctx, page); err != nil {
		t.Fatalf("replayed page: %v", err)
	}
	terminal := started.Add(2 * time.Minute)
	if err := uc.TerminateClientSyncAudit(ctx, "session-1", "completed", "remote_end", "", "", terminal); err != nil {
		t.Fatal(err)
	}
	if err := uc.TerminateClientSyncAudit(ctx, "session-1", "failed", "", "LATE", "must not replace terminal state", terminal.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var row model.ClientSyncAudit
	if err := db.First(&row, "session_id = ?", "session-1").Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "completed" || row.ProcessedPages != 2 || row.Inserted != 5 || row.TerminalAt == nil || !row.RetentionUntil.Equal(terminal.Add(7*24*time.Hour)) {
		t.Fatalf("unexpected audit: %+v", row)
	}
}

func TestUpdateClientSyncAuditNeverRegressesCounters(t *testing.T) {
	uc, db := newClientSyncAuditUseCase(t)
	now := time.Now().UTC()
	if err := db.Create(&model.ClientSyncAudit{SessionID: "monotonic", AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "running", StartedAt: now, UpdatedAt: now, ProcessedPages: 8, RemoteCount: 100, Inserted: 20, RestartCount: 3}).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.UpdateClientSyncAudit(context.Background(), ClientSyncAuditProgress{SessionID: "monotonic", ProcessedPages: 2, RemoteCount: 40, Inserted: 5, RestartCount: 1, UpdatedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var row model.ClientSyncAudit
	if err := db.First(&row, "session_id = ?", "monotonic").Error; err != nil {
		t.Fatal(err)
	}
	if row.ProcessedPages != 8 || row.RemoteCount != 100 || row.Inserted != 20 || row.RestartCount != 3 {
		t.Fatalf("counters regressed: %+v", row)
	}
}

func TestCleanupClientSyncAuditsHonorsBoundaryRunningAndBatchLimit(t *testing.T) {
	uc, db := newClientSyncAuditUseCase(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	rows := make([]model.ClientSyncAudit, 0, 503)
	for i := 0; i < 501; i++ {
		terminal := now.Add(-7*24*time.Hour - time.Second)
		rows = append(rows, model.ClientSyncAudit{SessionID: fmt.Sprintf("expired-%03d", i), AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "completed", StartedAt: terminal, UpdatedAt: terminal, TerminalAt: &terminal, RetentionUntil: terminal.Add(7 * 24 * time.Hour)})
	}
	boundaryTerminal := now.Add(-7 * 24 * time.Hour)
	rows = append(rows,
		model.ClientSyncAudit{SessionID: "boundary", AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "completed", StartedAt: boundaryTerminal, UpdatedAt: boundaryTerminal, TerminalAt: &boundaryTerminal, RetentionUntil: now},
		model.ClientSyncAudit{SessionID: "running", AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "running", StartedAt: now.Add(-30 * 24 * time.Hour), UpdatedAt: now.Add(-30 * 24 * time.Hour)},
	)
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.cleanupClientSyncAudits(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ClientSyncAudit{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("remaining = %d, want 3 (one expired beyond batch, boundary, running)", count)
	}
	for _, id := range []string{"boundary", "running"} {
		if err := db.First(&model.ClientSyncAudit{}, "session_id = ?", id).Error; err != nil {
			t.Fatalf("%s was deleted: %v", id, err)
		}
	}
}

func TestExpireClientSyncAuditsTerminalizesOnlyStaleRunningRows(t *testing.T) {
	uc, db := newClientSyncAuditUseCase(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	rows := []model.ClientSyncAudit{
		{SessionID: "stale", AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "running", StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-30 * time.Minute)},
		{SessionID: "fresh", AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "running", StartedAt: now, UpdatedAt: now.Add(-29 * time.Minute)},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.expireClientSyncAudits(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var stale, fresh model.ClientSyncAudit
	if err := db.First(&stale, "session_id = ?", "stale").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&fresh, "session_id = ?", "fresh").Error; err != nil {
		t.Fatal(err)
	}
	if stale.Status != "expired" || stale.TerminalAt == nil || !stale.RetentionUntil.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("stale audit = %+v", stale)
	}
	if fresh.Status != "running" || fresh.TerminalAt != nil {
		t.Fatalf("fresh audit = %+v", fresh)
	}
}

func TestExpireClientSyncAuditsDoesNotExpireRedisLiveSession(t *testing.T) {
	uc, db := newClientSyncAuditUseCase(t)
	mr := miniredis.RunT(t)
	uc.data.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = uc.data.RDB.Close() })
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	if err := db.Create(&model.ClientSyncAudit{SessionID: "live", AuthorizationID: 1, UserID: 1, Platform: "luogu", OJUID: "1", ClientKind: "userscript", Status: "running", StartedAt: old, UpdatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.data.RDB.HSet(context.Background(), "luogu:sync:session:live", "user_id", 1).Err(); err != nil {
		t.Fatal(err)
	}
	if err := uc.expireClientSyncAudits(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var row model.ClientSyncAudit
	if err := db.First(&row, "session_id = ?", "live").Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "running" || row.TerminalAt != nil {
		t.Fatalf("live session was expired: %+v", row)
	}
}
