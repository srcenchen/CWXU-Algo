package service

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// pipelineControl 全局流水线控制（单进程）
var pipelineControl = &PipelineControl{}

type ActiveJob struct {
	ProblemID  uint      `json:"problem_id"`
	Platform   string    `json:"platform"`
	ExternalID string    `json:"external_id"`
	Title      string    `json:"title"`
	Stage      string    `json:"stage"` // fetch | analyze
	StartedAt  time.Time `json:"started_at"`
}

type PipelineControl struct {
	analyzePaused atomic.Bool
	fetchPaused   atomic.Bool
	mu            sync.RWMutex
	active        map[string]*ActiveJob // key: stage:id
}

func (p *PipelineControl) IsAnalyzePaused() bool {
	return p.analyzePaused.Load()
}

func (p *PipelineControl) SetAnalyzePaused(v bool) {
	p.analyzePaused.Store(v)
}

func (p *PipelineControl) IsFetchPaused() bool {
	return p.fetchPaused.Load()
}

func (p *PipelineControl) SetFetchPaused(v bool) {
	p.fetchPaused.Store(v)
}

// IsPaused 兼容旧调用：仅表示 AI 是否暂停
func (p *PipelineControl) IsPaused() bool {
	return p.IsAnalyzePaused()
}

func (p *PipelineControl) SetPaused(v bool) {
	p.SetAnalyzePaused(v)
}

func (p *PipelineControl) TrackStart(stage string, id uint, platform, externalID, title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		p.active = map[string]*ActiveJob{}
	}
	key := fmt.Sprintf("%s:%d", stage, id)
	p.active[key] = &ActiveJob{
		ProblemID:  id,
		Platform:   platform,
		ExternalID: externalID,
		Title:      title,
		Stage:      stage,
		StartedAt:  time.Now(),
	}
}

func (p *PipelineControl) TrackEnd(stage string, id uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		return
	}
	delete(p.active, fmt.Sprintf("%s:%d", stage, id))
}

func (p *PipelineControl) SnapshotActive() []ActiveJob {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ActiveJob, 0, len(p.active))
	for _, j := range p.active {
		if j != nil {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			if out[i].Stage == out[j].Stage {
				return out[i].ProblemID < out[j].ProblemID
			}
			return out[i].Stage < out[j].Stage
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}
