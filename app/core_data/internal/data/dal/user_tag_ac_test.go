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

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func userTagAbilityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.UserTagAC{}, &model.UserACProblem{}, &model.Problem{}, &model.ProblemTag{},
		&model.SubmitLog{}, &model.Platform{}, &model.AbilityModelState{}, &model.ProblemAbilityStat{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func addUserTagProblem(t *testing.T, db *gorm.DB, platform, externalID, difficulty string, tags ...string) model.Problem {
	t.Helper()
	p := model.Problem{Platform: platform, ExternalID: externalID, Title: externalID, Difficulty: difficulty}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: tag}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func addUserTagACKey(t *testing.T, db *gorm.DB, userID int64, key, platform string, at time.Time) {
	t.Helper()
	if err := db.Create(&model.UserACProblem{UserID: userID, ProblemKey: key, Platform: platform, FirstACAt: at}).Error; err != nil {
		t.Fatal(err)
	}
}

func addUserTagSubmit(t *testing.T, db *gorm.DB, userID int64, platform, submitID, status string, problemID *uint, externalID string, at time.Time) {
	t.Helper()
	l := model.SubmitLog{UserID: userID, Platform: platform, SubmitID: submitID, Status: status, ProblemID: problemID, ExternalID: externalID, Time: at}
	l.FillIsAC()
	if err := db.Create(&l).Error; err != nil {
		t.Fatal(err)
	}
}

func setActiveAbilityVersion(t *testing.T, db *gorm.DB, version uint64) {
	t.Helper()
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: version}).Error; err != nil {
		t.Fatal(err)
	}
}

func tagAbilityRows(t *testing.T, db *gorm.DB, userID int64) map[string]model.UserTagAC {
	t.Helper()
	var rows []model.UserTagAC
	if err := db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := make(map[string]model.UserTagAC, len(rows))
	for _, row := range rows {
		out[row.Tag] = row
	}
	return out
}

func TestRebuildUserTagAbilityCanonicalKeysAndSharedQuality(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "P1000", "hard", "dp", "graph")
	setActiveAbilityVersion(t, db, 7)
	if err := db.Create(&model.ProblemAbilityStat{ModelVersion: 7, ProblemID: p.ID, Platform: "luogu", Difficulty: "hard", Hardness: 1.4}).Error; err != nil {
		t.Fatal(err)
	}
	addUserTagACKey(t, db, 1, fmt.Sprintf("p:%d", p.ID), "Luogu", now.Add(time.Hour))
	addUserTagACKey(t, db, 1, "e: LUOGU : P1000 ", " luogu ", now)
	addUserTagACKey(t, db, 1, "n:Luogu:untitled", "Luogu", now)
	addUserTagACKey(t, db, 1, "e:LeetCode:ac-1", "LeetCode", now)

	if err := RebuildUserTagACForUser(ctx, db, 1); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rows := tagAbilityRows(t, db, 1)
	if len(rows) != 2 || rows["dp"].Count != 1 || rows["graph"].Count != 1 {
		t.Fatalf("canonical candidate count=%+v", rows)
	}
	want := ProblemMasteryQuality(1.4, 0.78)
	if math.Abs(rows["dp"].Weight-want) > 1e-12 || math.Abs(rows["graph"].Weight-want) > 1e-12 {
		t.Fatalf("same problem must fan out its identical x: %+v want=%v", rows, want)
	}
}

