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
		&model.UserTagAC{}, &model.UserTagACSnapshot{}, &model.UserACProblem{}, &model.Problem{}, &model.ProblemTag{},
		&model.SubmitLog{}, &model.Platform{}, &model.AbilityModelState{}, &model.ProblemAbilityStat{},
		&model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatalf("install profile evidence revision triggers: %v", err)
	}
	return db
}

func ensureUserTagSnapshotTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if db.Migrator().HasTable("user_tag_ac_snapshots") {
		return
	}
	if err := db.Exec(`CREATE TABLE user_tag_ac_snapshots (
		user_id INTEGER PRIMARY KEY,
		score_version INTEGER NOT NULL,
		model_version INTEGER NOT NULL,
		evidence_dataset_revision INTEGER NOT NULL,
		evidence_user_revision INTEGER NOT NULL,
		row_count INTEGER NOT NULL,
		published_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create user_tag_ac_snapshots: %v", err)
	}
}

func stampUserTagSnapshot(t *testing.T, db *gorm.DB, userID int64, modelVersion uint64) ProfileEvidenceIdentity {
	t.Helper()
	ensureUserTagSnapshotTable(t, db)
	identity, err := ReadProfileEvidenceIdentity(context.Background(), db, userID)
	if err != nil {
		t.Fatal(err)
	}
	var rowCount int64
	if err := db.Model(&model.UserTagAC{}).Where("user_id = ? AND score_version = ? AND model_version = ?", userID, CurrentUserTagAbilityScoreVersion, modelVersion).Count(&rowCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO user_tag_ac_snapshots
		(user_id, score_version, model_version, evidence_dataset_revision, evidence_user_revision, row_count, published_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id) DO UPDATE SET
			score_version = excluded.score_version,
			model_version = excluded.model_version,
			evidence_dataset_revision = excluded.evidence_dataset_revision,
			evidence_user_revision = excluded.evidence_user_revision,
			row_count = excluded.row_count,
			published_at = excluded.published_at`,
		userID, CurrentUserTagAbilityScoreVersion, modelVersion,
		identity.DatasetRevision, identity.UserRevision, rowCount).Error; err != nil {
		t.Fatal(err)
	}
	return identity
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

