package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	spiderregistry "cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spider/platform"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type failGenerationRedisHook struct{}

type failPendingScheduleOnceHook struct {
	once sync.Once
}

type staleGenerationProvider struct {
	rdb *redis.Client
}

func (*staleGenerationProvider) Name() string { return "ReviewStaleGeneration" }
func (p *staleGenerationProvider) FetchSubmitLog(ctx context.Context, userID int64, _ string, _ bool) ([]model.SubmitLog, error) {
	if err := p.rdb.Incr(ctx, task.GenerationKey(userID, p.Name())).Err(); err != nil {
		return nil, err
	}
	return []model.SubmitLog{{SubmitID: "stale-1", Status: "AC", Time: time.Now()}}, nil
}

func (failGenerationRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failGenerationRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && strings.HasPrefix(fmt.Sprint(cmd.Args()[1]), "spider:gen:") {
			return errors.New("generation redis unavailable")
		}
		return next(ctx, cmd)
	}
}
func (failGenerationRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failPendingScheduleOnceHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failPendingScheduleOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "zadd" && len(cmd.Args()) > 1 && fmt.Sprint(cmd.Args()[1]) == pendingVerdictDueZKey {
			failed := false
			h.once.Do(func() { failed = true })
			if failed {
				return errors.New("pending schedule unavailable")
			}
		}
		return next(ctx, cmd)
	}
}
func (h *failPendingScheduleOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func newSubmitImporterForTest(t *testing.T) (*SpiderUseCase, *gorm.DB, *redis.Client) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{},
		&model.DailyUserStat{},
		&model.UserACProblem{},
		&model.UserACProblemDay{},
	); err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &SpiderUseCase{data: &data.Data{DB: db, RDB: rdb}}, db, rdb
}

func TestImportSubmitLogsIsIdempotentAndKeepsAggregates(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	ctx := context.Background()
	const generation int64 = 3
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), generation, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	logs := []model.SubmitLog{{
		UserID: 999, Platform: "wrong", SubmitID: "LuoGu:90002",
		Problem: "P1001 A+B Problem", ExternalID: "P1001", Lang: "C++14 (GCC 9)", Status: "AC",
		Time: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	}}

	first, err := uc.ImportSubmitLogs(ctx, 7, "LuoGu", generation, logs)
	if err != nil {
		t.Fatal(err)
	}
	if first.Inserted != 1 {
		t.Fatalf("inserted=%d want=1", first.Inserted)
	}
	second, err := uc.ImportSubmitLogs(ctx, 7, "LuoGu", generation, logs)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted != 0 {
		t.Fatalf("retry inserted=%d want=0", second.Inserted)
	}

	var stored model.SubmitLog
	if err := db.First(&stored, "platform = ? AND submit_id = ?", "LuoGu", "90002").Error; err != nil {
		t.Fatal(err)
	}
	if stored.UserID != 7 || stored.ExternalID != "P1001" || !stored.IsAC {
		t.Fatalf("stored=%+v", stored)
	}
	var daily model.DailyUserStat
	if err := db.First(&daily, "user_id = ? AND platform = ?", 7, "LuoGu").Error; err != nil {
		t.Fatal(err)
	}
	if daily.SubmitCnt != 1 || daily.AcCnt != 1 {
		t.Fatalf("daily submit=%d ac=%d", daily.SubmitCnt, daily.AcCnt)
	}
	var lifetime, days int64
	if err := db.Model(&model.UserACProblem{}).Where("user_id = ?", 7).Count(&lifetime).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.UserACProblemDay{}).Where("user_id = ?", 7).Count(&days).Error; err != nil {
		t.Fatal(err)
	}
	if lifetime != 1 || days != 1 {
		t.Fatalf("lifetime=%d days=%d", lifetime, days)
	}
	if version, err := rdb.Get(ctx, "statistic:user:7:ver").Int64(); err != nil || version != 1 {
		t.Fatalf("user cache version=%d err=%v", version, err)
	}
}

