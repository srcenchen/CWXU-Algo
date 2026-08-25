package service

import (
	"context"
	"fmt"
	"testing"

	"cwxu-algo/app/core_data/internal/data"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestTryPlatformWriteLockUnlockDoesNotDeleteNewOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SpiderUseCase{data: &data.Data{RDB: rdb}}
	ctx := context.Background()
	key := fmt.Sprintf("spider:writelock:%d:%s", 7, "LuoGu")

	unlockA, ok := uc.tryPlatformWriteLock(ctx, 7, "LuoGu")
	if !ok {
		t.Fatal("owner A must acquire lock")
	}
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	unlockB, ok := uc.tryPlatformWriteLock(ctx, 7, "LuoGu")
	if !ok {
		t.Fatal("owner B must acquire expired lock")
	}
	defer unlockB()

	unlockA()
	if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 1 {
		t.Fatalf("owner A unlock deleted owner B lock: exists=%d err=%v", n, err)
	}
}

func TestTryPlatformWriteLockUnlockReleasesOwnLock(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SpiderUseCase{data: &data.Data{RDB: rdb}}
	ctx := context.Background()
	key := fmt.Sprintf("spider:writelock:%d:%s", 8, "QOJ")

	unlock, ok := uc.tryPlatformWriteLock(ctx, 8, "QOJ")
	if !ok {
		t.Fatal("lock must be acquired")
	}
	unlock()
	if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("owner unlock did not release lock: exists=%d err=%v", n, err)
	}
}
