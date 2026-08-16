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
	mu      sync.Mutex
	locked  bool
	status  Status
	has     bool
	renewed chan struct{}
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
	if status.Enabled || status.State != StateDisabled || status.Error != "missing backup configuration" {
		t.Fatalf("unexpected disabled status: %+v", status)
	}
	if !store.has || store.status.State != StateDisabled {
		t.Fatalf("disabled status was not persisted: %+v", store.status)
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
