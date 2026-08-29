package backupcoord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	native "cwxu-algo/app/core_data/internal/backup"
)

type fakeRunner struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	runs    int
}

type resultRunner struct {
	err  error
	runs int
	done chan struct{}
}

func (r *resultRunner) Run(context.Context) (native.Result, error) {
	r.runs++
	if r.done != nil {
		close(r.done)
	}
	return native.Result{}, r.err
}

func (r *fakeRunner) Run(ctx context.Context) (native.Result, error) {
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()
	close(r.started)
	select {
	case <-r.release:
		return native.Result{ArchiveKey: "backup/core.cwxubak", Size: 42, SHA256: "abc", Databases: 2}, nil
	case <-ctx.Done():
		return native.Result{}, ctx.Err()
	}
}

type fakeStore struct {
	mu       sync.Mutex
	locked   bool
	status   Status
	has      bool
	renewed  chan struct{}
	claimed  map[string]bool
	doneDays map[string]bool
}

func (s *fakeStore) ClaimDay(_ context.Context, day string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed == nil {
		s.claimed = map[string]bool{}
	}
	if s.doneDays[day] {
		return false, nil
	}
	if s.claimed[day] {
		return false, nil
	}
	s.claimed[day] = true
	return true, nil
}
func (s *fakeStore) CompleteDay(_ context.Context, day string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doneDays == nil {
		s.doneDays = map[string]bool{}
	}
	s.doneDays[day] = true
	delete(s.claimed, day)
	return nil
}
func (s *fakeStore) ReleaseDay(_ context.Context, day string) error {
	s.mu.Lock()
	delete(s.claimed, day)
	s.mu.Unlock()
	return nil
}

func (s *fakeStore) TryLock(context.Context, string, time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return false, nil
	}
	s.locked = true
	return true, nil
}
func (s *fakeStore) Renew(context.Context, string, time.Duration) (bool, error) {
	select {
	case s.renewed <- struct{}{}:
	default:
	}
	return true, nil
}
func (s *fakeStore) Unlock(context.Context, string) error {
	s.mu.Lock()
	s.locked = false
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) Load(context.Context) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.has, nil
}
func (s *fakeStore) Save(_ context.Context, status Status) error {
	s.mu.Lock()
	s.status, s.has = status, true
	s.mu.Unlock()
	return nil
}

func TestDisabledCoordinatorRejectsTriggerAndExposesReason(t *testing.T) {
	store := &fakeStore{}
	c := NewForTest(false, "missing backup configuration", nil, store)
	if err := c.Trigger(context.Background(), TriggerManual); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Trigger error = %v, want ErrDisabled", err)
	}
	status := c.Status()
	if status.Enabled || status.State != StateDisabled || status.Error != ErrDisabled.Error() {
		t.Fatalf("unexpected disabled status: %+v", status)
	}
	if !store.has || store.status.State != StateDisabled {
		t.Fatalf("disabled status was not persisted: %+v", store.status)
	}
}

func TestDynamicSwitchBlocksNewRunsAndStatusReflectsDisabled(t *testing.T) {
	enabled := true
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	c := NewForTestDynamic(func(context.Context) bool { return enabled }, "", runner, &fakeStore{renewed: make(chan struct{}, 1)})
	if err := c.Trigger(context.Background(), TriggerManual); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	enabled = false
	if got := c.Status(); got.Enabled || got.State != StateRunning {
		t.Fatalf("running status after disable = %+v", got)
	}
	c.mu.RLock()
	done := c.done
	c.mu.RUnlock()
	close(runner.release)
	<-done
	if err := c.Trigger(context.Background(), TriggerManual); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Trigger after disable = %v, want ErrDisabled", err)
	}
}

func TestDynamicSwitchReportsStaticConfigurationProblem(t *testing.T) {
	c := NewForTestDynamic(func(context.Context) bool { return true }, "missing CWXU_BACKUP_PG_DSN", nil, &fakeStore{})
	if err := c.Trigger(context.Background(), TriggerManual); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Trigger error = %v, want ErrDisabled", err)
	}
	status := c.Status()
	if status.Enabled || status.State != StateDisabled || status.Error != "missing CWXU_BACKUP_PG_DSN" {
		t.Fatalf("status = %+v", status)
	}
}

