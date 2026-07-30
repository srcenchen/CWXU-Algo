package dal

import (
	"context"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupContestListDB(t *testing.T) *SpiderDal {
	t.Helper()
	dsn := "file:contest_list_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ContestLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &SpiderDal{db: db}
}

func TestGetContestListScoped_DedupAndOrder(t *testing.T) {
	d := setupContestListDB(t)
	t1 := time.Unix(1700000000, 0)
	t2 := time.Unix(1700001000, 0)
	t3 := time.Unix(1700002000, 0)
	seed := []model.ContestLog{
		// 同一场多人：应取 time 最新、同 time 取 id 较大
		{Platform: "NowCoder", UserID: 1, ContestId: "100", ContestName: "A-old", Time: t1, Rank: 10, AcCount: 1},
		{Platform: "NowCoder", UserID: 2, ContestId: "100", ContestName: "A-new", Time: t2, Rank: 5, AcCount: 3},
		{Platform: "CodeForces", UserID: 1, ContestId: "200", ContestName: "B", Time: t3, Rank: 1, AcCount: 4},
		{Platform: "NowCoder", UserID: 3, ContestId: "101", ContestName: "C", Time: t1, Rank: 2, AcCount: 2},
		// 其它组织成员外的记录（member 过滤时应忽略）
		{Platform: "AtCoder", UserID: 99, ContestId: "abc", ContestName: "out", Time: t3.Add(time.Hour), Rank: 1, AcCount: 1},
	}
	if err := d.db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	logs, total, err := d.GetContestListScoped(context.Background(), ContestListQuery{
		UserId:    -1,
		Offset:    0,
		Limit:     10,
		MemberIDs: []int64{1, 2, 3},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Fatalf("total=%d want 3", total)
	}
	if len(logs) != 3 {
		t.Fatalf("len=%d want 3", len(logs))
	}
	// 按 time DESC：B(t3) → A(t2) → C(t1)
	if logs[0].ContestId != "200" || logs[0].Platform != "CodeForces" {
		t.Fatalf("first=%s/%s want CodeForces/200", logs[0].Platform, logs[0].ContestId)
	}
	if logs[1].ContestId != "100" || logs[1].ContestName != "A-new" {
		t.Fatalf("second name=%q want A-new (latest row for contest 100)", logs[1].ContestName)
	}
	if logs[2].ContestId != "101" {
		t.Fatalf("third=%s want 101", logs[2].ContestId)
	}
}

func TestGetContestListScoped_PaginationAndKeyword(t *testing.T) {
	d := setupContestListDB(t)
	base := time.Unix(1700000000, 0)
	for i := 0; i < 15; i++ {
		cl := model.ContestLog{
			Platform:    "NowCoder",
			UserID:      1,
			ContestId:   "c" + string(rune('a'+i%10)) + string(rune('0'+i/10)),
			ContestName: "Round " + string(rune('A'+i)),
			Time:        base.Add(time.Duration(i) * time.Hour),
		}
		// 保证 contest_id 唯一可读
		cl.ContestId = "cid-" + string(rune('a'+i))
		if err := d.db.Create(&cl).Error; err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	page1, total, err := d.GetContestListScoped(context.Background(), ContestListQuery{
		UserId: 1, Offset: 0, Limit: 5,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if total != 15 {
		t.Fatalf("total=%d want 15", total)
	}
	if len(page1) != 5 {
		t.Fatalf("page1 len=%d", len(page1))
	}
	page2, _, err := d.GetContestListScoped(context.Background(), ContestListQuery{
		UserId: 1, Offset: 5, Limit: 5,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 5 {
		t.Fatalf("page2 len=%d", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("page1 and page2 should not overlap on first item")
	}

	kw, kwTotal, err := d.GetContestListScoped(context.Background(), ContestListQuery{
		UserId: 1, Offset: 0, Limit: 20, Keyword: "cid-a",
	})
	if err != nil {
		t.Fatalf("keyword: %v", err)
	}
	if kwTotal < 1 || len(kw) < 1 {
		t.Fatalf("keyword total=%d len=%d", kwTotal, len(kw))
	}
}

func TestGetContestListScoped_EmptyMembers(t *testing.T) {
	d := setupContestListDB(t)
	logs, total, err := d.GetContestListScoped(context.Background(), ContestListQuery{
		UserId: -1, MemberIDs: []int64{}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if total != 0 || len(logs) != 0 {
		t.Fatalf("want empty, got total=%d len=%d", total, len(logs))
	}
}
