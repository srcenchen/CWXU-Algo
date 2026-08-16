package backup

import (
	"context"
	"sync"
	"time"
)

// Stage is the externally observable lifecycle stage of a Task.
type Stage string

const (
	StageIdle      Stage = "idle"
	StageRunning   Stage = "running"
	StageSucceeded Stage = "succeeded"
	StageFailed    Stage = "failed"
)

// Status is an in-process snapshot; distributed locking/status can wrap Task later.
type Status struct {
	Stage      Stage     `json:"stage"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Error      string    `json:"error,omitempty"`
	LastResult Result    `json:"lastResult,omitempty"`
}

// RunFunc is implemented by Runner and permits isolated Task tests.
type RunFunc interface {
	Run(context.Context) (Result, error)
}

// Task offers the same synchronous API to HTTP-triggered goroutines and cron jobs.
type Task struct {
	runner RunFunc
	mu     sync.RWMutex
	status Status
}

func NewTask(runner RunFunc) *Task {
	return &Task{runner: runner, status: Status{Stage: StageIdle}}
}

func (t *Task) Run(ctx context.Context) (Result, error) {
	t.mu.Lock()
	if t.status.Stage == StageRunning {
		t.mu.Unlock()
		return Result{}, ErrAlreadyRunning
	}
	t.status = Status{Stage: StageRunning, StartedAt: time.Now().UTC()}
	t.mu.Unlock()
	result, err := t.runner.Run(ctx)
	t.mu.Lock()
	t.status.FinishedAt = time.Now().UTC()
	if err != nil {
		t.status.Stage = StageFailed
		t.status.Error = err.Error()
	} else {
		t.status.Stage = StageSucceeded
		t.status.LastResult = result
	}
	t.mu.Unlock()
	return result, err
}

func (t *Task) Status() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}
