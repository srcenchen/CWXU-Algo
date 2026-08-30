package service

import (
	"fmt"
	"testing"
	"time"

	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	profiletask "cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func bindBacklogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.Problem{}, &model.UserACProblem{},
		&model.UserACProblemDay{}, &model.UserProblemStatus{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestBindSubmitsAfterSpiderOnlyRebuildsMarkedPlatform(t *testing.T) {
	db := bindBacklogTestDB(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	if err := profiletask.MarkProfileRebuildAfterBinding(rdb, 77, "LuoGu"); err != nil {
		t.Fatal(err)
	}
	if err := uc.BindSubmitsAfterSpiderForPlatform(77, "CodeForces"); err != nil {
		t.Fatal(err)
	}
	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("unrelated platform consumed rebuild marker: %+v", events)
	}
	if got := rdb.Exists(t.Context(), profiletask.ProfileRebuildAfterBindingKey(77, "LuoGu")).Val(); got != 1 {
		t.Fatalf("rebuild marker was consumed by unrelated platform: exists=%d", got)
	}
	if err := uc.BindSubmitsAfterSpiderForPlatform(77, "LuoGu"); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 1 || !events[0].Force || events[0].UserId != 77 {
		t.Fatalf("marked platform did not force rebuild: %+v", events)
	}
	if got := rdb.Exists(t.Context(), profiletask.ProfileRebuildAfterBindingKey(77, "LuoGu")).Val(); got != 0 {
		t.Fatalf("rebuild marker not cleared after successful enqueue: exists=%d", got)
	}
}

func seedUnboundCodeforcesSubmits(t *testing.T, db *gorm.DB, userID int64, count int, isAC bool) {
	t.Helper()
	rows := make([]model.SubmitLog, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, model.SubmitLog{
			Platform: "CodeForces", UserID: userID, SubmitID: fmt.Sprintf("bind-%d", i),
			Contest: "1791", Problem: "A", Status: "WA", IsAC: isAC, Time: time.Now(),
		})
	}
	if err := db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
}

func TestBindSubmitsAfterSpiderDrains501WithinOneWatermark(t *testing.T) {
	db := bindBacklogTestDB(t)
	seedUnboundCodeforcesSubmits(t, db, 71, 501, false)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}

	if err := uc.BindSubmitsAfterSpider(71); err != nil {
		t.Fatal(err)
	}

	var remaining int64
	if err := db.Model(&model.SubmitLog{}).
		Where("user_id = ? AND (problem_id IS NULL OR problem_id = 0)", 71).
		Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining unbound=%d, want 0", remaining)
	}
}