func TestSubmitOwnerConflict(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	ctx := context.Background()
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), 1, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	existing := model.SubmitLog{UserID: 8, Platform: "LuoGu", SubmitID: "42", Status: "WA", Time: time.Now()}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	_, err := uc.ImportSubmitLogs(ctx, 7, "LuoGu", 1, []model.SubmitLog{{SubmitID: "42", Status: "AC", Time: time.Now()}})
	if err == nil || kratoserrors.FromError(err).Reason != "SUBMIT_OWNER_CONFLICT" {
		t.Fatalf("err=%v reason=%q", err, kratoserrors.FromError(err).Reason)
	}
}

func TestSubmitOwnerClaimRaceDoesNotPolluteLoserAggregates(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	ctx := context.Background()
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), 1, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	var inject sync.Once
	if err := db.Callback().Query().After("gorm:query").Register("test:inject-submit-owner-race", func(tx *gorm.DB) {
		if tx.Statement.Table != "submit_logs" {
			return
		}
		if _, ok := tx.Statement.Dest.(*[]string); !ok {
			return
		}
		inject.Do(func() {
			owner := model.SubmitLog{UserID: 8, Platform: "LuoGu", SubmitID: "42", Status: "WA", Time: time.Now()}
			if err := db.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Create(&owner).Error; err != nil {
				t.Errorf("inject competing owner: %v", err)
			}
		})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove("test:inject-submit-owner-race") })

	result, err := uc.ImportSubmitLogs(ctx, 7, "LuoGu", 1, []model.SubmitLog{{
		SubmitID: "42", ExternalID: "P1001", Status: "AC", Time: time.Now(),
	}})
	if luoguReason := kratoserrors.Reason(err); luoguReason != "SUBMIT_OWNER_CONFLICT" {
		t.Fatalf("result=%+v err=%v reason=%q", result, err, luoguReason)
	}
	if result.Inserted != 0 {
		t.Fatalf("loser reported inserted=%d", result.Inserted)
	}

	var dailyCount int64
	if err := db.Model(&model.DailyUserStat{}).Where("user_id = ?", 7).Count(&dailyCount).Error; err != nil {
		t.Fatal(err)
	}
	var acCount int64
	if err := db.Model(&model.UserACProblem{}).Where("user_id = ?", 7).Count(&acCount).Error; err != nil {
		t.Fatal(err)
	}
	if dailyCount != 0 || acCount != 0 {
		t.Fatalf("loser aggregates polluted: daily=%d ac=%d", dailyCount, acCount)
	}
}

func TestImportSubmitLogsRejectsChangedGeneration(t *testing.T) {
	uc, _, rdb := newSubmitImporterForTest(t)
	ctx := context.Background()
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), 4, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	_, err := uc.ImportSubmitLogs(ctx, 7, "LuoGu", 3, []model.SubmitLog{{SubmitID: "43", Status: "AC", Time: time.Now()}})
	if err == nil || kratoserrors.FromError(err).Reason != "SYNC_BINDING_CHANGED" {
		t.Fatalf("err=%v reason=%q", err, kratoserrors.FromError(err).Reason)
	}
}

func TestImportAndCompleteClientSyncFailClosedWhenGenerationUnavailable(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	if err := db.AutoMigrate(&model.Platform{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Platform{UserID: 7, Platform: "LuoGu", Username: "2245873"}).Error; err != nil {
		t.Fatal(err)
	}
	rdb.AddHook(failGenerationRedisHook{})

	_, err := uc.ImportSubmitLogs(context.Background(), 7, "LuoGu", 0, []model.SubmitLog{{
		SubmitID: "44", Status: "AC", Time: time.Now(),
	}})
	if kratoserrors.Reason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("import err=%v reason=%q", err, kratoserrors.Reason(err))
	}
	if err := uc.CompleteClientSync(context.Background(), 7, "LuoGu", 0, "44", time.Now()); kratoserrors.Reason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("complete err=%v reason=%q", err, kratoserrors.Reason(err))
	}
}

