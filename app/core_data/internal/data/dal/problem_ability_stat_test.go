package dal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func problemAbilityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.Problem{}, &model.Platform{},
		&model.AbilityModelState{}, &model.ProblemAbilityStat{}, &model.AbilityMaintenancePending{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestRefreshProblemAbilityStatsScansBeforeTakingPublicationLock(t *testing.T) {
	db := problemAbilityTestDB(t)
	var events []string
	if err := db.Callback().Query().Before("gorm:query").Register("test:ability_publication_lock_order", func(tx *gorm.DB) {
		if tx.Statement.Table == "ability_model_state" {
			if _, locked := tx.Statement.Clauses["FOR"]; locked {
				events = append(events, "lock")
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Row().Before("gorm:row").Register("test:ability_evidence_scan_order", func(tx *gorm.DB) {
		if strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "with terminal as") {
			events = append(events, "evidence")
		}
	}); err != nil {
		t.Fatal(err)
	}

	if err := RefreshProblemAbilityStats(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0] != "evidence" || events[1] != "lock" {
		t.Fatalf("heavy evidence scan must precede the publication row lock: %v", events)
	}
}

func TestRefreshProblemAbilityStatsForMaintenanceRollsBackActiveSwitchWithTransition(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	now := time.Now()
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 4, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	pending := model.AbilityMaintenancePending{
		Scope: "problem:atomic", OperationID: "atomic-intent", Revision: 3, Phase: "facts",
		LeaseOwner: "owner", Operation: "problem", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected transition failure")
	_, err := RefreshProblemAbilityStatsForMaintenance(ctx, db, func(_ context.Context, _ *gorm.DB, _ uint64) error {
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("refresh err=%v want transition failure", err)
	}
	var state model.AbilityModelState
	if err := db.First(&state, 1).Error; err != nil {
		t.Fatal(err)
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if state.ActiveVersion != 4 || stored.Phase != "facts" || stored.Revision != 3 {
		t.Fatalf("atomic rollback state=%+v pending=%+v", state, stored)
	}
}

func createProblemAbilityTestProblem(t *testing.T, db *gorm.DB, platform, externalID, difficulty string) model.Problem {
	t.Helper()
	p := model.Problem{Platform: platform, ExternalID: externalID, Title: externalID, Difficulty: difficulty}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("create problem: %v", err)
	}
	return p
}

func addProblemAbilityLog(t *testing.T, db *gorm.DB, userID int64, platform, submitID, status string, problemID *uint, at time.Time) {
	t.Helper()
	l := model.SubmitLog{UserID: userID, Platform: platform, SubmitID: submitID, Status: status, ProblemID: problemID, Time: at}
	l.FillIsAC()
	if err := db.Create(&l).Error; err != nil {
		t.Fatalf("create submit %s/%s: %v", platform, submitID, err)
	}
}

func TestProblemAbilityEvidenceFiltersAndCapsSequences(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	luogu := createProblemAbilityTestProblem(t, db, "LuoGu", "P1000", "easy")
	codeforces := createProblemAbilityTestProblem(t, db, "Codeforces", "1A", "hard")

	for _, uid := range []int64{1, 2, 6} {
		if err := db.Create(&model.Platform{UserID: uid, Platform: "LuoGu", Username: fmt.Sprintf("u%d", uid), ClientSyncCompletedAt: &now}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Pending does not count. The raw duplicate is deliberately inserted after
	// dropping the current uniqueness index to cover pre-migration dirty data.
	addProblemAbilityLog(t, db, 1, "LuoGu", "pending", "PENDING", &luogu.ID, now)
	addProblemAbilityLog(t, db, 1, "LuoGu", "same", "WA", &luogu.ID, now.Add(time.Minute))
	if err := db.Exec("DROP INDEX idx_submit_plat_sid").Error; err != nil {
		t.Fatal(err)
	}
	addProblemAbilityLog(t, db, 1, "LuoGu", "same", "WA", &luogu.ID, now.Add(2*time.Minute))
	addProblemAbilityLog(t, db, 1, "LuoGu", "ac", "AC", &luogu.ID, now.Add(3*time.Minute))
	addProblemAbilityLog(t, db, 1, "LuoGu", "after-ac", "WA", &luogu.ID, now.Add(4*time.Minute))

	// A late AC still has only twenty attempts but remains a successful solve.
	for i := 1; i <= 25; i++ {
		addProblemAbilityLog(t, db, 2, "LuoGu", fmt.Sprintf("late-%02d", i), "WA", &luogu.ID, now.Add(time.Duration(i)*time.Hour))
	}
	addProblemAbilityLog(t, db, 2, "LuoGu", "late-ac", "AC", &luogu.ID, now.Add(26*time.Hour))
	// A no-AC sequence is capped as well; incomplete evidence with failures is weighted 0.6.
	for i := 1; i <= 22; i++ {
		addProblemAbilityLog(t, db, 3, "LuoGu", fmt.Sprintf("fail-%02d", i), "WA", &luogu.ID, now.Add(time.Duration(i)*time.Hour))
	}
	// Incomplete AC-only observations are not evidence at all.
	addProblemAbilityLog(t, db, 4, "LuoGu", "only-ac", "AC", &luogu.ID, now)
	// Incomplete observations with a failure are retained at confidence 0.6.
	addProblemAbilityLog(t, db, 5, "LuoGu", "incomplete-wa", "WA", &luogu.ID, now)
	addProblemAbilityLog(t, db, 5, "LuoGu", "incomplete-ac", "AC", &luogu.ID, now.Add(time.Minute))
	// A normal terminal failure is evidence; invalid users, blank IDs, and synthetic rows are not.
	addProblemAbilityLog(t, db, 6, "LuoGu", "ordinary-wa", "WA", &luogu.ID, now)
	addProblemAbilityLog(t, db, 0, "LuoGu", "invalid-user", "WA", &luogu.ID, now)
	addProblemAbilityLog(t, db, 6, "LuoGu", "", "WA", &luogu.ID, now)
	addProblemAbilityLog(t, db, 6, "LeetCode", "lc-prob-1", "AC", &luogu.ID, now)
	addProblemAbilityLog(t, db, 6, "UOJ", "uoj-ac-6-1", "AC", &luogu.ID, now)
	// Same submit ID on a different platform must remain independently usable.
	addProblemAbilityLog(t, db, 7, "Codeforces", "same", "WA", &codeforces.ID, now)
	addProblemAbilityLog(t, db, 7, "Codeforces", "cf-ac", "AC", &codeforces.ID, now.Add(time.Minute))

	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var state model.AbilityModelState
	if err := db.First(&state).Error; err != nil {
		t.Fatal(err)
	}
	var got model.ProblemAbilityStat
	if err := db.Where("model_version = ? AND problem_id = ?", state.ActiveVersion, luogu.ID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.AttemptCount-36.2) > 1e-9 || math.Abs(got.ACUserCount-2.6) > 1e-9 {
		t.Fatalf("LuoGu evidence N/A=%v/%v, want 36.2/2.6", got.AttemptCount, got.ACUserCount)
	}
	var cf model.ProblemAbilityStat
	if err := db.Where("model_version = ? AND problem_id = ?", state.ActiveVersion, codeforces.ID).First(&cf).Error; err != nil {
		t.Fatal(err)
	}
	if math.Abs(cf.AttemptCount-1.2) > 1e-9 || math.Abs(cf.ACUserCount-0.6) > 1e-9 {
		t.Fatalf("cross-platform ID evidence N/A=%v/%v, want 1.2/0.6", cf.AttemptCount, cf.ACUserCount)
	}
}

func TestRefreshProblemAbilityStatsPublishesCompleteNewSnapshot(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	p := createProblemAbilityTestProblem(t, db, "LuoGu", "P1001", "medium")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&model.Platform{UserID: 9, Platform: "LuoGu", Username: "u9", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	addProblemAbilityLog(t, db, 9, "LuoGu", "new", "AC", &p.ID, now)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 41}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemAbilityStat{ModelVersion: 41, ProblemID: p.ID, Platform: "LuoGu", Difficulty: "medium", AttemptCount: 99, ACUserCount: 99}).Error; err != nil {
		t.Fatal(err)
	}

	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	stats, err := ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil {
		t.Fatalf("read active snapshot: %v", err)
	}
	if len(stats) != 1 || stats[0].ModelVersion != 42 || stats[0].AttemptCount != 1 || stats[0].ACUserCount != 1 {
		t.Fatalf("active read exposed stale/mixed snapshot: %+v", stats)
	}
	profile := DifficultyAbilityProfile("medium")
	mu := (1 + 200*profile.Prior) / 201
	wantPosterior := (1 + 30*mu) / 31
	if math.Abs(stats[0].GroupPriorRate-mu) > 1e-9 || math.Abs(stats[0].PosteriorACRate-wantPosterior) > 1e-9 {
		t.Fatalf("posterior group/problem=%v/%v, want %v/%v", stats[0].GroupPriorRate, stats[0].PosteriorACRate, mu, wantPosterior)
	}
	wantHardness := ProblemHardness("medium", 1, 1, 1, 1)
	if math.Abs(stats[0].Hardness-wantHardness) > 1e-9 {
		t.Fatalf("hardness=%v want %v", stats[0].Hardness, wantHardness)
	}
}

func TestRefreshProblemAbilityStatsForPeriodCoalescesCronButNotAdminForce(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()

	version, refreshed, err := RefreshProblemAbilityStatsForPeriod(ctx, db, "2026-08-29")
	if err != nil || !refreshed || version != 1 {
		t.Fatalf("first scheduled refresh version=%d refreshed=%v err=%v", version, refreshed, err)
	}
	version, refreshed, err = RefreshProblemAbilityStatsForPeriod(ctx, db, "2026-08-29")
	if err != nil || refreshed || version != 1 {
		t.Fatalf("same-period scheduled refresh version=%d refreshed=%v err=%v", version, refreshed, err)
	}
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatalf("admin force refresh: %v", err)
	}
	version, refreshed, err = RefreshProblemAbilityStatsForPeriod(ctx, db, "2026-08-29")
	if err != nil || refreshed || version != 2 {
		t.Fatalf("admin force must stay independent without reopening the cron period: version=%d refreshed=%v err=%v", version, refreshed, err)
	}
	version, refreshed, err = RefreshProblemAbilityStatsForPeriod(ctx, db, "2026-08-30")
	if err != nil || !refreshed || version != 3 {
		t.Fatalf("next-period scheduled refresh version=%d refreshed=%v err=%v", version, refreshed, err)
	}
}

func TestRefreshProblemAbilityStatsForPeriodConcurrentCallersBuildOnce(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	start := make(chan struct{})
	type result struct {
		version   uint64
		refreshed bool
		err       error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			version, refreshed, err := RefreshProblemAbilityStatsForPeriod(ctx, db, "2026-08-29")
			results <- result{version: version, refreshed: refreshed, err: err}
		}()
	}
	close(start)
	builds := 0
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil || got.version != 1 {
			t.Fatalf("result=%+v", got)
		}
		if got.refreshed {
			builds++
		}
	}
	if builds != 1 {
		t.Fatalf("same-period concurrent scheduled builds=%d want=1", builds)
	}
}

func TestProblemAbilityEvidenceNormalizesPlatformAndBacklogRevokesCoverage(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := createProblemAbilityTestProblem(t, db, "LuoGu", "P1002", "easy")
	if err := db.Create(&model.Platform{UserID: 31, Platform: " Luogu ", Username: "u31", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	addProblemAbilityLog(t, db, 31, "luOgu", "ac", "AC", &p.ID, now)
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err := ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil || len(stats) != 1 || stats[0].AttemptCount != 1 || stats[0].ACUserCount != 1 {
		t.Fatalf("case-normalized complete AC evidence = %+v, err=%v", stats, err)
	}

	// A real terminal row without a problem binding revokes the completion
	// anchor even if its platform spelling differs from the bound sequence.
	addProblemAbilityLog(t, db, 31, "LUOGU", "unbound", "WA", nil, now.Add(time.Minute))
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err = ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("unbound terminal backlog must revoke AC-only coverage, got %+v", stats)
	}

	if err := db.Where("platform = ? AND submit_id = ?", "LUOGU", "unbound").Delete(&model.SubmitLog{}).Error; err != nil {
		t.Fatal(err)
	}
	zeroProblemID := uint(0)
	addProblemAbilityLog(t, db, 31, "LUOGU", "unbound-zero", "WA", &zeroProblemID, now.Add(2*time.Minute))
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err = ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("problem_id=0 backlog must remain unbound and revoke AC-only coverage, got %+v", stats)
	}

	if err := db.Where("platform = ? AND submit_id = ?", "LUOGU", "unbound-zero").Delete(&model.SubmitLog{}).Error; err != nil {
		t.Fatal(err)
	}
	foreign := createProblemAbilityTestProblem(t, db, "Codeforces", "1900A", "hard")
	addProblemAbilityLog(t, db, 31, "LUOGU", "cross-platform-binding", "WA", &foreign.ID, now.Add(3*time.Minute))
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err = ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("cross-platform dirty binding must revoke the complete-history anchor, got %+v", stats)
	}

	if err := db.Where("platform = ? AND submit_id = ?", "LUOGU", "cross-platform-binding").Delete(&model.SubmitLog{}).Error; err != nil {
		t.Fatal(err)
	}
	missingProblemID := foreign.ID + 100000
	addProblemAbilityLog(t, db, 31, "LUOGU", "missing-problem-binding", "WA", &missingProblemID, now.Add(4*time.Minute))
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err = ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("missing nonzero problem binding must revoke the complete-history anchor, got %+v", stats)
	}
}

func TestRefreshProblemAbilityStatsAggregatesNormalizedPlatformDifficultyGroup(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p1 := createProblemAbilityTestProblem(t, db, "LuoGu", "P1003", "easy")
	p2 := createProblemAbilityTestProblem(t, db, "luogu", "P1004", "简单")
	for _, uid := range []int64{41, 42} {
		if err := db.Create(&model.Platform{UserID: uid, Platform: "Luogu", Username: fmt.Sprintf("u%d", uid), ClientSyncCompletedAt: &now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	addProblemAbilityLog(t, db, 41, "LUOGU", "p1-wa", "WA", &p1.ID, now)
	addProblemAbilityLog(t, db, 41, "LUOGU", "p1-ac", "AC", &p1.ID, now.Add(time.Minute))
	addProblemAbilityLog(t, db, 42, "luogu", "p2-ac", "AC", &p2.ID, now)
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err := ActiveProblemAbilityStats(ctx, db, []uint{p1.ID, p2.ID})
	if err != nil || len(stats) != 2 {
		t.Fatalf("group stats=%+v err=%v", stats, err)
	}
	byID := map[uint]model.ProblemAbilityStat{}
	for _, stat := range stats {
		byID[stat.ProblemID] = stat
	}
	mu := (2 + 200*0.65) / (3 + 200)
	for _, id := range []uint{p1.ID, p2.ID} {
		if math.Abs(byID[id].GroupPriorRate-mu) > 1e-9 {
			t.Fatalf("problem %d group prior=%v want %v", id, byID[id].GroupPriorRate, mu)
		}
		if byID[id].Platform != "luogu" || byID[id].Difficulty != "easy" {
			t.Fatalf("problem %d group identity=%q/%q", id, byID[id].Platform, byID[id].Difficulty)
		}
	}
}

func TestRefreshProblemAbilityStatsRollbackAndEmptySnapshot(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	p := createProblemAbilityTestProblem(t, db, "LuoGu", "P1005", "medium")
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 8}).Error; err != nil {
		t.Fatal(err)
	}
	old := model.ProblemAbilityStat{ModelVersion: 8, ProblemID: p.ID, Platform: "luogu", Difficulty: "medium", AttemptCount: 7, ACUserCount: 3}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_problem_ability_stats
		BEFORE INSERT ON problem_ability_stats BEGIN SELECT RAISE(FAIL, 'forced write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&model.Platform{UserID: 51, Platform: "LuoGu", Username: "u51", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	addProblemAbilityLog(t, db, 51, "LuoGu", "ac", "AC", &p.ID, now)
	if err := RefreshProblemAbilityStats(ctx, db); err == nil {
		t.Fatal("refresh must report the forced snapshot-write failure")
	}
	var state model.AbilityModelState
	if err := db.First(&state, 1).Error; err != nil || state.ActiveVersion != 8 {
		t.Fatalf("failed build switched active state=%+v err=%v", state, err)
	}
	if err := db.Exec("DROP TRIGGER reject_problem_ability_stats").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DELETE FROM submit_logs").Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&state, 1).Error; err != nil || state.ActiveVersion != 9 {
		t.Fatalf("empty refresh state=%+v err=%v", state, err)
	}
	version, err := ActiveAbilityModelVersion(ctx, db)
	if err != nil || version != 9 {
		t.Fatalf("stable active version for empty snapshot=%d err=%v", version, err)
	}
	stats, err := ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil || len(stats) != 0 {
		t.Fatalf("empty active snapshot=%+v err=%v", stats, err)
	}
	var previous model.ProblemAbilityStat
	if err := db.Where("model_version = ? AND problem_id = ?", 8, p.ID).First(&previous).Error; err != nil {
		t.Fatalf("previous active snapshot was not retained: %v", err)
	}
}

func TestRefreshProblemAbilityStatsRejectsVersionOverflow(t *testing.T) {
	const maxPostgresBigint = uint64(math.MaxInt64)
	if _, err := nextAbilityModelVersion(maxPostgresBigint); err == nil {
		t.Fatal("version overflow was reported as a successful refresh")
	}
	if got, err := nextAbilityModelVersion(maxPostgresBigint - 1); err != nil || got != maxPostgresBigint {
		t.Fatalf("last PostgreSQL bigint version got=%d err=%v", got, err)
	}
}

func TestRefreshProblemAbilityStatsRequiresOneActiveStateUpdate(t *testing.T) {
	db := problemAbilityTestDB(t)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 3}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER ignore_ability_state_update BEFORE UPDATE OF active_version ON ability_model_state BEGIN SELECT RAISE(IGNORE); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshProblemAbilityStats(context.Background(), db); err == nil {
		t.Fatal("zero-row active state update was reported as refreshed")
	}
	var state model.AbilityModelState
	if err := db.First(&state, 1).Error; err != nil || state.ActiveVersion != 3 {
		t.Fatalf("failed state publication was not rolled back: state=%+v err=%v", state, err)
	}
}

func TestProblemAbilityEvidenceDuplicateUnboundDoesNotRevokeBoundCoverage(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := createProblemAbilityTestProblem(t, db, "LuoGu", "P1006", "easy")
	if err := db.Create(&model.Platform{UserID: 61, Platform: "LuoGu", Username: "u61", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	addProblemAbilityLog(t, db, 61, "LuoGu", "duplicate", "AC", &p.ID, now)
	foreign := createProblemAbilityTestProblem(t, db, "Codeforces", "1900B", "medium")
	if err := db.Exec("DROP INDEX idx_submit_plat_sid").Error; err != nil {
		t.Fatal(err)
	}
	// Historical dirty data can carry an unbound duplicate of the same real
	// submit. The bound row is canonical evidence and must win that conflict.
	addProblemAbilityLog(t, db, 61, "luogu", "duplicate", "WA", nil, now.Add(time.Minute))
	addProblemAbilityLog(t, db, 61, " LUOGU ", "duplicate", "WA", &foreign.ID, now.Add(2*time.Minute))
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatal(err)
	}
	stats, err := ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil || len(stats) != 1 || stats[0].AttemptCount != 1 || stats[0].ACUserCount != 1 {
		t.Fatalf("bound duplicate must preserve complete evidence, got=%+v err=%v", stats, err)
	}
}

func TestProblemAbilityEvidenceExcludesCrossPlatformDirtyBinding(t *testing.T) {
	db := problemAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := createProblemAbilityTestProblem(t, db, "LuoGu", "P1007", "hard")
	addProblemAbilityLog(t, db, 71, "Codeforces", "foreign-wa", "WA", &p.ID, now)
	addProblemAbilityLog(t, db, 71, "Codeforces", "foreign-ac", "AC", &p.ID, now.Add(time.Minute))
	if err := RefreshProblemAbilityStats(ctx, db); err != nil {
		t.Fatalf("cross-platform dirty binding must not make refresh fail: %v", err)
	}
	stats, err := ActiveProblemAbilityStats(ctx, db, []uint{p.ID})
	if err != nil || len(stats) != 0 {
		t.Fatalf("cross-platform dirty binding must be excluded, got=%+v err=%v", stats, err)
	}
}
