package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeAbilityStatsStore struct {
	active         uint64
	activeErr      error
	refreshErr     error
	refreshHook    func() error
	refreshCalls   int
	scheduled      map[string]uint64
	scheduledCalls int
}

func (s *fakeAbilityStatsStore) ActiveVersion(context.Context) (uint64, error) {
	return s.active, s.activeErr
}

func (s *fakeAbilityStatsStore) Refresh(context.Context) error {
	s.refreshCalls++
	if s.refreshHook != nil {
		if err := s.refreshHook(); err != nil {
			return err
		}
	}
	if s.refreshErr != nil {
		return s.refreshErr
	}
	s.active++
	return nil
}

func (s *fakeAbilityStatsStore) RefreshScheduled(_ context.Context, period string) (uint64, bool, error) {
	s.scheduledCalls++
	if version := s.scheduled[period]; version > 0 {
		return s.active, false, nil
	}
	if s.scheduled == nil {
		s.scheduled = map[string]uint64{}
	}
	s.active++
	s.scheduled[period] = s.active
	return s.active, true, nil
}

type fakeAbilityStatsLock struct {
	results     []bool
	err         error
	tryCalls    int
	lockToken   string
	unlockToken string
}

func (l *fakeAbilityStatsLock) TryLock(_ context.Context, token string, _ time.Duration) (bool, error) {
	l.tryCalls++
	l.lockToken = token
	if l.err != nil {
		return false, l.err
	}
	i := l.tryCalls - 1
	if i >= len(l.results) || !l.results[i] {
		return false, nil
	}
	return true, nil
}

func (l *fakeAbilityStatsLock) Unlock(_ context.Context, token string) error {
	l.unlockToken = token
	return nil
}

