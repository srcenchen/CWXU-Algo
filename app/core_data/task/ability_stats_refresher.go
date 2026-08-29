package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	abilityStatsRefreshLockKey   = "core:ability_stats:refresh:v1"
	abilityStatsRefreshLockLease = 5 * time.Minute
	abilityStatsRefreshPoll      = 250 * time.Millisecond
)

// AbilityStatsRefreshMode distinguishes readiness, ad-hoc force refreshes, and
// a daily scheduled refresh coalesced by its DB period marker.
type AbilityStatsRefreshMode uint8

const (
	AbilityStatsEnsureActive AbilityStatsRefreshMode = iota
	AbilityStatsForceNew
	AbilityStatsScheduledDaily
)

// AbilityStatsRefresher is the single scheduling/admin entry point for the
// versioned problem posterior. Redis only suppresses duplicate builders; the
// DB's atomic active-version protocol remains the source of correctness.
type AbilityStatsRefresher interface {
	Refresh(context.Context, AbilityStatsRefreshMode) (uint64, error)
}

// AbilityStatsMaintenanceTransition is committed in the same transaction as
// the active ability-model switch. Callers must use the supplied transaction.
type AbilityStatsMaintenanceTransition func(context.Context, *gorm.DB, uint64) error

// AbilityStatsMaintenanceRefresher is required by durable fact-maintenance
// flows so a crash cannot separate the active-model switch from MODEL_READY.
type AbilityStatsMaintenanceRefresher interface {
	RefreshForMaintenance(context.Context, AbilityStatsMaintenanceTransition) (uint64, error)
}

type abilityStatsStore interface {
	ActiveVersion(context.Context) (uint64, error)
	Refresh(context.Context) error
	RefreshScheduled(context.Context, string) (uint64, bool, error)
}

type abilityStatsMaintenanceStore interface {
	RefreshForMaintenance(context.Context, AbilityStatsMaintenanceTransition) (uint64, error)
}

type abilityStatsLock interface {
	TryLock(context.Context, string, time.Duration) (bool, error)
	Renew(context.Context, string, time.Duration) (bool, error)
	Unlock(context.Context, string) error
}

type abilityStatsWaiter interface {
	Wait(context.Context) error
}

type abilityStatsLeaseKeeper interface {
	Start(abilityStatsLock, string, time.Duration) func()
}

// ProblemAbilityStatsRefresher coordinates refresh callers around the DAL's
// atomically published ability_model_state pointer.
type ProblemAbilityStatsRefresher struct {
	store       abilityStatsStore
	lock        abilityStatsLock
	waiter      abilityStatsWaiter
	now         func() time.Time
	leaseKeeper abilityStatsLeaseKeeper
}

func NewProblemAbilityStatsRefresher(d *data.Data) *ProblemAbilityStatsRefresher {
	var db *gorm.DB
	var locker abilityStatsLock
	if d != nil {
		db = d.DB
		if d.RDB != nil {
			locker = &redisAbilityStatsLock{rdb: d.RDB}
		}
	}
	return newProblemAbilityStatsRefresher(
		&gormAbilityStatsStore{db: db},
		locker,
		timerAbilityStatsWaiter{interval: abilityStatsRefreshPoll},
	)
}

func newProblemAbilityStatsRefresher(store abilityStatsStore, locker abilityStatsLock, waiter abilityStatsWaiter) *ProblemAbilityStatsRefresher {
	return &ProblemAbilityStatsRefresher{
		store: store, lock: locker, waiter: waiter, now: time.Now,
		leaseKeeper: tickerAbilityStatsLeaseKeeper{},
	}
}

