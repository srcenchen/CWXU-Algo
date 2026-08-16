package backupcoord

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	backupLockKey   = "core:backup:lock:v1"
	backupStatusKey = "core:backup:status:v1"
)

var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

func (s *redisStatusStore) TryLock(ctx context.Context, token string, lease time.Duration) (bool, error) {
	if s.rdb == nil {
		return false, errors.New("backup Redis is unavailable")
	}
	return s.rdb.SetNX(ctx, backupLockKey, token, lease).Result()
}

func (s *redisStatusStore) Renew(ctx context.Context, token string, lease time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, s.rdb, []string{backupLockKey}, token, lease.Milliseconds()).Int64()
	return n == 1, err
}

func (s *redisStatusStore) Unlock(ctx context.Context, token string) error {
	_, err := unlockScript.Run(ctx, s.rdb, []string{backupLockKey}, token).Result()
	return err
}

func (s *redisStatusStore) Load(ctx context.Context) (Status, bool, error) {
	value, err := s.rdb.Get(ctx, backupStatusKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	var status Status
	if err := json.Unmarshal(value, &status); err != nil {
		return Status{}, false, err
	}
	return status, true, nil
}

func (s *redisStatusStore) Save(ctx context.Context, status Status) error {
	value, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, backupStatusKey, value, 0).Err()
}
