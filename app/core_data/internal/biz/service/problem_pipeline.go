package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// pipelineControl 全局流水线控制（单进程）
var pipelineControl = &PipelineControl{}

const (
	pipelineConversationIndex  = "problem:pipeline:conversations"
	pipelineConversationPrefix = "problem:pipeline:conversation:"
	pipelineConversationTTL    = time.Hour
)

type ActiveJob struct {
	ProblemID    uint      `json:"problem_id"`
	Platform     string    `json:"platform"`
	ExternalID   string    `json:"external_id"`
	Title        string    `json:"title"`
	Stage        string    `json:"stage"` // fetch | analyze
	StartedAt    time.Time `json:"started_at"`
	LatestOutput string    `json:"latest_output,omitempty"`
	State        string    `json:"state"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
}

func (p *PipelineControl) TrackOutput(stage string, id uint, output string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if job := p.active[fmt.Sprintf("%s:%d", stage, id)]; job != nil {
		if len(output) > 12000 {
			output = output[len(output)-12000:]
		}
		job.LatestOutput = output
		p.persistJob(job)
	}
}

type PipelineControl struct {
	analyzePaused atomic.Bool
	fetchPaused   atomic.Bool
	mu            sync.RWMutex
	active        map[string]*ActiveJob // key: stage:id
	rdb           *redis.Client
}

func (p *PipelineControl) ConfigureRedis(rdb *redis.Client) {
	p.mu.Lock()
	p.rdb = rdb
	p.mu.Unlock()
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
		State:      "running",
	}
	p.persistJob(p.active[key])
}

func (p *PipelineControl) TrackEnd(stage string, id uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		return
	}
	key := fmt.Sprintf("%s:%d", stage, id)
	if job := p.active[key]; job != nil {
		job.State = "finished"
		job.EndedAt = time.Now()
		p.persistJob(job)
	}
	delete(p.active, key)
}

func pipelineConversationKey(stage string, id uint) string {
	return fmt.Sprintf("%s%s:%d", pipelineConversationPrefix, stage, id)
}

func (p *PipelineControl) persistJob(job *ActiveJob) {
	if p.rdb == nil || job == nil {
		return
	}
	b, err := json.Marshal(job)
	if err != nil {
		return
	}
	ctx := context.Background()
	key := pipelineConversationKey(job.Stage, job.ProblemID)
	ttl := pipelineConversationTTL
	if job.State == "running" {
		ttl = 24 * time.Hour
	}
	_ = p.rdb.Set(ctx, key, b, ttl).Err()
	_ = p.rdb.ZAdd(ctx, pipelineConversationIndex, redis.Z{Score: float64(time.Now().Unix()), Member: key}).Err()
	_ = p.rdb.Expire(ctx, pipelineConversationIndex, 24*time.Hour).Err()
}

func (p *PipelineControl) SnapshotConversations() []ActiveJob {
	p.mu.RLock()
	rdb := p.rdb
	p.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	ctx := context.Background()
	cutoff := time.Now().Add(-pipelineConversationTTL).Unix()
	keys, err := rdb.ZRangeByScore(ctx, pipelineConversationIndex, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", cutoff), Max: "+inf",
	}).Result()
	if err != nil || len(keys) == 0 {
		return nil
	}
	values, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil
	}
	out := make([]ActiveJob, 0, len(values))
	for i, value := range values {
		if value == nil {
			_ = rdb.ZRem(ctx, pipelineConversationIndex, keys[i])
			continue
		}
		var job ActiveJob
		if s, ok := value.(string); ok && json.Unmarshal([]byte(s), &job) == nil {
			out = append(out, job)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > 100 {
		out = out[:100]
	}
	return out
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