func (r *ProblemAbilityStatsRefresher) Refresh(ctx context.Context, mode AbilityStatsRefreshMode) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.store == nil {
		return 0, errors.New("problem ability stats refresher: nil store")
	}
	if mode != AbilityStatsEnsureActive && mode != AbilityStatsForceNew && mode != AbilityStatsScheduledDaily {
		return 0, fmt.Errorf("problem ability stats refresher: invalid mode %d", mode)
	}

	baseline, err := r.store.ActiveVersion(ctx)
	if err != nil {
		return 0, err
	}
	if mode == AbilityStatsEnsureActive && baseline > 0 {
		return baseline, nil
	}
	period := ""
	if mode == AbilityStatsScheduledDaily {
		now := time.Now()
		if r.now != nil {
			now = r.now()
		}
		period = abilityStatsDailyPeriod(now)
	}

	token := newAbilityStatsLockToken()
	for {
		acquired, lockErr := r.tryLock(ctx, token)
		if lockErr != nil {
			// Redis is an optimization only. Falling through to the DB refresh is
			// safe because RefreshProblemAbilityStats serializes publication.
			log.Warnf("ability stats refresh lock unavailable, using DB protocol: %v", lockErr)
			if mode == AbilityStatsScheduledDaily {
				return r.refreshScheduled(ctx, period)
			}
			return r.refreshAndRead(ctx, baseline, mode)
		}
		if acquired {
			defer func() {
				if r.lock != nil {
					if err := r.lock.Unlock(context.Background(), token); err != nil {
						log.Warnf("ability stats refresh unlock: %v", err)
					}
				}
			}()
			current, err := r.store.ActiveVersion(ctx)
			if err != nil {
				return 0, err
			}
			if abilityStatsVersionReady(mode, baseline, current) {
				return current, nil
			}
			stopRenewal := func() {}
			if r.leaseKeeper != nil && r.lock != nil {
				stopRenewal = r.leaseKeeper.Start(r.lock, token, abilityStatsRefreshLockLease)
			}
			defer stopRenewal()
			if mode == AbilityStatsScheduledDaily {
				return r.refreshScheduled(ctx, period)
			}
			return r.refreshAndRead(ctx, baseline, mode)
		}

		current, err := r.store.ActiveVersion(ctx)
		if err != nil {
			return 0, err
		}
		if abilityStatsVersionReady(mode, baseline, current) {
			return current, nil
		}
		if r.waiter == nil {
			return 0, errors.New("problem ability stats refresher: lock held and no waiter")
		}
		if err := r.waiter.Wait(ctx); err != nil {
			return 0, err
		}
	}
}

func (r *ProblemAbilityStatsRefresher) RefreshForMaintenance(ctx context.Context, transition AbilityStatsMaintenanceTransition) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.store == nil || transition == nil {
		return 0, errors.New("problem ability stats refresher: invalid maintenance refresh")
	}
	store, ok := r.store.(abilityStatsMaintenanceStore)
	if !ok {
		return 0, errors.New("problem ability stats refresher: maintenance store unavailable")
	}
	token := newAbilityStatsLockToken()
	for {
		acquired, lockErr := r.tryLock(ctx, token)
		if lockErr != nil {
			log.Warnf("ability stats maintenance lock unavailable, using DB protocol: %v", lockErr)
			return store.RefreshForMaintenance(ctx, transition)
		}
		if acquired {
			defer func() {
				if r.lock != nil {
					if err := r.lock.Unlock(context.Background(), token); err != nil {
						log.Warnf("ability stats maintenance unlock: %v", err)
					}
				}
			}()
			stopRenewal := func() {}
			if r.leaseKeeper != nil && r.lock != nil {
				stopRenewal = r.leaseKeeper.Start(r.lock, token, abilityStatsRefreshLockLease)
			}
			defer stopRenewal()
			return store.RefreshForMaintenance(ctx, transition)
		}
		if r.waiter == nil {
			return 0, errors.New("problem ability stats refresher: maintenance lock held and no waiter")
		}
		if err := r.waiter.Wait(ctx); err != nil {
			return 0, err
		}
	}
}

func (r *ProblemAbilityStatsRefresher) tryLock(ctx context.Context, token string) (bool, error) {
	if r.lock == nil {
		return true, nil
	}
	return r.lock.TryLock(ctx, token, abilityStatsRefreshLockLease)
}

func (r *ProblemAbilityStatsRefresher) refreshAndRead(ctx context.Context, baseline uint64, mode AbilityStatsRefreshMode) (uint64, error) {
	if err := r.store.Refresh(ctx); err != nil {
		return 0, err
	}
	version, err := r.store.ActiveVersion(ctx)
	if err != nil {
		return 0, err
	}
	if !abilityStatsVersionReady(mode, baseline, version) {
		return 0, fmt.Errorf("problem ability stats refresher: published version %d did not satisfy mode %d from baseline %d", version, mode, baseline)
	}
	return version, nil
}

