package backupcoord

import (
	"context"
	"errors"
	"sync"
	"time"

	native "cwxu-algo/app/core_data/internal/backup"
	"cwxu-algo/app/core_data/internal/data"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
)

type Trigger string

const (
	TriggerManual    Trigger = "manual"
	TriggerScheduled Trigger = "scheduled"
)

type State string

const (
	StateIdle      State = "idle"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateDisabled  State = "disabled"
)

var (
	ErrDisabled       = errors.New("backup is disabled")
	ErrAlreadyRunning = errors.New("backup is already running")
	ErrStopping       = errors.New("backup coordinator is stopping")
)

type Status struct {
	Enabled       bool      `json:"enabled"`
	State         State     `json:"status"`
	Trigger       Trigger   `json:"trigger,omitempty"`
	Stage         string    `json:"stage,omitempty"`
	Message       string    `json:"message,omitempty"`
	Error         string    `json:"error,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	FinishedAt    time.Time `json:"finishedAt,omitempty"`
	ArchiveKey    string    `json:"archiveKey,omitempty"`
	ArchiveSize   int64     `json:"archiveSize,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	DatabaseCount int       `json:"databaseCount,omitempty"`
}

type runner interface {
	Run(context.Context) (native.Result, error)
}

type store interface {
	TryLock(context.Context, string, time.Duration) (bool, error)
	Renew(context.Context, string, time.Duration) (bool, error)
	Unlock(context.Context, string) error
	Load(context.Context) (Status, bool, error)
	Save(context.Context, Status) error
}

type Coordinator struct {
	runner     runner
	store      store
	lease      time.Duration
	renewEvery time.Duration

	mu       sync.RWMutex
	status   Status
	stopping bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewCoordinator(d *data.Data) *Coordinator {
	cfg, cfgErr := native.LoadConfig()
	redisStore := newRedisStore(d.RDB)
	if cfgErr != nil {
		return newCoordinator(false, cfgErr.Error(), nil, redisStore)
	}
	if !cfg.Enabled {
		return newCoordinator(false, "CWXU_BACKUP_ENABLED is false", nil, redisStore)
	}
	r, err := native.NewRunner(cfg, native.Dependencies{})
	if err != nil {
		return newCoordinator(false, err.Error(), nil, redisStore)
	}
	return newCoordinator(true, "", r, redisStore)
}

func NewForTest(enabled bool, reason string, r runner, s store) *Coordinator {
	return newCoordinator(enabled, reason, r, s)
}

func newCoordinator(enabled bool, reason string, r runner, s store) *Coordinator {
	c := &Coordinator{runner: r, store: s, lease: 30 * time.Minute, renewEvery: 10 * time.Minute}
	if !enabled {
		c.status = Status{Enabled: false, State: StateDisabled, Stage: string(StateDisabled), Error: reason, Message: reason}
		c.persistStartupStatus()
		return c
	}
	c.status = Status{Enabled: true, State: StateIdle, Stage: string(StateIdle)}
	if s != nil {
		if saved, ok, err := s.Load(context.Background()); err == nil && ok {
			saved.Enabled = true
			if saved.State == StateRunning {
				saved.State = StateFailed
				saved.Stage = string(StateFailed)
				saved.Error = "backup interrupted by service restart"
				saved.FinishedAt = time.Now().UTC()
			}
			c.status = saved
			if saved.State == StateFailed && saved.Error == "backup interrupted by service restart" {
				c.persistStartupStatus()
			}
		} else if err != nil {
			log.Warnf("backup: load persisted status: %v", err)
		}
	}
	return c
}

func (c *Coordinator) persistStartupStatus() {
	if c.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.store.Save(ctx, c.status); err != nil {
		log.Warnf("backup: persist startup status: %v", err)
	}
}

func (c *Coordinator) Trigger(ctx context.Context, trigger Trigger) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return ErrStopping
	}
	if !c.status.Enabled {
		return ErrDisabled
	}
	if c.status.State == StateRunning {
		return ErrAlreadyRunning
	}
	token := uuid.NewString()
	locked, err := c.store.TryLock(ctx, token, c.lease)
	if err != nil {
		return err
	}
	if !locked {
		return ErrAlreadyRunning
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	c.status = Status{
		Enabled: true, State: StateRunning, Trigger: trigger,
		Stage: string(StateRunning), Message: "backup is running", StartedAt: time.Now().UTC(),
	}
	if err := c.store.Save(ctx, c.status); err != nil {
		cancel()
		_ = c.store.Unlock(context.Background(), token)
		c.cancel, c.done = nil, nil
		c.status = Status{Enabled: true, State: StateIdle, Stage: string(StateIdle)}
		return err
	}
	go c.run(runCtx, token)
	return nil
}

func (c *Coordinator) run(ctx context.Context, token string) {
	done := c.done
	defer close(done)
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	go c.renewLease(renewCtx, token)
	result, err := c.runner.Run(ctx)
	stopRenew()

	c.mu.Lock()
	c.status.FinishedAt = time.Now().UTC()
	if err != nil {
		c.status.State = StateFailed
		c.status.Stage = string(StateFailed)
		c.status.Message = "backup failed"
		c.status.Error = err.Error()
	} else {
		c.status.State = StateSucceeded
		c.status.Stage = string(StateSucceeded)
		c.status.Message = "backup succeeded"
		c.status.ArchiveKey = result.ArchiveKey
		c.status.ArchiveSize = result.Size
		c.status.SHA256 = result.SHA256
		c.status.DatabaseCount = result.Databases
	}
	status := c.status
	c.cancel = nil
	c.mu.Unlock()
	if saveErr := c.store.Save(context.Background(), status); saveErr != nil {
		log.Errorf("backup: persist final status: %v", saveErr)
	}
	if unlockErr := c.store.Unlock(context.Background(), token); unlockErr != nil {
		log.Errorf("backup: release lock: %v", unlockErr)
	}
}

func (c *Coordinator) renewLease(ctx context.Context, token string) {
	ticker := time.NewTicker(c.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := c.store.Renew(ctx, token, c.lease)
			if err != nil || !ok {
				c.mu.RLock()
				cancel := c.cancel
				c.mu.RUnlock()
				if cancel != nil {
					cancel()
				}
				return
			}
		}
	}
}

func (c *Coordinator) Status() Status {
	c.mu.RLock()
	enabled := c.status.Enabled
	c.mu.RUnlock()
	if enabled && c.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		persisted, ok, err := c.store.Load(ctx)
		cancel()
		if err == nil && ok {
			persisted.Enabled = true
			c.mu.Lock()
			c.status = persisted
			c.mu.Unlock()
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Coordinator) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.stopping = true
	cancel, done := c.cancel, c.done
	if cancel != nil {
		cancel()
	}
	c.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Coordinator) RunScheduled() {
	if err := c.Trigger(context.Background(), TriggerScheduled); err != nil && !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrAlreadyRunning) && !errors.Is(err, ErrStopping) {
		log.Errorf("backup: scheduled trigger: %v", err)
	}
}

var ProviderSet = wire.NewSet(NewCoordinator)

type redisStatusStore struct {
	rdb *redis.Client
}

func newRedisStore(rdb *redis.Client) *redisStatusStore { return &redisStatusStore{rdb: rdb} }
