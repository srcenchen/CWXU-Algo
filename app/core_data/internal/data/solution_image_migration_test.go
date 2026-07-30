package data

import (
	"fmt"
	"testing"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMigrateSolutionImageURLsMarksPatchOnlyAfterSuccessfulCompletion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&schemaPatch{}, &model.ProblemUserSolution{}); err != nil {
		t.Fatal(err)
	}
	row := model.ProblemUserSolution{ProblemID: 1, UserID: 2, Title: "migration", ContentMD: "![x](https://zhiyuansofts.cn/blog/2/x.webp)"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_solution_path BEFORE UPDATE ON problem_user_solutions BEGIN SELECT RAISE(FAIL, 'solution failed'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	migrateSolutionImageURLsToPathOnly(db)
	var markerCount int64
	if err := db.Model(&schemaPatch{}).Where("key = ?", "solution_image_url_path_only_v1").Count(&markerCount).Error; err != nil {
		t.Fatal(err)
	}
	if markerCount != 0 {
		t.Fatal("failed solution migration must not write schema patch marker")
	}
	if err := db.Exec(`DROP TRIGGER fail_solution_path`).Error; err != nil {
		t.Fatal(err)
	}
	migrateSolutionImageURLsToPathOnly(db)
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.ContentMD != "![x](/blog/2/x.webp)" {
		t.Fatalf("restart did not rerun solution migration: %q", row.ContentMD)
	}
	if err := db.Model(&schemaPatch{}).Where("key = ?", "solution_image_url_path_only_v1").Count(&markerCount).Error; err != nil || markerCount != 1 {
		t.Fatalf("successful solution marker count=%d err=%v", markerCount, err)
	}
}

func TestMigrateSolutionImageURLsDoesNotOverwriteConcurrentEdit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&schemaPatch{}, &model.ProblemUserSolution{}); err != nil {
		t.Fatal(err)
	}
	row := model.ProblemUserSolution{ProblemID: 2, UserID: 3, Title: "migration", ContentMD: "![x](https://zhiyuansofts.cn/blog/3/x.webp)"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Update().Before("gorm:update").Register("test:concurrent-solution-path", func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "problem_user_solutions" {
			return
		}
		changed = true
		if err := db.Session(&gorm.Session{SkipHooks: true}).Model(&model.ProblemUserSolution{}).
			Where("id = ?", row.ID).UpdateColumn("content_md", "concurrent solution edit").Error; err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	migrateSolutionImageURLsToPathOnly(db)
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.ContentMD != "concurrent solution edit" {
		t.Fatalf("migration overwrote concurrent solution edit: %q", row.ContentMD)
	}
}

func TestMigrateSolutionImageURLsUsesBoundedKeysetBatches(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&schemaPatch{}, &model.ProblemUserSolution{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < solutionImageMigrationBatchSize+5; i++ {
		row := model.ProblemUserSolution{ProblemID: uint(i + 1), UserID: 9, Title: fmt.Sprintf("batch-%d", i), ContentMD: "![x](https://zhiyuansofts.cn/blog/9/x.webp)"}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	queries := 0
	missingLimit := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:bounded-solution-image-path", func(tx *gorm.DB) {
		if tx.Statement.Table != "problem_user_solutions" {
			return
		}
		queries++
		if _, ok := tx.Statement.Clauses["LIMIT"]; !ok {
			missingLimit = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	migrateSolutionImageURLsToPathOnly(db)
	if missingLimit {
		t.Fatal("solution migration issued an unbounded table query")
	}
	if queries < 3 {
		t.Fatalf("solution keyset queries=%d, want multiple batches plus terminating query", queries)
	}
}