func TestRebuildUserTagAbilityKeepsPreviousRowsWhenFactsIncomplete(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	p := addUserTagProblem(t, db, "Codeforces", "pending-facts", "medium")
	addUserTagACKey(t, db, 1, fmt.Sprintf("p:%d", p.ID), "Codeforces", time.Now())
	if err := db.Create(&model.UserTagAC{
		UserID: 1, Tag: "old", Count: 3, Weight: 2.4,
		ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RebuildUserTagACForUser(ctx, db, 1); !errors.Is(err, ErrUserTagAbilityIncomplete) {
		t.Fatalf("incomplete problem facts must keep the previous aggregate, err=%v", err)
	}
	rows := tagAbilityRows(t, db, 1)
	if len(rows) != 1 || rows["old"].Count != 3 {
		t.Fatalf("previous aggregate was deleted during incomplete rebuild: %+v", rows)
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
	if err := db.Exec("DROP INDEX idx_submit_plat_sid_user").Error; err != nil {
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
	stampUserTagSnapshot(t, db, 4, 9)
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

func TestListUserTagAbilityRejectsRowsFromOldEvidence(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 9)
	ensureUserTagSnapshotTable(t, db)
	identity, err := ReadProfileEvidenceIdentity(ctx, db, 40)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO user_tag_ac (user_id, tag, count, weight, score_version, model_version)
		VALUES (?, 'current', 1, 0.8, ?, 9),
		       (?, 'old-dataset', 9, 8, ?, 9),
		       (?, 'old-user', 9, 8, ?, 9)`,
		40, CurrentUserTagAbilityScoreVersion,
		401, CurrentUserTagAbilityScoreVersion,
		402, CurrentUserTagAbilityScoreVersion,
	).Error; err != nil {
		t.Fatal(err)
	}
	stampUserTagSnapshot(t, db, 40, 9)
	if err := db.Exec(`INSERT INTO user_tag_ac_snapshots
		(user_id, score_version, model_version, evidence_dataset_revision, evidence_user_revision, row_count, published_at)
		VALUES (?, ?, 9, ?, ?, 1, CURRENT_TIMESTAMP), (?, ?, 9, ?, ?, 1, CURRENT_TIMESTAMP)`,
		401, CurrentUserTagAbilityScoreVersion, identity.DatasetRevision+1, identity.UserRevision,
		402, CurrentUserTagAbilityScoreVersion, identity.DatasetRevision, identity.UserRevision+1,
	).Error; err != nil {
		t.Fatal(err)
	}

	current, err := ListUserTagAC(ctx, db, 40, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Tag != "current" {
		t.Fatalf("current evidence rows=%+v", current)
	}
	for _, userID := range []int64{401, 402} {
		rows, err := ListStaleUserTagAC(ctx, db, userID, 20)
		if err != nil {
			t.Fatal(err)
		}
		if rows.ModelVersion != 0 || len(rows.Rows) != 0 {
			t.Fatalf("old evidence user=%d remained readable: %+v", userID, rows)
		}
	}
}

func TestListStaleUserTagACSelectsLatestReadableVersionAndSortsBeforeLimit(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 9)
	if err := db.Exec(`INSERT INTO user_tag_ac (user_id, tag, count, weight, score_version, model_version)
		VALUES
			(41, 'legacy', 99, 99, 0, 8),
			(41, 'future', 99, 99, 1, 10),
			(41, 'older', 99, 99, 1, 7),
			(41, 'stale-best', 1, 0.98, 1, 8),
			(41, 'stale-bulk', 2, 0.30, 1, 8)`).Error; err != nil {
		t.Fatal(err)
	}
	stampUserTagSnapshot(t, db, 41, 8)

	snapshot, err := ListStaleUserTagAC(ctx, db, 41, 1)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ModelVersion != 8 {
		t.Fatalf("model version=%d, want latest readable stale version 8", snapshot.ModelVersion)
	}
	if len(snapshot.Rows) != 1 || snapshot.Rows[0].Tag != "stale-best" {
		t.Fatalf("rows=%+v, want score-sorted row from selected version only", snapshot.Rows)
	}

	empty, err := ListStaleUserTagAC(ctx, db, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ModelVersion != 0 || empty.Rows != nil {
		t.Fatalf("empty snapshot=%+v, want explicit no-version nil rows", empty)
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
	if err := db.Exec("DROP INDEX idx_submit_plat_sid_user").Error; err != nil {
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

func TestListUsersACProblemFiltersExternalKeysInSQL(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, " LuOgU ", " P1000 ", "medium", "greedy")
	addUserTagACKey(t, db, 11, fmt.Sprintf("p:%d", p.ID), "Luogu", now)
	addUserTagACKey(t, db, 12, "e:LuOgU:P1000", "LuOgU", now)
	for i := 0; i < 256; i++ {
		addUserTagACKey(t, db, int64(1000+i), fmt.Sprintf("e:LuOgU:noise-%d", i), "LuOgU", now)
		addUserTagACKey(t, db, int64(2000+i), fmt.Sprintf("e:AtCoder:noise-%d", i), "AtCoder", now)
	}

	loaded, querySQL := 0, ""
	if err := db.Callback().Query().After("gorm:query").Register("capture_list_users_ac_problem_rows", func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]model.UserACProblem); ok {
			loaded += len(*rows)
			querySQL = tx.Statement.SQL.String()
		}
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := ListUsersACProblem(ctx, db, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || !containsUserID(ids, 11) || !containsUserID(ids, 12) {
		t.Fatalf("canonical p:/e: users=%v", ids)
	}
	if loaded != 2 {
		t.Fatalf("unrelated e: rows entered the query result: loaded=%d", loaded)
	}
	if !strings.Contains(querySQL, "LOWER(TRIM(platform))") || !strings.Contains(querySQL, "SUBSTR(problem_key, 1, 2) = 'e:'") {
		t.Fatalf("external-key lookup must exactly imply the SQLite partial identity index: %s", querySQL)
	}
	if strings.Contains(querySQL, "problem_key LIKE ?") {
		t.Fatalf("external-key prefix must be a SQL literal so a partial index is usable: %s", querySQL)
	}
}

func TestAbilityExternalKeyPostgresPredicateHasSafeMalformedKeyLength(t *testing.T) {
	sqlDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}

	predicate := abilityExternalKeySQL(db)
	embeddedPlatform := "BTRIM(SUBSTRING(problem_key FROM 3 FOR GREATEST(POSITION(':' IN SUBSTRING(problem_key FROM 3)) - 1, 0)))"
	externalID := "BTRIM(SUBSTRING(problem_key FROM 3 + POSITION(':' IN SUBSTRING(problem_key FROM 3))))"
	want := "LOWER(BTRIM(platform)) = ? AND LEFT(problem_key, 2) = 'e:' AND " +
		"POSITION(':' IN SUBSTRING(problem_key FROM 3)) > 0 AND " +
		embeddedPlatform + " <> '' AND " + externalID + " <> '' AND " +
		"NOT (LOWER(" + embeddedPlatform + ") = 'leetcode' AND LEFT(LOWER(" + externalID + "), 3) = 'ac-') AND " +
		"LOWER(" + embeddedPlatform + ") = ? AND " + externalID + " = ?"
	if predicate != want {
		t.Fatalf("PostgreSQL external identity predicate=%q want=%q", predicate, want)
	}
	if strings.Contains(predicate, "FOR POSITION(':' IN SUBSTRING(problem_key FROM 3)) - 1") {
		t.Fatalf("malformed e:bad can reach a negative PostgreSQL substring length: %s", predicate)
	}
	if !strings.Contains(predicate, "FOR GREATEST(POSITION(':' IN SUBSTRING(problem_key FROM 3)) - 1, 0)") {
		t.Fatalf("PostgreSQL parser length must be safe independently of AND evaluation order: %s", predicate)
	}
}

func TestListUsersACProblemSQLiteFiltersSyntheticLeetCodeIdentityInSQL(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "LeetCode", "ac-1", "medium", "synthetic")
	addUserTagACKey(t, db, 98, "e:LeetCode:ac-1", "LeetCode", now)

	loaded, querySQL := 0, ""
	if err := db.Callback().Query().After("gorm:query").Register("capture_synthetic_external_identity_rows", func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]model.UserACProblem); ok {
			loaded += len(*rows)
			querySQL = tx.Statement.SQL.String()
		}
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := ListUsersACProblem(ctx, db, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 || loaded != 0 {
		t.Fatalf("synthetic LeetCode identity must be rejected in SQL: ids=%v loaded=%d", ids, loaded)
	}
	if !strings.Contains(querySQL, "SUBSTR(LOWER(") || !strings.Contains(querySQL, "= 'ac-'") {
		t.Fatalf("SQLite query must literally imply the synthetic-key partial predicate: %s", querySQL)
	}
}

func TestListUsersACProblemSQLiteRejectsMissingDelimiterAndPreservesExtraColons(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "contest:P1000:hard", "medium", "parser")
	addUserTagACKey(t, db, 91, fmt.Sprintf("p:%d", p.ID), "Luogu", now)
	addUserTagACKey(t, db, 92, "e:LUOGU:contest:P1000:hard", "Luogu", now)
	addUserTagACKey(t, db, 93, "e:bad", "Luogu", now)
	addUserTagACKey(t, db, 94, "e:Luogu:contest:P1000", "Luogu", now)

	ids, err := ListUsersACProblem(ctx, db, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || !containsUserID(ids, 91) || !containsUserID(ids, 92) {
		t.Fatalf("SQLite external-key parser users=%v, want canonical p: and full extra-colon e: keys", ids)
	}
}

func TestListUsersACProblemSkipsEmptyExternalIdentity(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "", "", "medium", "untitled")
	addUserTagACKey(t, db, 21, fmt.Sprintf("p:%d", p.ID), "", now)
	addUserTagACKey(t, db, 22, "e::", "", now)

	loaded := 0
	if err := db.Callback().Query().After("gorm:query").Register("capture_empty_external_lookup_rows", func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]model.UserACProblem); ok {
			loaded += len(*rows)
		}
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := ListUsersACProblem(ctx, db, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 21 || loaded != 1 {
		t.Fatalf("empty external identity must only use p: key: ids=%v loaded=%d", ids, loaded)
	}
}

func TestLoadAbilityProblemsFiltersByCanonicalPlatformAndExternalID(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	want := addUserTagProblem(t, db, " CodeForces ", " 1900A ", "medium", "target")
	for i := 0; i < 256; i++ {
		addUserTagProblem(t, db, fmt.Sprintf("OtherPlatform%d", i), "1900A", "medium", "noise")
	}

	loaded := 0
	if err := db.Callback().Query().After("gorm:query").Register("capture_ability_problem_lookup_rows", func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]model.Problem); ok {
			loaded += len(*rows)
		}
	}); err != nil {
		t.Fatal(err)
	}
	problems, err := loadAbilityProblems(ctx, db, nil, []abilityExternalKey{{platform: "codeforces", external: "1900A"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || problems[want.ID].ID != want.ID {
		t.Fatalf("canonical external lookup=%+v want=%d", problems, want.ID)
	}
	if loaded != 1 {
		t.Fatalf("cross-platform external-id rows entered the query result: loaded=%d", loaded)
	}
}

func TestLoadAbilityProblemsBatchesMoreThanTwoHundredKeysAndIDs(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	n := abilityLookupBatchSize + 3
	ids := make([]uint, 0, n)
	keys := make([]abilityExternalKey, 0, n)
	for i := 0; i < n; i++ {
		external := fmt.Sprintf("batch:%03d", i)
		p := addUserTagProblem(t, db, "Codeforces", external, "medium")
		ids = append(ids, p.ID)
		keys = append(keys, abilityExternalKey{platform: "codeforces", external: external})
	}
	queries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("capture_batched_ability_problem_queries", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]model.Problem); ok {
			queries++
		}
	}); err != nil {
		t.Fatal(err)
	}

	byExternal, err := loadAbilityProblems(ctx, db, nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(byExternal) != n || queries != 2 {
		t.Fatalf("external batches: rows=%d queries=%d want=%d/2", len(byExternal), queries, n)
	}
	queries = 0
	byID, err := loadAbilityProblems(ctx, db, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(byID) != n || queries != 2 {
		t.Fatalf("ID batches: rows=%d queries=%d want=%d/2", len(byID), queries, n)
	}
}

func TestLoadAbilityFactsBatchesTagsAndStatsBeyondTwoHundredCandidates(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	n := abilityLookupBatchSize + 3
	ids := make([]uint, 0, n)
	for i := 0; i < n; i++ {
		p := addUserTagProblem(t, db, "AtCoder", fmt.Sprintf("ABC%03d_A", i), "easy", fmt.Sprintf("tag-%03d", i))
		ids = append(ids, p.ID)
		if err := db.Create(&model.ProblemAbilityStat{
			ModelVersion: 9, ProblemID: p.ID, Platform: "atcoder", Difficulty: "easy", Hardness: float64(i+1) / 100,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	tagQueries, statQueries := 0, 0
	if err := db.Callback().Query().Before("gorm:query").Register("capture_batched_ability_fact_queries", func(tx *gorm.DB) {
		switch tx.Statement.Dest.(type) {
		case *[]model.ProblemTag:
			tagQueries++
		case *[]model.ProblemAbilityStat:
			statQueries++
		}
	}); err != nil {
		t.Fatal(err)
	}

	tags, hardness, err := loadAbilityFacts(ctx, db, ids, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != n || len(hardness) != n {
		t.Fatalf("fact aggregation incomplete: tags=%d hardness=%d want=%d", len(tags), len(hardness), n)
	}
	if tagQueries != 2 || statQueries != 2 {
		t.Fatalf("fact queries must be bounded batches: tags=%d stats=%d want=2/2", tagQueries, statQueries)
	}
}

func TestLoadAbilityEvidenceFiltersUnrelatedLogsButKeepsLuoguBacklog(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "P1000", "medium", "target")
	if err := db.Create(&model.Platform{UserID: 88, Platform: " LUOGU ", Username: "u88", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	for _, log := range []model.SubmitLog{
		{UserID: 88, Platform: "Luogu", SubmitID: "target-wa", Status: "WA", ProblemID: &p.ID, ExternalID: "P1000", Time: now},
		{UserID: 88, Platform: "Luogu", SubmitID: "target-ac", Status: "AC", ProblemID: &p.ID, ExternalID: "P1000", Time: now.Add(time.Minute)},
		{UserID: 88, Platform: "Luogu", SubmitID: "target-unbound", Status: "WA", ExternalID: "P1000", Time: now.Add(2 * time.Minute)},
		{UserID: 88, Platform: "Luogu", SubmitID: "luogu-backlog", Status: "WA", ExternalID: "P9999", Time: now.Add(3 * time.Minute)},
	} {
		log.FillIsAC()
		if err := db.Create(&log).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 256; i++ {
		log := model.SubmitLog{UserID: 88, Platform: "Codeforces", SubmitID: fmt.Sprintf("noise-%d", i), Status: "WA", ExternalID: fmt.Sprintf("%dA", i), Time: now.Add(time.Duration(i+4) * time.Minute)}
		log.FillIsAC()
		if err := db.Create(&log).Error; err != nil {
			t.Fatal(err)
		}
	}

	loaded := 0
	if err := db.Callback().Query().After("gorm:query").Register("capture_ability_evidence_rows", func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]model.SubmitLog); ok {
			loaded += len(*rows)
		}
	}); err != nil {
		t.Fatal(err)
	}
	key := abilityExternalKey{platform: "luogu", external: "P1000"}
	byProblem, completed, backlog, err := loadAbilityEvidence(ctx, db, 88,
		map[uint]abilityCandidate{p.ID: {problem: p, platform: "luogu"}},
		map[abilityExternalKey][]model.Problem{key: {p}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed["luogu"] || !backlog["luogu"] {
		t.Fatalf("Luogu completion/backlog=%v/%v", completed, backlog)
	}
	if got := len(byProblem[p.ID]); got != 3 {
		t.Fatalf("target evidence count=%d want=3", got)
	}
	if loaded > 5 {
		t.Fatalf("unrelated submissions entered evidence queries: loaded=%d", loaded)
	}
}

func TestLoadAbilityEvidenceIgnoresNonterminalLuoguBacklog(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "P1001", "medium", "target")
	if err := db.Create(&model.Platform{UserID: 89, Platform: "luogu", Username: "u89", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	for _, log := range []model.SubmitLog{
		{UserID: 89, Platform: "Luogu", SubmitID: "target", Status: "AC", ProblemID: &p.ID, ExternalID: "P1001", Time: now},
		{UserID: 89, Platform: "Luogu", SubmitID: "pending", Status: "TESTING", ExternalID: "P9998", Time: now},
		{UserID: 89, Platform: "Luogu", SubmitID: " ", Status: "WA", ExternalID: "P9999", Time: now},
	} {
		log.FillIsAC()
		if err := db.Create(&log).Error; err != nil {
			t.Fatal(err)
		}
	}
	key := abilityExternalKey{platform: "luogu", external: "P1001"}
	_, completed, backlog, err := loadAbilityEvidence(ctx, db, 89,
		map[uint]abilityCandidate{p.ID: {problem: p, platform: "luogu"}},
		map[abilityExternalKey][]model.Problem{key: {p}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed["luogu"] || backlog["luogu"] {
		t.Fatalf("nonterminal/empty Luogu evidence must not revoke completion: completed=%v backlog=%v", completed, backlog)
	}
}

func TestLoadAbilityEvidenceInvalidLuoguBindingRevokesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		problemID func(model.Problem) uint
	}{
		{name: "cross-platform", problemID: func(foreign model.Problem) uint { return foreign.ID }},
		{name: "missing-problem", problemID: func(foreign model.Problem) uint { return foreign.ID + 100000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := userTagAbilityTestDB(t)
			ctx := context.Background()
			now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
			target := addUserTagProblem(t, db, "Luogu", "P2000", "medium", "target")
			foreign := addUserTagProblem(t, db, "Codeforces", "2000A", "medium", "foreign")
			if err := db.Create(&model.Platform{UserID: 95, Platform: "Luogu", Username: "u95", ClientSyncCompletedAt: &now}).Error; err != nil {
				t.Fatal(err)
			}
			addUserTagSubmit(t, db, 95, "Luogu", "target-ac", "AC", &target.ID, "P2000", now)
			invalidID := tc.problemID(foreign)
			addUserTagSubmit(t, db, 95, "LUOGU", "invalid-binding", "WA", &invalidID, "P9999", now.Add(time.Minute))
			key := abilityExternalKey{platform: "luogu", external: "P2000"}
			_, completed, backlog, err := loadAbilityEvidence(ctx, db, 95,
				map[uint]abilityCandidate{target.ID: {problem: target, platform: "luogu"}},
				map[abilityExternalKey][]model.Problem{key: {target}},
			)
			if err != nil {
				t.Fatal(err)
			}
			if !completed["luogu"] || !backlog["luogu"] {
				t.Fatalf("invalid binding must revoke completion: completed=%v backlog=%v", completed, backlog)
			}
		})
	}
}

func TestLoadAbilityEvidenceCanonicalBoundDuplicateDoesNotCreateBacklog(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	target := addUserTagProblem(t, db, "Luogu", "P2001", "medium", "target")
	foreign := addUserTagProblem(t, db, "Codeforces", "2001A", "medium", "foreign")
	if err := db.Create(&model.Platform{UserID: 96, Platform: "Luogu", Username: "u96", ClientSyncCompletedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	addUserTagSubmit(t, db, 96, "Luogu", "duplicate", "AC", &target.ID, "P2001", now)
	if err := db.Exec("DROP INDEX idx_submit_plat_sid_user").Error; err != nil {
		t.Fatal(err)
	}
	addUserTagSubmit(t, db, 96, " LUOGU ", "duplicate", "WA", &foreign.ID, "P9999", now.Add(time.Minute))
	key := abilityExternalKey{platform: "luogu", external: "P2001"}
	byProblem, completed, backlog, err := loadAbilityEvidence(ctx, db, 96,
		map[uint]abilityCandidate{target.ID: {problem: target, platform: "luogu"}},
		map[abilityExternalKey][]model.Problem{key: {target}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !completed["luogu"] || backlog["luogu"] || len(byProblem[target.ID]) != 1 {
		t.Fatalf("canonical bound duplicate must win: evidence=%v completed=%v backlog=%v", byProblem, completed, backlog)
	}
}

func TestLoadAbilityEvidenceUsesTerminalStatusAsACTruth(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Luogu", "P2002", "medium", "target")
	stale := model.SubmitLog{
		UserID: 97, Platform: "Luogu", SubmitID: "corrected", Status: "WA",
		IsAC: true, ProblemID: &p.ID, ExternalID: "P2002", Time: now,
	}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	key := abilityExternalKey{platform: "luogu", external: "P2002"}
	byProblem, _, _, err := loadAbilityEvidence(ctx, db, 97,
		map[uint]abilityCandidate{p.ID: {problem: p, platform: "luogu"}},
		map[abilityExternalKey][]model.Problem{key: {p}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(byProblem[p.ID]) != 1 || byProblem[p.ID][0].isAC {
		t.Fatalf("corrected terminal status must override stale is_ac: %+v", byProblem[p.ID])
	}
}

func TestAbilityLookupIndexesAreNotOwnedByAutoMigrate(t *testing.T) {
	db := userTagAbilityTestDB(t)
	for _, index := range []struct {
		model any
		name  string
	}{
		{&model.UserACProblem{}, "idx_uac_problem_key"},
		{&model.UserACProblem{}, "idx_uac_ability_platform_key"},
		{&model.Problem{}, "idx_problem_ability_external_lookup"},
		{&model.SubmitLog{}, "idx_submit_user_problem"},
		{&model.SubmitLog{}, "idx_submit_user_ability_external"},
	} {
		if db.Migrator().HasIndex(index.model, index.name) {
			t.Fatalf("optional ability lookup index %s must not be created by blocking AutoMigrate", index.name)
		}
	}
}

func containsUserID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
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

func TestListUserIDsWithACButEmptyTagACDistinguishesPublishedEmptyFromStale(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	now := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Codeforces", "empty-radar", "medium")
	tagged := addUserTagProblem(t, db, "Codeforces", "bad-empty-radar", "medium", "graph")
	whitespaceTagged := addUserTagProblem(t, db, "Codeforces", "whitespace-empty-radar", "medium", " \t ")
	whitespaceExternal := addUserTagProblem(t, db, "Codeforces", "whitespace-external-empty-radar", "medium", " \t ")
	for _, userID := range []int64{35, 350} {
		addUserTagACKey(t, db, userID, fmt.Sprintf("p:%d", p.ID), "Codeforces", now)
		if err := RebuildUserTagACForUser(ctx, db, userID); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a previously published current empty snapshot whose authoritative
	// AC candidate now has a normalized tag. This is not a legitimate empty radar.
	addUserTagACKey(t, db, 36, fmt.Sprintf("p:%d", tagged.ID), "Codeforces", now)
	stampUserTagSnapshot(t, db, 36, 1)
	addUserTagACKey(t, db, 37, fmt.Sprintf("p:%d", whitespaceTagged.ID), "Codeforces", now)
	stampUserTagSnapshot(t, db, 37, 1)
	addUserTagACKey(t, db, 38, "e:Codeforces:"+whitespaceExternal.ExternalID, "Codeforces", now)
	stampUserTagSnapshot(t, db, 38, 1)
	// Only user 350's published-empty snapshot is invalidated afterwards.
	if err := db.Create(&model.SubmitLog{
		UserID: 350, Platform: "Codeforces", SubmitID: "late", Status: "WA", Time: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	ids, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 100)
	if err != nil {
		t.Fatal(err)
	}
	if containsUserID(ids, 35) {
		t.Fatalf("current published-empty snapshot was repeatedly selected for heal: %v", ids)
	}
	if !containsUserID(ids, 36) {
		t.Fatalf("current empty snapshot hid an authoritative tagged AC candidate: %v", ids)
	}
	if containsUserID(ids, 37) {
		t.Fatalf("whitespace-only problem tag made a legitimate empty snapshot unhealthy: %v", ids)
	}
	if containsUserID(ids, 38) {
		t.Fatalf("whitespace-only external problem tag made a legitimate empty snapshot unhealthy: %v", ids)
	}
	if !containsUserID(ids, 350) {
		t.Fatalf("stale published-empty snapshot was not selected for heal: %v", ids)
	}
}

func TestListUserIDsWithACButEmptyTagACRotatesBoundedEmptyWindow(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	now := time.Date(2026, 8, 29, 16, 30, 0, 0, time.UTC)
	untagged := addUserTagProblem(t, db, "Codeforces", "rotating-legitimate-empty", "medium")
	tagged := addUserTagProblem(t, db, "Codeforces", "rotating-invalid-empty", "medium", "graph")
	for userID := int64(600); userID < 604; userID++ {
		addUserTagACKey(t, db, userID, fmt.Sprintf("p:%d", untagged.ID), "Codeforces", now)
		stampUserTagSnapshot(t, db, userID, 1)
	}
	addUserTagACKey(t, db, 999, fmt.Sprintf("p:%d", tagged.ID), "Codeforces", now)
	stampUserTagSnapshot(t, db, 999, 1)

	first, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 4)
	if err != nil {
		t.Fatal(err)
	}
	if containsUserID(first, 999) {
		t.Fatalf("first bounded window unexpectedly reached high user: %v", first)
	}
	second, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !containsUserID(second, 999) {
		t.Fatalf("bounded window did not rotate to high invalid empty: first=%v second=%v", first, second)
	}
}

func TestListUserIDsWithACButEmptyTagACReservesBudgetForInvalidEmptySnapshots(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	now := time.Date(2026, 8, 29, 15, 0, 0, 0, time.UTC)
	tagged := addUserTagProblem(t, db, "Codeforces", "budgeted-invalid-empty", "medium", "graph")
	for _, userID := range []int64{10, 11, 12} {
		addUserTagACKey(t, db, userID, "n:Codeforces:unmapped", "Codeforces", now)
	}
	addUserTagACKey(t, db, 99, fmt.Sprintf("p:%d", tagged.ID), "Codeforces", now)
	stampUserTagSnapshot(t, db, 99, 1)

	ids, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) > 2 {
		t.Fatalf("heal returned %d users above limit: %v", len(ids), ids)
	}
	if !containsUserID(ids, 99) {
		t.Fatalf("missing snapshots consumed the whole heal budget: %v", ids)
	}
}

func TestListUserIDsWithACButEmptyTagACBoundsLegitimateEmptyValidationWork(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	now := time.Date(2026, 8, 29, 16, 0, 0, 0, time.UTC)
	untagged := addUserTagProblem(t, db, "Codeforces", "bounded-legitimate-empty", "medium")
	for userID := int64(500); userID < 512; userID++ {
		addUserTagACKey(t, db, userID, fmt.Sprintf("p:%d", untagged.ID), "Codeforces", now)
		stampUserTagSnapshot(t, db, userID, 1)
	}
	var sourceReads int
	if err := db.Callback().Query().Before("gorm:query").Register("count_bounded_empty_source_reads", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_ac_problems" {
			sourceReads++
		}
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := ListUserIDsWithACButEmptyTagAC(ctx, db, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("legitimate empty snapshots entered heal: %v", ids)
	}
	if sourceReads > 4 {
		t.Fatalf("bounded heal performed %d per-user source reads for limit=4", sourceReads)
	}
}

func TestListUserIDsWithACButEmptyTagACCapsValidationAtLookupBatchSize(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	setActiveAbilityVersion(t, db, 1)
	now := time.Date(2026, 8, 29, 16, 15, 0, 0, time.UTC)
	untagged := addUserTagProblem(t, db, "Codeforces", "hard-capped-legitimate-empty", "medium")
	for offset := 0; offset < abilityLookupBatchSize+5; offset++ {
		userID := int64(700 + offset)
		addUserTagACKey(t, db, userID, fmt.Sprintf("p:%d", untagged.ID), "Codeforces", now)
		stampUserTagSnapshot(t, db, userID, 1)
	}
	var sourceReads int
	if err := db.Callback().Query().Before("gorm:query").Register("count_hard_capped_empty_source_reads", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_ac_problems" {
			sourceReads++
		}
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := ListUserIDsWithACButEmptyTagAC(ctx, db, abilityLookupBatchSize+50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("legitimate empty snapshots entered heal: %v", ids)
	}
	if sourceReads > abilityLookupBatchSize {
		t.Fatalf("heal performed %d source reads above hard cap %d", sourceReads, abilityLookupBatchSize)
	}
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
	identity, err := EnsureProfileCacheIdentityForBuild(ctx, db, 37)
	if err != nil {
		t.Fatal(err)
	}
	err = replaceUserTagAbilityRows(ctx, db, 37, identity, []model.UserTagAC{{UserID: 37, Tag: "new", Count: 1, Weight: 0.8, ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: 1}})
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

func TestRebuildUserTagAbilityRollsBackWhenEvidenceChangesBeforePublish(t *testing.T) {
	db := userTagAbilityTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	p := addUserTagProblem(t, db, "Codeforces", "2000A", "medium", "new")
	setActiveAbilityVersion(t, db, 1)
	addUserTagACKey(t, db, 38, fmt.Sprintf("p:%d", p.ID), "Codeforces", now)
	ensureUserTagSnapshotTable(t, db)
	if err := db.Create(&model.UserTagAC{
		UserID: 38, Tag: "old", Count: 1, Weight: 0.7,
		ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	stampUserTagSnapshot(t, db, 38, 1)
	var changed bool
	if err := db.Callback().Query().After("gorm:query").Register("test:bump_evidence_before_tag_publish", func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "problem_tags" {
			return
		}
		changed = true
		if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
			Model(&model.UserProfileEvidenceVersion{}).Where("user_id = ?", 38).
			Update("revision", gorm.Expr("revision + 1")).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	err := RebuildUserTagACForUser(ctx, db, 38)
	if err == nil {
		t.Fatal("evidence change must reject the rebuilt tag snapshot")
	}
	rows := tagAbilityRows(t, db, 38)
	if len(rows) != 1 || rows["old"].Count != 1 {
		t.Fatalf("rejected evidence snapshot changed published rows: %+v", rows)
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
