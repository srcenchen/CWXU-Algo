package service

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestWithPurgeUserPlatformGuardsBumpsGenerationBeforeLocksAndDelete(t *testing.T) {
	var events []string
	err := withPurgeUserPlatformGuards(context.Background(), []string{"AtCoder", "LuoGu"},
		func(platform string) { events = append(events, "bump:"+platform) },
		func(_ context.Context, platform string) (func(), bool) {
			events = append(events, "lock:"+platform)
			return func() { events = append(events, "unlock:"+platform) }, true
		},
		func() error { events = append(events, "delete"); return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bump:AtCoder", "bump:LuoGu", "lock:AtCoder", "lock:LuoGu", "delete", "unlock:LuoGu", "unlock:AtCoder"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestWithPurgeUserPlatformGuardsInvalidatesOldTaskGeneration(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	spiderTask := task.NewSpiderTask(nil, rdb, nil)
	const userID = int64(17)
	oldGeneration := task.CurrentGeneration(rdb, userID, "QOJ")

	err := withPurgeUserPlatformGuards(context.Background(), []string{"QOJ"},
		func(platform string) { spiderTask.BumpGeneration(userID, platform) },
		func(context.Context, string) (func(), bool) { return func() {}, true },
		func() error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if current := task.CurrentGeneration(rdb, userID, "QOJ"); current == oldGeneration {
		t.Fatalf("old task generation remained valid: old=%d current=%d", oldGeneration, current)
	}
}

func TestWithPurgeUserPlatformGuardsWaitsForHeldLockBeforeDelete(t *testing.T) {
	held := make(chan struct{})
	acquired := make(chan struct{})
	deleted := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		done <- withPurgeUserPlatformGuards(context.Background(), []string{"QOJ"}, func(string) {},
			func(ctx context.Context, _ string) (func(), bool) {
				select {
				case <-held:
					once.Do(func() { close(acquired) })
					return func() {}, true
				case <-ctx.Done():
					return func() {}, false
				}
			}, func() error { close(deleted); return nil })
	}()
	select {
	case <-deleted:
		t.Fatal("delete ran while platform lock was held")
	case <-time.After(20 * time.Millisecond):
	}
	close(held)
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("purge did not acquire released lock")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-deleted:
	default:
		t.Fatal("delete did not run after acquiring lock")
	}
}

func TestWithPurgeUserPlatformGuardsLockFailureSkipsDeleteAndReleasesLocks(t *testing.T) {
	deleted := false
	unlocked := false
	locks := 0
	err := withPurgeUserPlatformGuards(context.Background(), []string{"AtCoder", "QOJ"}, func(string) {},
		func(context.Context, string) (func(), bool) {
			locks++
			if locks == 2 {
				return func() {}, false
			}
			return func() { unlocked = true }, true
		}, func() error { deleted = true; return nil })
	if err == nil || deleted || !unlocked {
		t.Fatalf("err=%v deleted=%v unlocked=%v", err, deleted, unlocked)
	}
}

func TestTrySpiderPlatformWriteLockSerializesAndUnlocks(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	unlock, ok := trySpiderPlatformWriteLock(context.Background(), rdb, 7, "LuoGu")
	if !ok {
		t.Fatal("first lock must succeed")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := trySpiderPlatformWriteLock(cancelled, rdb, 7, "LuoGu"); ok {
		t.Fatal("contending cancelled lock must fail")
	}
	unlock()
	if _, ok := trySpiderPlatformWriteLock(context.Background(), rdb, 7, "LuoGu"); !ok {
		t.Fatal("lock must be acquirable after unlock")
	}
}

func TestTrySpiderPlatformWriteLockUnlockDoesNotDeleteNewOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	key := "spider:writelock:9:QOJ"

	unlockA, ok := trySpiderPlatformWriteLock(ctx, rdb, 9, "QOJ")
	if !ok {
		t.Fatal("owner A must acquire lock")
	}
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	unlockB, ok := trySpiderPlatformWriteLock(ctx, rdb, 9, "QOJ")
	if !ok {
		t.Fatal("owner B must acquire expired lock")
	}
	defer unlockB()

	unlockA()
	if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 1 {
		t.Fatalf("owner A unlock deleted owner B lock: exists=%d err=%v", n, err)
	}
}

func TestTrySpiderPlatformWriteLockUnlockReleasesOwnLock(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	key := "spider:writelock:10:AtCoder"

	unlock, ok := trySpiderPlatformWriteLock(ctx, rdb, 10, "AtCoder")
	if !ok {
		t.Fatal("lock must be acquired")
	}
	unlock()
	if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("owner unlock did not release lock: exists=%d err=%v", n, err)
	}
}

func TestReplaceSpiderBindingDeletesPlatformRepairState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Platform{}, &model.SubmitLog{}, &model.ContestLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SpiderRepairState{UserID: 7, Platform: "LuoGu", RepairKey: "test", Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := replaceSpiderBinding(context.Background(), db, model.Platform{UserID: 7, Platform: "LuoGu", Username: "new-user"}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.SpiderRepairState{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("repair state count=%d want=0", count)
	}
}

func TestPurgeSpiderRepairStatesDeletesAllUserPlatforms(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	states := []model.SpiderRepairState{
		{UserID: 7, Platform: "QOJ", RepairKey: "a", Version: 1, CompletedAt: time.Now()},
		{UserID: 7, Platform: "LuoGu", RepairKey: "b", Version: 1, CompletedAt: time.Now()},
		{UserID: 8, Platform: "QOJ", RepairKey: "a", Version: 1, CompletedAt: time.Now()},
	}
	if err := db.Create(&states).Error; err != nil {
		t.Fatal(err)
	}
	if err := purgeSpiderRepairStates(context.Background(), db, 7); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.SpiderRepairState{}).Where("user_id = ?", 7).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("purged user count=%d err=%v", count, err)
	}
	if err := db.Model(&model.SpiderRepairState{}).Where("user_id = ?", 8).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("other user count=%d err=%v", count, err)
	}
}
