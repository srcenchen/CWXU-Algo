package backupcoord

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	backupLockKey    = "core:backup:lock:v1"
	backupStatusKey  = "core:backup:status:v1"
	backupDayPrefix  = "core:backup:scheduled-running:v2:"
	backupDonePrefix = "core:backup:scheduled-done:v2:"
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

var claimDayScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[2]) == 1 then return 0 end
return redis.call("SET", KEYS[1], "1", "PX", ARGV[1], "NX") and 1 or 0`)

var completeDayScript = redis.NewScript(`
redis.call("SET", KEYS[2], "1", "PX", ARGV[1])
redis.call("DEL", KEYS[1])
return 1`)

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

func (s *redisStatusStore) ClaimDay(ctx context.Context, day string, ttl time.Duration) (bool, error) {
	if s.rdb == nil {
		return false, errors.New("backup Redis is unavailable")
	}
	n, err := claimDayScript.Run(ctx, s.rdb, []string{backupDayPrefix + day, backupDonePrefix + day}, ttl.Milliseconds()).Int64()
	return n == 1, err
}

func (s *redisStatusStore) CompleteDay(ctx context.Context, day string, ttl time.Duration) error {
	if s.rdb == nil {
		return errors.New("backup Redis is unavailable")
	}
	_, err := completeDayScript.Run(ctx, s.rdb, []string{backupDayPrefix + day, backupDonePrefix + day}, ttl.Milliseconds()).Result()
	return err
}

func (s *redisStatusStore) ReleaseDay(ctx context.Context, day string) error {
	if s.rdb == nil {
		return errors.New("backup Redis is unavailable")
	}
	return s.rdb.Del(ctx, backupDayPrefix+day).Err()
}
