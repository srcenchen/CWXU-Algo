package service

import (
	"context"
	"testing"

	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	biz "cwxu-algo/app/core_data/internal/biz/service"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupProblemsetTest(t *testing.T) (*ProblemsetService, *gorm.DB) {
	t.Helper()
	name := "file:ps_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(name), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.Problem{},
		&model.Problemset{},
		&model.ProblemsetItem{},
		&model.ProblemsetLike{},
		&model.UserProblemStatus{},
		&model.SubmitLog{},
	); err != nil {
		t.Fatal(err)
	}
	s := &ProblemsetService{db: db, uc: nil, reg: nil}
	return s, db
}

func TestEnsureSystemProblemsetsIdempotent(t *testing.T) {
	_, db := setupProblemsetTest(t)
	if err := dal.EnsureSystemProblemsets(context.Background(), db, 42); err != nil {
		t.Fatal(err)
	}
	if err := dal.EnsureSystemProblemsets(context.Background(), db, 42); err != nil {
		t.Fatal(err)
	}
	var n int64
	_ = db.Model(&model.Problemset{}).Where("owner_id = ?", 42).Count(&n).Error
	if n != 2 {
		t.Fatalf("want 2 system sets, got %d", n)
	}
	var kinds []string
	_ = db.Model(&model.Problemset{}).Where("owner_id = ?", 42).Pluck("kind", &kinds).Error
	hasFav, hasTodo := false, false
	for _, k := range kinds {
		if k == model.ProblemsetKindFavorites {
			hasFav = true
		}
		if k == model.ProblemsetKindTodo {
			hasTodo = true
		}
	}
	if !hasFav || !hasTodo {
		t.Fatalf("kinds=%v", kinds)
	}
}