func TestRebuildUserTagAbilityEvidenceCoverageAndSyntheticNeutral(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	luogu := addUserTagProblem(t, db, "Luogu", "P1001", "medium", "luogu")
	lc := addUserTagProblem(t, db, "LeetCode", "two-sum", "medium", "leetcode")
	uoj := addUserTagProblem(t, db, "UOJ", "42", "medium", "uoj")
	setActiveAbilityVersion(t, db, 3)
	for _, p := range []model.Problem{luogu, lc, uoj} {
		if err := db.Create(&model.ProblemAbilityStat{ModelVersion: 3, ProblemID: p.ID, Platform: normalizeAbilityPlatform(p.Platform), Difficulty: "medium", Hardness: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []model.Problem{luogu, lc, uoj} {
		addUserTagACKey(t, db, 2, fmt.Sprintf("p:%d", p.ID), p.Platform, now)
	}
	// Incomplete ordinary AC-only is neutral; an observed failure is a one-sided penalty.
	addUserTagSubmit(t, db, 2, " codeforces ", "not-this-problem", "AC", nil, "x", now)
	for i := 0; i < 4; i++ {
		addUserTagSubmit(t, db, 2, " LUOGU ", fmt.Sprintf("wa-%d", i), "WA", &luogu.ID, "P1001", now.Add(time.Duration(i)*time.Minute))
	}
	addUserTagSubmit(t, db, 2, "luogu", "ac", "AC", &luogu.ID, "P1001", now.Add(10*time.Minute))
	// Synthetic rows retain the mapped personal problem but never contribute time/process evidence.
	addUserTagSubmit(t, db, 2, "LeetCode", "lc-prob-two-sum", "AC", &lc.ID, "two-sum", time.Unix(0, 0))
	addUserTagSubmit(t, db, 2, "UOJ", "uoj-ac-2-42", "AC", &uoj.ID, "42", now.Add(-365*24*time.Hour))

	if err := RebuildUserTagACForUser(ctx, db, 2); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rows := tagAbilityRows(t, db, 2)
	if rows["luogu"].Weight >= ProblemMasteryQuality(1, 0.78) {
		t.Fatalf("incomplete observed failure must only reduce score: %+v", rows["luogu"])
	}
	neutral := ProblemMasteryQuality(1, 0.78)
	if math.Abs(rows["leetcode"].Weight-neutral) > 1e-12 || math.Abs(rows["uoj"].Weight-neutral) > 1e-12 {
		t.Fatalf("synthetic mapped AC must remain neutral: %+v neutral=%v", rows, neutral)
	}

	// A canonical LuoGu completion anchor makes the same sequence complete,
	// but an unbound terminal backlog revokes that coverage again.
	if err := db.Create(&model.Platform{UserID: 2, Platform: " LuOgU ", Username: "u2", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RebuildUserTagACForUser(ctx, db, 2); err != nil {
		t.Fatal(err)
	}
	complete := tagAbilityRows(t, db, 2)["luogu"].Weight
	if math.Abs(complete-rows["luogu"].Weight) < 1e-12 {
		t.Fatalf("complete normalized Luogu anchor did not affect process: before=%v after=%v", rows["luogu"].Weight, complete)
	}
	addUserTagSubmit(t, db, 2, "luOGu", "unbound", "WA", nil, "", now.Add(time.Hour))
	if err := RebuildUserTagACForUser(ctx, db, 2); err != nil {
		t.Fatal(err)
	}
	if revoked := tagAbilityRows(t, db, 2)["luogu"].Weight; math.Abs(revoked-rows["luogu"].Weight) > 1e-12 {
		t.Fatalf("unbound terminal backlog must revoke completeness: got=%v want=%v", revoked, rows["luogu"].Weight)
	}
}

func TestRebuildUserTagAbilityTruncatesFirstACAndUsesFallbacks(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "P1002", "hard", "cap")
	noPosterior := addUserTagProblem(t, db, "Codeforces", "1A", "easy", "static")
	setActiveAbilityVersion(t, db, 11)
	addUserTagACKey(t, db, 3, fmt.Sprintf("p:%d", p.ID), "Luogu", now)
	addUserTagACKey(t, db, 3, fmt.Sprintf("p:%d", noPosterior.ID), "Codeforces", now)
	if err := db.Create(&model.Platform{UserID: 3, Platform: "luogu", Username: "u3", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		addUserTagSubmit(t, db, 3, "LUOGU", fmt.Sprintf("%02d", i), "WA", &p.ID, "P1002", now.Add(time.Duration(i)*time.Minute))
	}
	addUserTagSubmit(t, db, 3, "LUOGU", "first-ac", "AC", &p.ID, "P1002", now.Add(25*time.Minute))
	addUserTagSubmit(t, db, 3, "LUOGU", "after-ac", "WA", &p.ID, "P1002", now.Add(26*time.Minute))
	// Dirty duplicate must not create another attempt.
	if err := db.Exec("DROP INDEX idx_submit_plat_sid").Error; err != nil {
		t.Fatal(err)
	}
	addUserTagSubmit(t, db, 3, "luogu", "00", "WA", &p.ID, "P1002", now.Add(time.Hour))

	if err := RebuildUserTagACForUser(ctx, db, 3); err != nil {
		t.Fatal(err)
	}
	rows := tagAbilityRows(t, db, 3)
	wantCapped := ProblemMasteryQuality(DifficultyAbilityProfile("hard").Quality, SolveEffort(20, 25, true))
	if math.Abs(rows["cap"].Weight-wantCapped) > 1e-12 {
		t.Fatalf("first AC sequence must cap at 20 and ignore after AC: got=%v want=%v", rows["cap"].Weight, wantCapped)
	}
	wantStatic := ProblemMasteryQuality(DifficultyAbilityProfile("easy").Quality, 0.78)
	if math.Abs(rows["static"].Weight-wantStatic) > 1e-12 {
		t.Fatalf("missing posterior must use static difficulty: got=%v want=%v", rows["static"].Weight, wantStatic)
	}
}

func TestListUserTagAbilityUsesOnlyActiveVersionAndScoresBeforeLimit(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 9)
	// Columns introduced by the scoring model make historical rows invalid.
	if err := db.Exec(`INSERT INTO user_tag_ac (user_id, tag, count, weight, score_version, model_version)
		VALUES (4, 'legacy', 99, 99, 0, 0), (4, 'stale', 99, 99, 1, 8), (4, 'best', 1, 0.98, 1, 9)`).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := db.Exec(`INSERT INTO user_tag_ac (user_id, tag, count, weight, score_version, model_version) VALUES (?, ?, ?, ?, 1, 9)`, 4, fmt.Sprintf("bulk-%02d", i), 2, 0.30).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := ListUserTagAC(ctx, db, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Tag != "best" {
		t.Fatalf("limit must happen after score sorting and ignore old rows: %+v", rows)
	}
	n, err := CountUserTagAC(ctx, db, 4)
	if err != nil || n != 21 {
		t.Fatalf("active count=%d err=%v, want 21", n, err)
	}
	ids, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == 4 {
			t.Fatalf("active radar rows must prevent empty-radar heal: %v", ids)
		}
	}
}

func TestRebuildUserTagAbilityMapsPOnlyCandidateUnboundProcess(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Codeforces", "1900A", "medium", "p-only")
	setActiveAbilityVersion(t, db, 1)
	addUserTagACKey(t, db, 31, fmt.Sprintf("p:%d", p.ID), "Codeforces", now)
	for i := 0; i < 4; i++ {
		addUserTagSubmit(t, db, 31, " codeforces ", fmt.Sprintf("wa-%d", i), "WA", nil, "1900A", now.Add(time.Duration(i)*time.Minute))
	}
	addUserTagSubmit(t, db, 31, "CODEFORCES", "ac", "AC", nil, "1900A", now.Add(8*time.Minute))

	if err := RebuildUserTagACForUser(ctx, db, 31); err != nil {
		t.Fatal(err)
	}
	got := tagAbilityRows(t, db, 31)["p-only"].Weight
	neutral := ProblemMasteryQuality(1, 0.78)
	if got >= neutral {
		t.Fatalf("p-only candidate must use uniquely mapped unbound failures: got=%v neutral=%v", got, neutral)
	}
}

func TestRebuildUserTagAbilityDoesNotGuessAmbiguousPOnlyExternalProcess(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Codeforces", "1900B", "medium", "ambiguous")
	// The database's raw unique index allows these whitespace/case variants,
	// while their canonical external identity is intentionally ambiguous.
	_ = addUserTagProblem(t, db, " codeforces ", " 1900B ", "medium", "other")
	setActiveAbilityVersion(t, db, 1)
	addUserTagACKey(t, db, 32, fmt.Sprintf("p:%d", p.ID), "Codeforces", now)
	for i := 0; i < 4; i++ {
		addUserTagSubmit(t, db, 32, "codeforces", fmt.Sprintf("wa-%d", i), "WA", nil, "1900B", now.Add(time.Duration(i)*time.Minute))
	}
	addUserTagSubmit(t, db, 32, "codeforces", "ac", "AC", nil, "1900B", now.Add(8*time.Minute))

	if err := RebuildUserTagACForUser(ctx, db, 32); err != nil {
		t.Fatal(err)
	}
	got := tagAbilityRows(t, db, 32)["ambiguous"].Weight
	want := ProblemMasteryQuality(1, 0.78)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("ambiguous external identity must not be guessed: got=%v want neutral=%v", got, want)
	}
}

func TestRebuildUserTagAbilityCanonicalDuplicateDoesNotIncreaseCompleteLuoguAttempts(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "P1900", "medium", "dedup")
	setActiveAbilityVersion(t, db, 1)
	if err := db.Create(&model.Platform{UserID: 33, Platform: " LUOGU ", Username: "u33", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	addUserTagACKey(t, db, 33, fmt.Sprintf("p:%d", p.ID), "Luogu", now)
	addUserTagSubmit(t, db, 33, "Luogu", "same", "WA", &p.ID, "P1900", now)
	if err := db.Exec("DROP INDEX idx_submit_plat_sid").Error; err != nil {
		t.Fatal(err)
	}
	addUserTagSubmit(t, db, 33, " luogu ", "same", "WA", &p.ID, "P1900", now.Add(time.Minute))
	addUserTagSubmit(t, db, 33, "LUOGU", "ac", "AC", &p.ID, "P1900", now.Add(2*time.Minute))

	if err := RebuildUserTagACForUser(ctx, db, 33); err != nil {
		t.Fatal(err)
	}
	got := tagAbilityRows(t, db, 33)["dedup"].Weight
	want := ProblemMasteryQuality(1, SolveEffort(2, 2, true))
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("canonical duplicate must not increase complete Luogu attempts: got=%v want=%v", got, want)
	}
	withoutDedupe := ProblemMasteryQuality(1, SolveEffort(3, 2, true))
	if math.Abs(withoutDedupe-want) < 1e-12 {
		t.Fatal("test fixture must distinguish a non-deduplicated attempt sequence")
	}
}

func TestRebuildUserTagAbilityEpochFirstACKeepsEarlierValidTime(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := model.Problem{ID: 44, Platform: "Codeforces", ExternalID: "1900D"}
	ext, ok := parseAbilityExternalKey("e:Codeforces:1900D")
	if !ok {
		t.Fatal("test key did not parse")
	}
	candidates, _ := resolveAbilityCandidates([]model.UserACProblem{
		{ProblemKey: "p:44", FirstACAt: now},
		{ProblemKey: "e:Codeforces:1900D", FirstACAt: time.Unix(0, 0)},
	}, map[uint]model.Problem{44: p}, []abilityExternalKey{ext})
	if got := candidates[44].firstACAt; !got.Equal(now) {
		t.Fatalf("epoch must not replace valid FirstACAt: got=%v want=%v", got, now)
	}
}

func TestRebuildUserTagAbilityUniqueEpochFirstACIsZero(t *testing.T) {
	p := model.Problem{ID: 45, Platform: "Codeforces", ExternalID: "1900F"}
	candidates := map[uint]abilityCandidate{}
	mergeAbilityCandidate(candidates, p, time.Unix(0, 0))
	if got := candidates[p.ID].firstACAt; !got.IsZero() {
		t.Fatalf("unique epoch FirstACAt must be discarded: got=%v", got)
	}
}

func TestListUserIDsWithACButEmptyTagACIncludesLegacyRow(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	if err := db.Create(&model.UserTagAC{UserID: 34, Tag: "legacy", Count: 9, Weight: 9, ScoreVersion: 0, ModelVersion: 0}).Error; err != nil {
		t.Fatal(err)
	}
	addUserTagACKey(t, db, 34, "p:1", "Codeforces", time.Now())
	ids, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == 34 {
			return
		}
	}
	t.Fatalf("legacy user_tag_ac row must not suppress empty-radar heal: %v", ids)
}

func TestRebuildUserTagAbilityNonLuoguACOnlyIsStrictlyNeutral(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Codeforces", "1900E", "medium", "ac-only")
	setActiveAbilityVersion(t, db, 1)
	addUserTagACKey(t, db, 36, fmt.Sprintf("p:%d", p.ID), "Codeforces", now)
	addUserTagSubmit(t, db, 36, "codeforces", "ac", "AC", &p.ID, "1900E", now)
	if err := RebuildUserTagACForUser(ctx, db, 36); err != nil {
		t.Fatal(err)
	}
	got := tagAbilityRows(t, db, 36)["ac-only"].Weight
	want := ProblemMasteryQuality(1, 0.78)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("ordinary non-Luogu AC-only must be neutral: got=%v want=%v", got, want)
	}
}

func TestReplaceUserTagAbilityRowsRollsBackOnLockedVersionFlip(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	old := model.UserTagAC{UserID: 37, Tag: "old", Count: 1, Weight: 0.7, ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: 1}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	// SQLite has no FOR UPDATE syntax. This deterministic same-transaction
	// flip exercises the mandatory final locked recheck without timing races.
	if err := db.Exec(`CREATE TRIGGER flip_ability_model_on_tag_delete
		AFTER DELETE ON user_tag_ac WHEN OLD.user_id = 37
		BEGIN UPDATE ability_model_state SET active_version = 2 WHERE id = 1; END`).Error; err != nil {
		t.Fatal(err)
	}
	err := replaceUserTagAbilityRows(ctx, db, 37, 1, []model.UserTagAC{{UserID: 37, Tag: "new", Count: 1, Weight: 0.8, ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: 1}})
	if !errors.Is(err, ErrUserTagAbilityModelChanged) {
		t.Fatalf("version flip must force retry/rollback, err=%v", err)
	}
	var state model.AbilityModelState
	if err := db.First(&state, 1).Error; err != nil || state.ActiveVersion != 1 {
		t.Fatalf("rollback must preserve active version: state=%+v err=%v", state, err)
	}
	rows := tagAbilityRows(t, db, 37)
	if len(rows) != 1 || rows["old"].Count != 1 || rows["new"].Count != 0 {
		t.Fatalf("version flip must rollback replacement rows: %+v", rows)
	}
}

func TestReplaceUserTagAbilityRowsUsesPostgresShareLock(t *testing.T) {
	sqlDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DryRun: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	var stateSQL string
	if err := db.Callback().Query().After("gorm:query").Register("capture_user_tag_ability_lock", func(tx *gorm.DB) {
		stateSQL += tx.Statement.SQL.String()
	}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = lockActiveUserTagAbilityModelVersion(context.Background(), db)
	if !strings.Contains(strings.ToUpper(stateSQL), "FOR SHARE") {
		t.Fatalf("active model recheck must hold PostgreSQL shared row lock, SQL=%q", stateSQL)
	}
}