func TestDisabledSwitchDoesNotExposeIrrelevantStaticConfigurationProblem(t *testing.T) {
	c := NewForTestDynamic(func(context.Context) bool { return false }, "missing CWXU_BACKUP_PG_DSN", nil, &fakeStore{})
	status := c.Status()
	if status.Error != ErrDisabled.Error() {
		t.Fatalf("status error = %q, want disabled reason", status.Error)
	}
}

func TestDynamicSwitchStatusBecomesIdleAfterEnable(t *testing.T) {
	enabled := false
	c := NewForTestDynamic(func(context.Context) bool { return enabled }, "", &fakeRunner{}, &fakeStore{})
	if got := c.Status(); got.Enabled || got.State != StateDisabled {
		t.Fatalf("disabled status = %+v", got)
	}
	enabled = true
	if got := c.Status(); !got.Enabled || got.State != StateIdle || got.Error != "" {
		t.Fatalf("enabled status = %+v, want idle", got)
	}
}

func TestTriggerReturnsBeforeRunCompletesAndRejectsDuplicate(t *testing.T) {
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	store := &fakeStore{renewed: make(chan struct{}, 1)}
	c := NewForTest(true, "", runner, store)
	c.lease = 30 * time.Millisecond
	c.renewEvery = 5 * time.Millisecond

	if err := c.Trigger(context.Background(), TriggerManual); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("async runner did not start")
	}
	if got := c.Status(); got.State != StateRunning || got.Trigger != TriggerManual {
		t.Fatalf("running status = %+v", got)
	}
	if err := c.Trigger(context.Background(), TriggerScheduled); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("duplicate error = %v, want ErrAlreadyRunning", err)
	}
	select {
	case <-store.renewed:
	case <-time.After(time.Second):
		t.Fatal("lock lease was not renewed")
	}
	close(runner.release)
	waitState(t, c, StateSucceeded)
	got := c.Status()
	if got.ArchiveKey != "backup/core.cwxubak" || got.ArchiveSize != 42 || got.DatabaseCount != 2 {
		t.Fatalf("result status = %+v", got)
	}
}

func TestDistributedLockRejectsAnotherCoordinator(t *testing.T) {
	store := &fakeStore{renewed: make(chan struct{}, 1)}
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	first := NewForTest(true, "", runner, store)
	second := NewForTest(true, "", &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}, store)
	if err := first.Trigger(context.Background(), TriggerManual); err != nil {
		t.Fatal(err)
	}
	if err := second.Trigger(context.Background(), TriggerManual); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Trigger error = %v, want ErrAlreadyRunning", err)
	}
	close(runner.release)
	waitState(t, first, StateSucceeded)
}

func TestCoordinatorLoadsPersistedStatus(t *testing.T) {
	want := Status{Enabled: true, State: StateSucceeded, Trigger: TriggerScheduled, ArchiveKey: "old.cwxubak"}
	c := NewForTest(true, "", &fakeRunner{}, &fakeStore{status: want, has: true})
	if got := c.Status(); got.State != want.State || got.ArchiveKey != want.ArchiveKey || got.Trigger != want.Trigger {
		t.Fatalf("loaded status = %+v, want %+v", got, want)
	}
}

func TestStatusRefreshesPersistedStateFromAnotherInstance(t *testing.T) {
	store := &fakeStore{status: Status{Enabled: true, State: StateIdle}, has: true}
	c := NewForTest(true, "", &fakeRunner{}, store)
	store.mu.Lock()
	store.status = Status{Enabled: true, State: StateRunning, Trigger: TriggerScheduled}
	store.mu.Unlock()
	if got := c.Status(); got.State != StateRunning || got.Trigger != TriggerScheduled {
		t.Fatalf("refreshed status = %+v", got)
	}
}

func TestCoordinatorPersistsInterruptedRunningStatusAfterRestart(t *testing.T) {
	store := &fakeStore{status: Status{Enabled: true, State: StateRunning}, has: true}
	c := NewForTest(true, "", &fakeRunner{}, store)
	if got := c.Status(); got.State != StateFailed || got.Error == "" {
		t.Fatalf("recovered status = %+v", got)
	}
	if store.status.State != StateFailed || store.status.Error == "" {
		t.Fatalf("persisted recovered status = %+v", store.status)
	}
}

func TestStopPreventsNewRunsAndCancelsCurrentRun(t *testing.T) {
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	c := NewForTest(true, "", runner, &fakeStore{renewed: make(chan struct{}, 1)})
	if err := c.Trigger(context.Background(), TriggerManual); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Trigger(context.Background(), TriggerManual); !errors.Is(err, ErrStopping) {
		t.Fatalf("Trigger after Stop = %v, want ErrStopping", err)
	}
}

