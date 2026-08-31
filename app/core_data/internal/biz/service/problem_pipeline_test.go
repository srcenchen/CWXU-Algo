package service

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestPipelineControlPersistsPromptOutputAndFinishedJob(t *testing.T) {
	mr := miniredis.RunT(t)
	p := &PipelineControl{rdb: redis.NewClient(&redis.Options{Addr: mr.Addr()})}

	p.TrackStart("analyze", 65775, "AtCoder", "arc228_a", "A - Row and Col swap")
	p.TrackPrompt("analyze", 65775, "system prompt\n\nuser prompt")
	p.TrackReasoning("analyze", 65775, "raw reasoning")
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
	if job.ReasoningOutput != "raw reasoning" {
		t.Fatalf("reasoning output = %q", job.ReasoningOutput)
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

func TestPipelineControlSanitizesInvalidUTF8Output(t *testing.T) {
	p := &PipelineControl{}
	p.TrackStart("analyze", 1, "AtCoder", "abc", "题目")
	p.TrackOutput("analyze", 1, "正常\xff输出")
	p.TrackReasoning("analyze", 1, "思考\xfe过程")

	jobs := p.SnapshotActive()
	if len(jobs) != 1 {
		t.Fatalf("got %d active jobs, want 1", len(jobs))
	}
	if !utf8.ValidString(jobs[0].LatestOutput) {
		t.Fatalf("latest output is invalid UTF-8: %q", jobs[0].LatestOutput)
	}
	if !utf8.ValidString(jobs[0].ReasoningOutput) {
		t.Fatalf("reasoning output is invalid UTF-8: %q", jobs[0].ReasoningOutput)
	}
}

func TestPipelineControlKeepsTruncatedOutputValidUTF8(t *testing.T) {
	p := &PipelineControl{}
	p.TrackStart("analyze", 1, "AtCoder", "abc", "题目")
	p.TrackOutput("analyze", 1, "前缀"+repeatString("中", 5000))

	jobs := p.SnapshotActive()
	if len(jobs) != 1 {
		t.Fatalf("got %d active jobs, want 1", len(jobs))
	}
	if !utf8.ValidString(jobs[0].LatestOutput) {
		t.Fatalf("truncated output is invalid UTF-8")
	}
}

func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