func TestServerSpiderSilentlyDiscardsStaleGeneration(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	provider := &staleGenerationProvider{rdb: rdb}
	spiderregistry.Register(provider)
	if err := rdb.Set(context.Background(), task.GenerationKey(7, provider.Name()), 1, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	rows, complete, err := uc.fetchAndSave(context.Background(), 7, model.Platform{
		UserID: 7, Platform: provider.Name(), Username: "bound-user",
	}, false, false)
	if err != nil || rows != 0 || complete {
		t.Fatalf("rows=%d complete=%v err=%v", rows, complete, err)
	}
	var count int64
	if err := db.Model(&model.SubmitLog{}).Where("platform = ?", provider.Name()).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale server fetch inserted %d rows", count)
	}
}

func TestContestWriteAlwaysGetsFinalCacheInvalidation(t *testing.T) {
	uc, _, rdb := newSubmitImporterForTest(t)
	ctx := context.Background()
	key := "core:contest_log:user:7:ver"
	if err := rdb.Set(ctx, key, 1, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	uc.invalidateAfterContestWrite(7, true)
	if version, err := rdb.Get(ctx, key).Int64(); err != nil || version != 2 {
		t.Fatalf("contest cache version=%d err=%v", version, err)
	}
}

func TestLuoGuRecordToSubmitLogMapsRawValues(t *testing.T) {
	record := platform.Record{ID: 90002, SubmitTime: 1787846400, Status: 12, Language: 28}
	record.Problem.Pid = "P1001"
	record.Problem.Title = "A+B Problem"
	log := platform.LuoGuRecordToSubmitLog(7, record)
	if log.UserID != 7 || log.Platform != "LuoGu" || log.SubmitID != "90002" {
		t.Fatalf("identity=%+v", log)
	}
	if log.Status != "AC" || log.Lang != "C++14 (GCC 9)" {
		t.Fatalf("status/lang=%q/%q", log.Status, log.Lang)
	}
	if log.ExternalID != "P1001" || log.Problem != "P1001 A+B Problem" {
		t.Fatalf("problem=%q external=%q", log.Problem, log.ExternalID)
	}
}

func createClientSyncMaintenanceTablesForRedTest(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&model.Platform{}, &model.ClientSyncPageReceipt{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE TABLE IF NOT EXISTS client_sync_post_process_jobs (
			session_id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			platform TEXT NOT NULL,
			dirty BOOLEAN NOT NULL DEFAULT FALSE,
			receipt_count INTEGER NOT NULL DEFAULT 0,
			ready_at DATETIME NOT NULL,
			lease_until DATETIME,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			completed_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)
	`).Error; err != nil {
		t.Fatal(err)
	}
}

func importDirtyClientPageForTest(t *testing.T, uc *SpiderUseCase, db *gorm.DB, rdb *redis.Client, sessionID, completion string, now time.Time) {
	t.Helper()
	createClientSyncMaintenanceTablesForRedTest(t, db)
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").FirstOrCreate(&model.Platform{
		UserID: 7, Platform: "LuoGu", Username: "2245873",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), task.GenerationKey(7, "LuoGu"), 2, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	_, err := uc.ImportClientSyncPage(context.Background(), 7, "LuoGu", 2, []model.SubmitLog{{
		SubmitID: sessionID + "-submit", ExternalID: "P1001", Status: "AC", Time: now,
	}}, ClientSyncPageImport{
		SessionID: sessionID, Page: 1, Digest: strings.Repeat("a", 64),
		FirstSubmitID: sessionID + "-submit", RemoteCount: 2, PerPage: 1,
		CompletionReason: completion, CompletedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientSyncReceiptSchedulesPendingRetryWithoutNewRows(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	uc.spiderTask = task.NewSpiderTask(nil, rdb, db)
	ctx := context.Background()
	const generation int64 = 2
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), generation, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	existing := model.SubmitLog{
		UserID: 7, Platform: "LuoGu", SubmitID: "pending-1", ExternalID: "P1001",
		Status: "Judging", Time: now,
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	_, err := uc.ImportClientSyncPage(ctx, 7, "LuoGu", generation, []model.SubmitLog{existing}, ClientSyncPageImport{
		SessionID: "pending-session", Page: 1, Digest: strings.Repeat("d", 64),
		FirstSubmitID: existing.SubmitID, RemoteCount: 2, PerPage: 1,
		ExpiresAt: now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.ZScore(ctx, pendingVerdictDueZKey, pendingVerdictMember(7, "LuoGu")).Result(); err != nil {
		t.Fatalf("pending retry was not durably scheduled: %v", err)
	}
}

func TestClientSyncReceiptReplayRepairsFailedPendingSchedule(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	uc.spiderTask = task.NewSpiderTask(nil, rdb, db)
	rdb.AddHook(&failPendingScheduleOnceHook{})
	ctx := context.Background()
	const generation int64 = 2
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), generation, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	page := ClientSyncPageImport{
		SessionID: "pending-replay", Page: 1, Digest: strings.Repeat("e", 64),
		FirstSubmitID: "pending-new", RemoteCount: 2, PerPage: 1,
		ExpiresAt: now.Add(30 * time.Minute),
	}
	logs := []model.SubmitLog{{
		SubmitID: "pending-new", ExternalID: "P1001", Status: "Judging", Time: now,
	}}
	if _, err := uc.ImportClientSyncPage(ctx, 7, "LuoGu", generation, logs, page); kratoserrors.Reason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("first err=%v reason=%q", err, kratoserrors.Reason(err))
	}
	if _, err := uc.ImportClientSyncPage(ctx, 7, "LuoGu", generation, logs, page); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.ZScore(ctx, pendingVerdictDueZKey, pendingVerdictMember(7, "LuoGu")).Result(); err != nil {
		t.Fatalf("receipt replay did not repair pending schedule: %v", err)
	}
}

func TestClientSyncMaintenanceRepairsFailedPendingOnlyReceipt(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	uc.spiderTask = task.NewSpiderTask(nil, rdb, db)
	rdb.AddHook(&failPendingScheduleOnceHook{})
	ctx := context.Background()
	const generation int64 = 2
	if err := rdb.Set(ctx, task.GenerationKey(7, "LuoGu"), generation, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	existing := model.SubmitLog{
		UserID: 7, Platform: "LuoGu", SubmitID: "pending-only", ExternalID: "P1001",
		Status: "Judging", Time: now,
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	_, err := uc.ImportClientSyncPage(ctx, 7, "LuoGu", generation, []model.SubmitLog{existing}, ClientSyncPageImport{
		SessionID: "pending-only-maintenance", Page: 1, Digest: strings.Repeat("f", 64),
		FirstSubmitID: existing.SubmitID, RemoteCount: 2, PerPage: 1,
		ExpiresAt: now.Add(30 * time.Minute),
	})
	if kratoserrors.Reason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("first err=%v reason=%q", err, kratoserrors.Reason(err))
	}
	if err := uc.RunClientSyncMaintenanceOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.ZScore(ctx, pendingVerdictDueZKey, pendingVerdictMember(7, "LuoGu")).Result(); err != nil {
		t.Fatalf("maintenance did not repair pending-only receipt: %v", err)
	}
}

func TestClientSyncCleanupKeepsExpiredUnappliedPendingOnlyReceipt(t *testing.T) {
	uc, db, _ := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	now := time.Now().UTC()
	receipt := model.ClientSyncPageReceipt{
		SessionID: "expired-pending-only", Page: 1, Digest: strings.Repeat("f", 64),
		UserID: 7, Platform: "LuoGu", HasPending: true, ExpiresAt: now.Add(-time.Minute),
	}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.cleanupClientSyncReceipts(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ClientSyncPageReceipt{}).
		Where("session_id = ?", receipt.SessionID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unapplied pending-only receipts=%d want=1", count)
	}
}

func TestClientSyncReceiptPersistsDirtyJobForCompletionAndExpiry(t *testing.T) {
	tests := []struct {
		name       string
		completion string
		wantReady  time.Duration
	}{
		{name: "natural expiry", wantReady: 30 * time.Minute},
		{name: "normal completion", completion: "remote_end", wantReady: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc, db, rdb := newSubmitImporterForTest(t)
			now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
			importDirtyClientPageForTest(t, uc, db, rdb, "session-"+strings.ReplaceAll(tt.name, " ", "-"), tt.completion, now)
			var row struct {
				ReadyAt time.Time
			}
			if err := db.Raw("SELECT ready_at FROM client_sync_post_process_jobs LIMIT 1").Scan(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.ReadyAt.IsZero() || row.ReadyAt.Before(now.Add(tt.wantReady-time.Second)) || row.ReadyAt.After(now.Add(tt.wantReady+time.Second)) {
				t.Fatalf("dirty job ready_at=%v want around %v", row.ReadyAt, now.Add(tt.wantReady))
			}
		})
	}
}

func TestClientSyncExplicitTerminationMakesUncheckpointedDirtyJobReady(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	importDirtyClientPageForTest(t, uc, db, rdb, "session-terminate", "", now)
	type terminationMarker interface {
		MarkClientSyncSessionTerminated(context.Context, string, time.Time) error
	}
	marker, ok := any(uc).(terminationMarker)
	if !ok {
		t.Fatal("SpiderUseCase does not persist explicit client-sync termination")
	}
	terminatedAt := now.Add(time.Minute)
	if err := marker.MarkClientSyncSessionTerminated(context.Background(), "session-terminate", terminatedAt); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("client_sync_post_process_jobs").Where("session_id = ? AND ready_at <= ?", "session-terminate", terminatedAt).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("ready termination jobs=%d err=%v", count, err)
	}
}

func TestClientSyncMaintenanceProcessesNaturalExpiryOnce(t *testing.T) {
	uc, db, _ := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	now := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	if err := db.Exec(`INSERT INTO client_sync_post_process_jobs
		(session_id,user_id,platform,dirty,ready_at,attempts,last_error,created_at,updated_at)
		VALUES (?,?,?,TRUE,?,0,'',?,?)`, "expired-session", 99, "LuoGu", now.Add(-time.Minute), now.Add(-time.Hour), now.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	uc.problem = &ProblemUseCase{data: &data.Data{DB: db}}
	type maintenanceRunner interface {
		RunClientSyncMaintenanceOnce(context.Context, time.Time) error
	}
	runner, ok := any(uc).(maintenanceRunner)
	if !ok {
		t.Fatal("SpiderUseCase has no persistent client-sync maintenance worker")
	}
	if err := runner.RunClientSyncMaintenanceOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunClientSyncMaintenanceOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Attempts    int
		CompletedAt *time.Time
	}
	if err := db.Raw("SELECT attempts,completed_at FROM client_sync_post_process_jobs WHERE session_id = ?", "expired-session").Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 1 || row.CompletedAt == nil {
		t.Fatalf("maintenance job=%+v want one completed attempt", row)
	}
}

func TestClientSyncMaintenanceCleansExpiredReceiptsInBoundedBatches(t *testing.T) {
	uc, db, _ := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	now := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	receipts := make([]model.ClientSyncPageReceipt, 0, 502)
	for page := int32(1); page <= 501; page++ {
		receipts = append(receipts, model.ClientSyncPageReceipt{
			SessionID: "cleanup-session", Page: page, Digest: fmt.Sprintf("%064d", page),
			UserID: 7, Platform: "LuoGu", ExpiresAt: now.Add(-time.Minute),
		})
	}
	receipts = append(receipts, model.ClientSyncPageReceipt{
		SessionID: "future-session", Page: 1, Digest: strings.Repeat("f", 64),
		UserID: 7, Platform: "LuoGu", ExpiresAt: now.Add(time.Hour),
	})
	if err := db.CreateInBatches(&receipts, 200).Error; err != nil {
		t.Fatal(err)
	}
	type maintenanceRunner interface {
		RunClientSyncMaintenanceOnce(context.Context, time.Time) error
	}
	runner, ok := any(uc).(maintenanceRunner)
	if !ok {
		t.Fatal("SpiderUseCase has no receipt retention worker")
	}
	if err := runner.RunClientSyncMaintenanceOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var expired, future int64
	if err := db.Model(&model.ClientSyncPageReceipt{}).Where("expires_at <= ?", now).Count(&expired).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ClientSyncPageReceipt{}).Where("expires_at > ?", now).Count(&future).Error; err != nil {
		t.Fatal(err)
	}
	if expired != 1 || future != 1 {
		t.Fatalf("cleanup expired=%d future=%d want 1/1 after one 500-row batch", expired, future)
	}
}

func TestClientSyncMaintenanceCleansExpiredJobsInBoundedBatches(t *testing.T) {
	uc, db, _ := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	now := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	jobs := make([]model.ClientSyncPostProcessJob, 0, 501)
	for index := 0; index < 501; index++ {
		jobs = append(jobs, model.ClientSyncPostProcessJob{
			SessionID: fmt.Sprintf("clean-job-%d", index), UserID: 7, Platform: "LuoGu",
			ReadyAt: now.Add(-time.Minute),
		})
	}
	if err := db.CreateInBatches(&jobs, 200).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.RunClientSyncMaintenanceOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := db.Model(&model.ClientSyncPostProcessJob{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining jobs=%d want=1 after one 500-row batch", remaining)
	}
}

func TestClientSyncReceiptCountIsBoundedPerSession(t *testing.T) {
	uc, db, rdb := newSubmitImporterForTest(t)
	createClientSyncMaintenanceTablesForRedTest(t, db)
	if err := rdb.Set(context.Background(), task.GenerationKey(7, "LuoGu"), 2, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	receipts := make([]model.ClientSyncPageReceipt, 0, 5000)
	for page := int32(1); page <= 5000; page++ {
		receipts = append(receipts, model.ClientSyncPageReceipt{
			SessionID: "bounded-session", Page: page, Digest: fmt.Sprintf("%064d", page),
			UserID: 7, Platform: "LuoGu", Generation: 2, ExpiresAt: now.Add(time.Hour),
		})
	}
	if err := db.CreateInBatches(&receipts, 250).Error; err != nil {
		t.Fatal(err)
	}
	_, err := uc.ImportClientSyncPage(context.Background(), 7, "LuoGu", 2, nil, ClientSyncPageImport{
		SessionID: "bounded-session", Restart: 3, Page: 5001, Digest: strings.Repeat("e", 64),
		RemoteCount: 100_000, PerPage: 20, ExpiresAt: now.Add(time.Hour),
	})
	if kratoserrors.Reason(err) != "LUOGU_RECORDS_CHANGED" {
		t.Fatalf("receipt cap err=%v reason=%q", err, kratoserrors.Reason(err))
	}
}

func TestClientSyncReceiptConcurrentUniqueConflictReloadsWinner(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "receipt-race.sqlite") + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	winnerDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}, &model.ClientSyncPageReceipt{},
	); err != nil {
		t.Fatal(err)
	}
	uc := &SpiderUseCase{data: &data.Data{DB: db}}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	page := ClientSyncPageImport{
		SessionID: "receipt-race", Page: 1, Digest: strings.Repeat("c", 64),
		InsertedBefore: 4, RemoteCount: 10, PerPage: 10,
		ExpiresAt: now.Add(time.Hour),
	}
	winner := model.ClientSyncPageReceipt{
		SessionID: page.SessionID, Page: page.Page, Digest: page.Digest,
		UserID: 7, Platform: "LuoGu", PageInserted: 0, Inserted: 4,
		ProcessedPages: 1, NextPage: 2, RemoteCount: 10, PerPage: 10,
		ExpiresAt: page.ExpiresAt,
	}
	var receiptQueries atomic.Int32
	if err := db.Callback().Query().After("gorm:query").Register("test:inject-receipt-winner", func(tx *gorm.DB) {
		if tx.Statement.Table != "client_sync_page_receipts" || receiptQueries.Add(1) != 2 {
			return
		}
		if createErr := winnerDB.Create(&winner).Error; createErr != nil {
			t.Errorf("inject receipt winner: %v", createErr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove("test:inject-receipt-winner") })

	result, err := uc.ImportClientSyncPage(context.Background(), 7, "LuoGu", 0, nil, page)
	if err != nil {
		t.Fatalf("receipt unique loser leaked database error: %v", err)
	}
	if !result.Replayed || result.Inserted != 4 || result.NextPage != 2 {
		t.Fatalf("winner receipt not replayed: %+v", result)
	}
}