func (l *fakeAbilityStatsLock) Renew(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

type controlledAbilityStatsLeaseKeeper struct {
	active bool
	starts int
	stops  int
}

func (k *controlledAbilityStatsLeaseKeeper) Start(abilityStatsLock, string, time.Duration) func() {
	k.starts++
	k.active = true
	return func() {
		k.active = false
		k.stops++
	}
}

type controlledAbilityStatsWaiter struct {
	hooks []func()
	err   error
	calls int
}

func (w *controlledAbilityStatsWaiter) Wait(context.Context) error {
	w.calls++
	i := w.calls - 1
	if i < len(w.hooks) && w.hooks[i] != nil {
		w.hooks[i]()
	}
	return w.err
}

func TestAbilityStatsRefresherForceWaitsForOtherPublishedVersion(t *testing.T) {
	store := &fakeAbilityStatsStore{active: 7}
	lock := &fakeAbilityStatsLock{results: []bool{false, false}}
	waiter := &controlledAbilityStatsWaiter{hooks: []func(){func() { store.active = 8 }}}
	r := newProblemAbilityStatsRefresher(store, lock, waiter)

	version, err := r.Refresh(context.Background(), AbilityStatsForceNew)

	if err != nil || version != 8 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if store.refreshCalls != 0 {
		t.Fatalf("non-owner rebuilt instead of accepting the atomically published version: %d", store.refreshCalls)
	}
}

func TestAbilityStatsRefresherEnsureTakesOverWhenNoActiveVersion(t *testing.T) {
	store := &fakeAbilityStatsStore{}
	lock := &fakeAbilityStatsLock{results: []bool{false, true}}
	waiter := &controlledAbilityStatsWaiter{}
	r := newProblemAbilityStatsRefresher(store, lock, waiter)

	version, err := r.Refresh(context.Background(), AbilityStatsEnsureActive)

	if err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if store.refreshCalls != 1 || lock.tryCalls != 2 || waiter.calls != 1 {
		t.Fatalf("refresh=%d lock=%d waits=%d", store.refreshCalls, lock.tryCalls, waiter.calls)
	}
}

func TestAbilityStatsRefresherDoesNotSucceedWithoutActiveOrTakeover(t *testing.T) {
	waitErr := errors.New("controlled wait exhausted")
	store := &fakeAbilityStatsStore{}
	lock := &fakeAbilityStatsLock{results: []bool{false}}
	waiter := &controlledAbilityStatsWaiter{err: waitErr}
	r := newProblemAbilityStatsRefresher(store, lock, waiter)

	version, err := r.Refresh(context.Background(), AbilityStatsEnsureActive)

	if version != 0 || !errors.Is(err, waitErr) || store.refreshCalls != 0 {
		t.Fatalf("version=%d err=%v refresh=%d", version, err, store.refreshCalls)
	}
}

func TestAbilityStatsRefresherRedisFailureFallsBackToAtomicDBRefresh(t *testing.T) {
	store := &fakeAbilityStatsStore{active: 3}
	lock := &fakeAbilityStatsLock{err: errors.New("redis unavailable")}
	r := newProblemAbilityStatsRefresher(store, lock, &controlledAbilityStatsWaiter{})

	version, err := r.Refresh(context.Background(), AbilityStatsForceNew)

	if err != nil || version != 4 || store.refreshCalls != 1 {
		t.Fatalf("version=%d err=%v refresh=%d", version, err, store.refreshCalls)
	}
}

func TestAbilityStatsRefresherScheduledDailyCoalescesInDBWhenRedisFails(t *testing.T) {
	store := &fakeAbilityStatsStore{}
	lock := &fakeAbilityStatsLock{err: errors.New("redis unavailable")}
	r := newProblemAbilityStatsRefresher(store, lock, &controlledAbilityStatsWaiter{})
	r.now = func() time.Time { return time.Date(2026, 8, 29, 4, 0, 0, 0, cronTZ()) }

	first, err := r.Refresh(context.Background(), AbilityStatsScheduledDaily)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Refresh(context.Background(), AbilityStatsScheduledDaily)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 1 || store.scheduledCalls != 2 || store.active != 1 {
		t.Fatalf("first=%d second=%d scheduled_calls=%d active=%d", first, second, store.scheduledCalls, store.active)
	}
}

func TestAbilityStatsRefresherMaintainsOwnerLeaseAroundLongRefresh(t *testing.T) {
	keeper := &controlledAbilityStatsLeaseKeeper{}
	store := &fakeAbilityStatsStore{active: 3}
	store.refreshHook = func() error {
		if !keeper.active {
			return errors.New("lease keeper was not active during refresh")
		}
		return nil
	}
	lock := &fakeAbilityStatsLock{results: []bool{true}}
	r := newProblemAbilityStatsRefresher(store, lock, &controlledAbilityStatsWaiter{})
	r.leaseKeeper = keeper

	version, err := r.Refresh(context.Background(), AbilityStatsForceNew)

	if err != nil || version != 4 || keeper.starts != 1 || keeper.stops != 1 || keeper.active {
		t.Fatalf("version=%d err=%v keeper=%+v", version, err, keeper)
	}
}

func TestAbilityStatsRefresherReturnsRefreshFailureAndOwnerUnlocks(t *testing.T) {
	refreshErr := errors.New("posterior build failed")
	store := &fakeAbilityStatsStore{active: 3, refreshErr: refreshErr}
	lock := &fakeAbilityStatsLock{results: []bool{true}}
	r := newProblemAbilityStatsRefresher(store, lock, &controlledAbilityStatsWaiter{})

	version, err := r.Refresh(context.Background(), AbilityStatsForceNew)

	if version != 0 || !errors.Is(err, refreshErr) || store.refreshCalls != 1 {
		t.Fatalf("version=%d err=%v refresh=%d", version, err, store.refreshCalls)
	}
	if lock.lockToken == "" || lock.unlockToken != lock.lockToken {
		t.Fatalf("lock token=%q unlock token=%q", lock.lockToken, lock.unlockToken)
	}
}

func TestRedisAbilityStatsLockExpiredOwnerCannotDeleteTakeover(t *testing.T) {
	mr, rdb := newTestRedis(t)
	lock := &redisAbilityStatsLock{rdb: rdb}
	ctx := context.Background()

	acquired, err := lock.TryLock(ctx, "owner-a", time.Second)
	if err != nil || !acquired {
		t.Fatalf("acquire=%v err=%v", acquired, err)
	}
	mr.FastForward(2 * time.Second)
	acquired, err = lock.TryLock(ctx, "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("takeover=%v err=%v", acquired, err)
	}
	if err := lock.Unlock(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	if got, err := rdb.Get(ctx, abilityStatsRefreshLockKey).Result(); err != nil || got != "owner-b" {
		t.Fatalf("non-owner unlock deleted current lock: got=%q err=%v", got, err)
	}
}

func TestRedisAbilityStatsLockRenewsOnlyItsOwnerToken(t *testing.T) {
	mr, rdb := newTestRedis(t)
	lock := &redisAbilityStatsLock{rdb: rdb}
	ctx := context.Background()
	if acquired, err := lock.TryLock(ctx, "owner-a", time.Second); err != nil || !acquired {
		t.Fatalf("acquire=%v err=%v", acquired, err)
	}
	renewed, err := lock.Renew(ctx, "owner-a", 3*time.Second)
	if err != nil || !renewed {
		t.Fatalf("renewed=%v err=%v", renewed, err)
	}
	mr.FastForward(2 * time.Second)
	if got, err := rdb.Get(ctx, abilityStatsRefreshLockKey).Result(); err != nil || got != "owner-a" {
		t.Fatalf("renewal did not extend owner lease: got=%q err=%v", got, err)
	}
	mr.Set(abilityStatsRefreshLockKey, "owner-b")
	renewed, err = lock.Renew(ctx, "owner-a", 3*time.Second)
	if err != nil || renewed {
		t.Fatalf("stale owner renewed takeover: renewed=%v err=%v", renewed, err)
	}
}