func abilityStatsVersionReady(mode AbilityStatsRefreshMode, baseline, current uint64) bool {
	if mode == AbilityStatsEnsureActive {
		return current > 0
	}
	if mode == AbilityStatsForceNew {
		return current > baseline
	}
	// Scheduled refreshes are coalesced by their DB period marker, not merely
	// by an active-version advance that could have come from an admin force.
	return false
}

func (r *ProblemAbilityStatsRefresher) refreshScheduled(ctx context.Context, period string) (uint64, error) {
	version, _, err := r.store.RefreshScheduled(ctx, period)
	return version, err
}

func abilityStatsDailyPeriod(now time.Time) string {
	return now.In(cronTZ()).Format("2006-01-02")
}

type gormAbilityStatsStore struct{ db *gorm.DB }

func (s *gormAbilityStatsStore) ActiveVersion(ctx context.Context) (uint64, error) {
	version, err := dal.ActiveAbilityModelVersion(ctx, s.db)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return version, err
}

func (s *gormAbilityStatsStore) Refresh(ctx context.Context) error {
	return dal.RefreshProblemAbilityStats(ctx, s.db)
}

func (s *gormAbilityStatsStore) RefreshForMaintenance(ctx context.Context, transition AbilityStatsMaintenanceTransition) (uint64, error) {
	return dal.RefreshProblemAbilityStatsForMaintenance(ctx, s.db, func(callbackCtx context.Context, tx *gorm.DB, version uint64) error {
		return transition(callbackCtx, tx, version)
	})
}

func (s *gormAbilityStatsStore) RefreshScheduled(ctx context.Context, period string) (uint64, bool, error) {
	return dal.RefreshProblemAbilityStatsForPeriod(ctx, s.db, period)
}

type redisAbilityStatsLock struct{ rdb *redis.Client }

func (l *redisAbilityStatsLock) TryLock(ctx context.Context, token string, lease time.Duration) (bool, error) {
	if l == nil || l.rdb == nil {
		return true, nil
	}
	return l.rdb.SetNX(ctx, abilityStatsRefreshLockKey, token, lease).Result()
}

var abilityStatsRenewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

func (l *redisAbilityStatsLock) Renew(ctx context.Context, token string, lease time.Duration) (bool, error) {
	if l == nil || l.rdb == nil {
		return true, nil
	}
	millis := lease.Milliseconds()
	if millis <= 0 {
		millis = 1
	}
	n, err := abilityStatsRenewScript.Run(
		ctx, l.rdb, []string{abilityStatsRefreshLockKey}, token, millis,
	).Int64()
	return n == 1, err
}

var abilityStatsUnlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

func (l *redisAbilityStatsLock) Unlock(ctx context.Context, token string) error {
	if l == nil || l.rdb == nil {
		return nil
	}
	return abilityStatsUnlockScript.Run(ctx, l.rdb, []string{abilityStatsRefreshLockKey}, token).Err()
}

type timerAbilityStatsWaiter struct{ interval time.Duration }

func (w timerAbilityStatsWaiter) Wait(ctx context.Context) error {
	interval := w.interval
	if interval <= 0 {
		interval = abilityStatsRefreshPoll
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type tickerAbilityStatsLeaseKeeper struct{}

func (tickerAbilityStatsLeaseKeeper) Start(lock abilityStatsLock, token string, lease time.Duration) func() {
	if lock == nil || lease <= 0 {
		return func() {}
	}
	interval := lease / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, interval)
				renewed, err := lock.Renew(renewCtx, token, lease)
				renewCancel()
				if err != nil {
					log.Warnf("ability stats refresh lease renewal: %v", err)
					continue
				}
				if !renewed {
					log.Warnf("ability stats refresh lease lost before DB refresh completed")
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

var abilityStatsTokenFallback atomic.Uint64

func newAbilityStatsLockToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), abilityStatsTokenFallback.Add(1))
}