func TestScheduledTimeMatchSameDayDedupAndNextDay(t *testing.T) {
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	store := &fakeStore{renewed: make(chan struct{}, 1)}
	c := NewForTestSchedule(func(context.Context) Schedule { return Schedule{Enabled: true, Time: "03:15"} }, runner, store)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	c.RunScheduled(time.Date(2026, 8, 16, 3, 14, 0, 0, loc))
	if runner.runs != 0 {
		t.Fatalf("ran before configured minute: %d", runner.runs)
	}
	c.RunScheduled(time.Date(2026, 8, 16, 3, 15, 0, 0, loc))
	<-runner.started
	close(runner.release)
	waitState(t, c, StateSucceeded)
	c.RunScheduled(time.Date(2026, 8, 16, 3, 15, 30, 0, loc))
	if runner.runs != 1 {
		t.Fatalf("same-day runs = %d", runner.runs)
	}

	runner2 := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	c.runner = runner2
	c.RunScheduled(time.Date(2026, 8, 17, 3, 15, 0, 0, loc))
	<-runner2.started
	close(runner2.release)
	waitState(t, c, StateSucceeded)
	if runner2.runs != 1 {
		t.Fatalf("next-day runs = %d", runner2.runs)
	}
}

func TestScheduledDoesNotCatchUpOutsideConfiguredMinute(t *testing.T) {
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	c := NewForTestSchedule(func(context.Context) Schedule { return Schedule{Enabled: true, Time: "03:15"} }, runner, &fakeStore{})
	loc, _ := time.LoadLocation("Asia/Shanghai")
	c.RunScheduled(time.Date(2026, 8, 16, 4, 7, 0, 0, loc))
	if runner.runs != 0 {
		t.Fatalf("ran outside configured minute: %d", runner.runs)
	}
}

func TestScheduledFailureReleasesDayForRetryButSuccessKeepsDone(t *testing.T) {
	store := &fakeStore{}
	failed := &resultRunner{err: errors.New("async failure"), done: make(chan struct{})}
	c := NewForTestSchedule(func(context.Context) Schedule { return Schedule{Enabled: true, Time: "03:15"} }, failed, store)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 8, 16, 3, 15, 0, 0, loc)
	c.RunScheduled(now)
	<-failed.done
	waitState(t, c, StateFailed)

	succeeded := &resultRunner{done: make(chan struct{})}
	c.runner = succeeded
	c.RunScheduled(now.Add(30 * time.Second))
	<-succeeded.done
	waitState(t, c, StateSucceeded)
	c.RunScheduled(now.Add(45 * time.Second))
	if succeeded.runs != 1 {
		t.Fatalf("successful day repeated %d times", succeeded.runs)
	}
}

func TestExpiredRunningClaimCanRetryAfterServiceRestart(t *testing.T) {
	store := &fakeStore{claimed: map[string]bool{"2026-08-16": true}}
	// Simulate expiry of the running lease after the process that owned it stopped.
	delete(store.claimed, "2026-08-16")
	runner := &resultRunner{done: make(chan struct{})}
	c := NewForTestSchedule(func(context.Context) Schedule { return Schedule{Enabled: true, Time: "03:15"} }, runner, store)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	c.RunScheduled(time.Date(2026, 8, 16, 3, 15, 0, 0, loc))
	select {
	case <-runner.done:
	case <-time.After(time.Second):
		t.Fatal("expired running claim blocked restart catch-up")
	}
	waitState(t, c, StateSucceeded)
}

func TestScheduledInvalidTimeFallsBackToTwoAndDisabledSkips(t *testing.T) {
	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	enabled := true
	c := NewForTestSchedule(func(context.Context) Schedule { return Schedule{Enabled: enabled, Time: "invalid"} }, runner, &fakeStore{})
	loc, _ := time.LoadLocation("Asia/Shanghai")
	c.RunScheduled(time.Date(2026, 8, 16, 2, 0, 0, 0, loc))
	<-runner.started
	close(runner.release)
	waitState(t, c, StateSucceeded)
	enabled = false
	c.RunScheduled(time.Date(2026, 8, 17, 2, 0, 0, 0, loc))
	if runner.runs != 1 {
		t.Fatalf("disabled schedule runs = %d", runner.runs)
	}
}

func waitState(t *testing.T, c *Coordinator, want State) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.Status().State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("status = %+v, want state %s", c.Status(), want)
}