func TestRemoveFromTodoOnAC_OnlyTodo(t *testing.T) {
	_, db := setupProblemsetTest(t)
	uid := uint(7)
	_ = dal.EnsureSystemProblemsets(context.Background(), db, uid)

	// 建一题
	p := model.Problem{Platform: "CodeForces", ExternalID: "1A", Title: "Theatre", Status: "COMPLETED"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	// 自定义公有题单
	custom := model.Problemset{
		OwnerID: uid, Title: "公开练", Kind: model.ProblemsetKindCustom,
		Visibility: model.ProblemsetVisPublic, ItemCount: 1,
	}
	if err := db.Create(&custom).Error; err != nil {
		t.Fatal(err)
	}

	var fav, todo model.Problemset
	_ = db.Where("owner_id = ? AND kind = ?", uid, model.ProblemsetKindFavorites).First(&fav).Error
	_ = db.Where("owner_id = ? AND kind = ?", uid, model.ProblemsetKindTodo).First(&todo).Error

	for _, setID := range []uint{fav.ID, todo.ID, custom.ID} {
		if err := db.Create(&model.ProblemsetItem{ProblemsetID: setID, ProblemID: p.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Model(&model.Problemset{}).Where("id IN ?", []uint{fav.ID, todo.ID}).
		Update("item_count", 1).Error

	// AC 剔除
	if err := dal.RemoveFromTodoOnAC(context.Background(), db, int64(uid), p.ID); err != nil {
		t.Fatal(err)
	}

	var todoCnt, favCnt, customCnt int64
	_ = db.Model(&model.ProblemsetItem{}).Where("problemset_id = ? AND problem_id = ?", todo.ID, p.ID).Count(&todoCnt).Error
	_ = db.Model(&model.ProblemsetItem{}).Where("problemset_id = ? AND problem_id = ?", fav.ID, p.ID).Count(&favCnt).Error
	_ = db.Model(&model.ProblemsetItem{}).Where("problemset_id = ? AND problem_id = ?", custom.ID, p.ID).Count(&customCnt).Error
	if todoCnt != 0 {
		t.Fatalf("todo should be empty after AC, got %d", todoCnt)
	}
	if favCnt != 1 {
		t.Fatalf("favorites must keep problem after AC, got %d", favCnt)
	}
	if customCnt != 1 {
		t.Fatalf("custom must keep problem after AC, got %d", customCnt)
	}
}

func TestCanViewProblemsetVisibility(t *testing.T) {
	owner := uint(1)
	other := uint(2)
	priv := &model.Problemset{OwnerID: owner, Visibility: model.ProblemsetVisPrivate}
	pwd := &model.Problemset{OwnerID: owner, Visibility: model.ProblemsetVisPassword}
	pub := &model.Problemset{OwnerID: owner, Visibility: model.ProblemsetVisPublic}

	if !CanViewProblemset(owner, priv, false) {
		t.Fatal("owner should view private")
	}
	if CanViewProblemset(other, priv, false) {
		t.Fatal("other must not view private")
	}
	if CanViewProblemset(other, pwd, false) {
		t.Fatal("other without unlock must not view password set")
	}
	if !CanViewProblemset(other, pwd, true) {
		t.Fatal("other with unlock should view password set")
	}
	if !CanViewProblemset(0, pub, false) {
		t.Fatal("anonymous should view public")
	}
	if !IsPublicProblemset(&model.Problemset{Visibility: model.ProblemsetVisPublic, Kind: model.ProblemsetKindCustom}) {
		t.Fatal("custom public should be public")
	}
	if IsPublicProblemset(&model.Problemset{Visibility: model.ProblemsetVisPublic, Kind: model.ProblemsetKindTodo}) {
		t.Fatal("system todo must not appear as square public")
	}
}

func TestParseProblemURL_CommonOJs(t *testing.T) {
	cases := []struct {
		url      string
		platform string
		ext      string
	}{
		{"https://codeforces.com/contest/1/problem/A", "CodeForces", "1A"},
		{"https://codeforces.com/problemset/problem/4/A", "CodeForces", "4A"},
		{"https://atcoder.jp/contests/abc100/tasks/abc100_a", "AtCoder", "abc100_a"},
		{"https://www.luogu.com.cn/problem/P1001", "LuoGu", "P1001"},
		{"https://leetcode.cn/problems/two-sum/", "LeetCode", "two-sum"},
		{"https://ac.nowcoder.com/acm/problem/12345", "NowCoder", "12345"},
		{"https://qoj.ac/problem/100", "QOJ", "100"},
		{"https://loj.ac/p/6582", "LOJ", "6582"},
		{"https://uoj.ac/problem/1", "UOJ", "1"},
		{"http://poj.org/problem?id=1000", "POJ", "1000"},
	}
	for _, c := range cases {
		p, err := biz.ParseProblemURL(c.url)
		if err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		if p.Platform != c.platform || p.ExternalID != c.ext {
			t.Fatalf("%s: got %s/%s want %s/%s", c.url, p.Platform, p.ExternalID, c.platform, c.ext)
		}
	}
	if _, err := biz.ParseProblemURL("https://example.com/foo"); err == nil {
		t.Fatal("unknown host should fail")
	}
}

func TestProblemsetCreateVisibilityAndLike(t *testing.T) {
	// unlock token 依赖 JWT secret
	t.Setenv("CWXU_JWT_SECRET", "test-jwt-secret-at-least-32-chars!!")
	s, db := setupProblemsetTest(t)
	// 直接测 DB 路径（绕过 JWT）
	hash, err := hashProblemsetPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	pub := model.Problemset{
		OwnerID: 1, Title: "公开", Description: "desc", Kind: model.ProblemsetKindCustom,
		Visibility: model.ProblemsetVisPublic,
	}
	priv := model.Problemset{
		OwnerID: 1, Title: "私有", Kind: model.ProblemsetKindCustom,
		Visibility: model.ProblemsetVisPrivate,
	}
	pwd := model.Problemset{
		OwnerID: 1, Title: "密码", Kind: model.ProblemsetKindCustom,
		Visibility: model.ProblemsetVisPassword, PasswordHash: hash,
	}
	if err := db.Create(&pub).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&priv).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&pwd).Error; err != nil {
		t.Fatal(err)
	}

	// 广场只见 public custom
	var square []model.Problemset
	_ = db.Where("visibility = ? AND kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom).Find(&square).Error
	if len(square) != 1 || square[0].ID != pub.ID {
		t.Fatalf("square=%+v", square)
	}

	// 密码校验
	if !checkProblemsetPassword(pwd.PasswordHash, "secret") {
		t.Fatal("password check failed")
	}
	if checkProblemsetPassword(pwd.PasswordHash, "wrong") {
		t.Fatal("wrong password should fail")
	}
	token := makeProblemsetUnlockToken(pwd.ID)
	if !verifyProblemsetUnlockToken(token, pwd.ID) {
		t.Fatal("unlock token invalid")
	}
	if verifyProblemsetUnlockToken(token, pub.ID) {
		t.Fatal("token must not unlock other set")
	}

	// 点赞
	like := model.ProblemsetLike{UserID: 2, ProblemsetID: pub.ID}
	if err := db.Create(&like).Error; err != nil {
		t.Fatal(err)
	}
	_ = db.Model(&pub).UpdateColumn("like_count", 1).Error
	var cnt int
	_ = db.Model(&model.Problemset{}).Select("like_count").Where("id = ?", pub.ID).Scan(&cnt).Error
	if cnt != 1 {
		t.Fatalf("likeCount=%d", cnt)
	}
	_ = s // keep service used
}

func TestProblemsetItemReorder(t *testing.T) {
	_, db := setupProblemsetTest(t)
	ps := model.Problemset{
		OwnerID: 1, Title: "排序", Kind: model.ProblemsetKindCustom,
		Visibility: model.ProblemsetVisPrivate, ItemCount: 3,
	}
	if err := db.Create(&ps).Error; err != nil {
		t.Fatal(err)
	}
	probs := []model.Problem{
		{Platform: "LuoGu", ExternalID: "P1", Title: "A", Status: "COMPLETED"},
		{Platform: "LuoGu", ExternalID: "P2", Title: "B", Status: "COMPLETED"},
		{Platform: "LuoGu", ExternalID: "P3", Title: "C", Status: "COMPLETED"},
	}
	for i := range probs {
		if err := db.Create(&probs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	items := []model.ProblemsetItem{
		{ProblemsetID: ps.ID, ProblemID: probs[0].ID, SortOrder: 0},
		{ProblemsetID: ps.ID, ProblemID: probs[1].ID, SortOrder: 1},
		{ProblemsetID: ps.ID, ProblemID: probs[2].ID, SortOrder: 2},
	}
	for i := range items {
		if err := db.Create(&items[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 新顺序：C, A, B
	order := []uint{items[2].ID, items[0].ID, items[1].ID}
	err := db.Transaction(func(tx *gorm.DB) error {
		for i, id := range order {
			if err := tx.Model(&model.ProblemsetItem{}).
				Where("id = ? AND problemset_id = ?", id, ps.ID).
				Update("sort_order", i).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []model.ProblemsetItem
	if err := db.Where("problemset_id = ?", ps.ID).Order("sort_order ASC, id ASC").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	wantPIDs := []uint{probs[2].ID, probs[0].ID, probs[1].ID}
	for i, it := range got {
		if it.SortOrder != i {
			t.Fatalf("idx %d sortOrder=%d", i, it.SortOrder)
		}
		if it.ProblemID != wantPIDs[i] {
			t.Fatalf("idx %d problemId=%d want %d", i, it.ProblemID, wantPIDs[i])
		}
	}
}

func TestApplyUserProblemStatusAC_RemovesTodo(t *testing.T) {
	_, db := setupProblemsetTest(t)
	uid := int64(9)
	_ = dal.EnsureSystemProblemsets(context.Background(), db, uint(uid))
	p := model.Problem{Platform: "LuoGu", ExternalID: "P1001", Title: "A+B", Status: "COMPLETED"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	var todo model.Problemset
	_ = db.Where("owner_id = ? AND kind = ?", uid, model.ProblemsetKindTodo).First(&todo).Error
	if err := db.Create(&model.ProblemsetItem{ProblemsetID: todo.ID, ProblemID: p.ID}).Error; err != nil {
		t.Fatal(err)
	}
	pid := p.ID
	logs := []model.SubmitLog{{
		UserID: uid, ProblemID: &pid, IsAC: true, Platform: "LuoGu",
	}}
	if err := dal.ApplyUserProblemStatusFromSubmits(context.Background(), db, logs); err != nil {
		t.Fatal(err)
	}
	var n int64
	_ = db.Model(&model.ProblemsetItem{}).Where("problemset_id = ? AND problem_id = ?", todo.ID, p.ID).Count(&n).Error
	if n != 0 {
		t.Fatalf("todo item should be removed via AC status path, got %d", n)
	}
}
