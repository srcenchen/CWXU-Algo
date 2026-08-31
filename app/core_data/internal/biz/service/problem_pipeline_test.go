package service

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestPipelineControlPersistsPromptOutputAndFinishedJob(t *testing.T) {
	mr := miniredis.RunT(t)
	p := &PipelineControl{rdb: redis.NewClient(&redis.Options{Addr: mr.Addr()})}

	p.TrackStart("analyze", 65775, "AtCoder", "arc228_a", "A - Row and Col swap")
	p.TrackPrompt("analyze", 65775, "system prompt\n\nuser prompt")
	p.TrackOutput("analyze", 65775, "partial output")
	p.TrackEnd("analyze", 65775)

	jobs := p.SnapshotConversations()
	if len(jobs) != 1 {
		t.Fatalf("got %d conversation jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.Prompt != "system prompt\n\nuser prompt" {
		t.Fatalf("prompt = %q", job.Prompt)
	}
	if job.LatestOutput != "partial output" {
		t.Fatalf("latest output = %q", job.LatestOutput)
	}
	if job.State != "finished" || job.EndedAt.IsZero() {
		t.Fatalf("job state = %q, ended_at = %v", job.State, job.EndedAt)
	}

	ttl, err := p.rdb.TTL(context.Background(), pipelineConversationKey("analyze", 65775)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > time.Hour {
		t.Fatalf("finished job ttl = %v, want <= 1 hour", ttl)
	}
}
