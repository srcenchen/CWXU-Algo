package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

type recordingRunner struct {
	output string
	calls  [][]string
}

func (r *recordingRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.output, nil
}

func (r *recordingRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil
}

func TestRemoveGoalgoImagesFiltersRefs(t *testing.T) {
	runner := &recordingRunner{output: "registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo:frontend-latest\n" +
		"registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo:agent-sha-abc\n" +
		"goalgo-custom:ci\n" +
		"nginxinc/nginx-unprivileged:1.29.1-alpine\n"}
	if err := removeGoalgoImages(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 docker calls, got %d", len(runner.calls))
	}
	rmi := runner.calls[1]
	if rmi[0] != "docker" || rmi[1] != "rmi" {
		t.Fatalf("unexpected rmi call: %v", rmi)
	}
	joined := strings.Join(rmi, " ")
	if !strings.Contains(joined, "goalgo:frontend-latest") ||
		!strings.Contains(joined, "goalgo:agent-sha-abc") ||
		!strings.Contains(joined, "goalgo-custom:ci") {
		t.Fatalf("rmi missing goalgo refs: %v", rmi)
	}
	if strings.Contains(joined, "nginxinc") {
		t.Fatalf("infra images must not be removed: %v", rmi)
	}
}