func TestBindSubmitsAfterSpiderUpdateFailureDoesNotPublishDerivedState(t *testing.T) {
	db := bindBacklogTestDB(t)
	seedUnboundCodeforcesSubmits(t, db, 72, 1, true)
	var row model.SubmitLog
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_submit_problem_bind
		BEFORE UPDATE OF problem_id ON submit_logs
		WHEN OLD.id = %d
		BEGIN
			SELECT RAISE(FAIL, 'forced bind update failure');
		END`, row.ID)).Error; err != nil {
		t.Fatal(err)
	}
	oldKey := model.ACProblemKey(row.Platform, row.ExternalID, row.Problem, nil)
	if err := db.Create(&model.UserACProblem{UserID: 72, ProblemKey: oldKey, Platform: row.Platform, FirstACAt: row.Time}).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}

	if err := uc.BindSubmitsAfterSpider(72); err == nil {
		t.Fatal("expected bind update error")
	}

	var after model.SubmitLog
	if err := db.First(&after, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ProblemID != nil && *after.ProblemID != 0 {
		t.Fatalf("submit was falsely bound: problem_id=%v", *after.ProblemID)
	}
	var statuses int64
	if err := db.Model(&model.UserProblemStatus{}).Where("user_id = ?", 72).Count(&statuses).Error; err != nil {
		t.Fatal(err)
	}
	if statuses != 0 {
		t.Fatalf("derived statuses=%d, want 0", statuses)
	}
	var ac model.UserACProblem
	if err := db.First(&ac, "user_id = ?", 72).Error; err != nil {
		t.Fatal(err)
	}
	if ac.ProblemKey != oldKey {
		t.Fatalf("failed bind promoted AC key %q -> %q", oldKey, ac.ProblemKey)
	}
	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("failed bind published profile events: %+v", events)
	}
}

func TestBindSubmitsAfterSpiderSkipBankIsNotABindFailure(t *testing.T) {
	db := bindBacklogTestDB(t)
	row := model.SubmitLog{
		Platform: "LeetCode", UserID: 73, SubmitID: "lc-ac-total-73",
		Contest: "leetcode", Problem: "lc-ac-problem-73", Status: "AC", IsAC: true, Time: time.Now(),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}

	if err := uc.BindSubmitsAfterSpider(73); err != nil {
		t.Fatalf("synthetic skip-bank row must not fail the bounded drain: %v", err)
	}
	var after model.SubmitLog
	if err := db.First(&after, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ProblemID != nil && *after.ProblemID != 0 {
		t.Fatalf("skip-bank row unexpectedly bound to %d", *after.ProblemID)
	}
}

func TestBindSubmitsAfterSpiderLeavesRowsInsertedAfterWatermarkForNextRun(t *testing.T) {
	db := bindBacklogTestDB(t)
	seedUnboundCodeforcesSubmits(t, db, 74, 1, false)
	var first model.SubmitLog
	if err := db.First(&first, "user_id = ?", 74).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf(`
		CREATE TRIGGER insert_after_bind_watermark
		AFTER UPDATE OF problem_id ON submit_logs
		WHEN OLD.id = %d
		BEGIN
			INSERT INTO submit_logs(platform,user_id,submit_id,contest,problem,status,is_ac,time)
			VALUES('CodeForces',74,'after-watermark','1791','A','WA',0,CURRENT_TIMESTAMP);
		END`, first.ID)).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}
	if err := uc.BindSubmitsAfterSpider(74); err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := db.Model(&model.SubmitLog{}).Where("user_id = ? AND (problem_id IS NULL OR problem_id = 0)", 74).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("post-watermark rows processed in same run: remaining=%d", remaining)
	}
	if err := uc.BindSubmitsAfterSpider(74); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SubmitLog{}).Where("user_id = ? AND (problem_id IS NULL OR problem_id = 0)", 74).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("post-watermark row not processed next run: remaining=%d", remaining)
	}
}

func TestBindSubmitsAfterSpiderZeroRowsIsNotSuccess(t *testing.T) {
	db := bindBacklogTestDB(t)
	seedUnboundCodeforcesSubmits(t, db, 75, 1, true)
	var row model.SubmitLog
	if err := db.First(&row, "user_id = ?", 75).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER ignore_submit_problem_bind BEFORE UPDATE OF problem_id ON submit_logs WHEN OLD.id = %d BEGIN SELECT RAISE(IGNORE); END`, row.ID)).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	if err := uc.BindSubmitsAfterSpider(75); err == nil {
		t.Fatal("zero-row conditional update was treated as success")
	}
	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("zero-row bind published profile: %+v", events)
	}
	var after model.SubmitLog
	if err := db.First(&after, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ProblemID != nil && *after.ProblemID != 0 {
		t.Fatalf("zero-row bind mutated submit: %+v", after)
	}
}

func TestBindSubmitsAfterSpiderPromotionFailureRollsBackBindingForRetry(t *testing.T) {
	db := bindBacklogTestDB(t)
	seedUnboundCodeforcesSubmits(t, db, 76, 1, true)
	var row model.SubmitLog
	if err := db.First(&row, "user_id = ?", 76).Error; err != nil {
		t.Fatal(err)
	}
	oldKey := model.ACProblemKey(row.Platform, row.ExternalID, row.Problem, nil)
	if err := db.Create(&model.UserACProblem{UserID: 76, ProblemKey: oldKey, Platform: row.Platform, FirstACAt: row.Time}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_ac_key_promotion BEFORE INSERT ON user_ac_problems WHEN NEW.problem_key LIKE 'p:%' BEGIN SELECT RAISE(FAIL, 'forced promotion failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}

	if err := uc.BindSubmitsAfterSpider(76); err == nil {
		t.Fatal("expected promotion failure")
	}
	var failed model.SubmitLog
	if err := db.First(&failed, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.ProblemID != nil && *failed.ProblemID != 0 {
		t.Fatalf("promotion failure left submit permanently bound: %+v", failed)
	}
	var ac model.UserACProblem
	if err := db.First(&ac, "user_id = ?", 76).Error; err != nil {
		t.Fatal(err)
	}
	if ac.ProblemKey != oldKey {
		t.Fatalf("promotion failure changed key %q -> %q", oldKey, ac.ProblemKey)
	}
	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("promotion failure published profile: %+v", events)
	}
	if err := db.Exec(`DROP TRIGGER fail_ac_key_promotion`).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.BindSubmitsAfterSpider(76); err != nil {
		t.Fatalf("retry after promotion repair failed: %v", err)
	}
	if err := db.First(&failed, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.ProblemID == nil || *failed.ProblemID == 0 {
		t.Fatal("retry did not bind submit")
	}
}
