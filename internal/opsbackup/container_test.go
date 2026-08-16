package opsbackup

import (
	"context"
	"io"
	"strings"
	"testing"
)

type recordingCommand struct {
	calls [][]string
}

func (r *recordingCommand) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return "", nil
}

func (r *recordingCommand) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil
}

func TestContainerToolRunnerRewritesPgRestore(t *testing.T) {
	inner := &recordingCommand{}
	runner := ContainerToolRunner{Inner: inner, Image: "repo:core-data-latest", WorkDir: "/opt/goalgo/restore/verify-1"}
	if _, err := runner.CombinedOutput(context.Background(), "pg_restore", "--list", "/opt/goalgo/restore/verify-1/database-001.dump"); err != nil {
		t.Fatal(err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(inner.calls))
	}
	got := strings.Join(inner.calls[0], " ")
	if !strings.Contains(got, "docker run --rm --user root -v /opt/goalgo/restore/verify-1:/opt/goalgo/restore/verify-1 --entrypoint pg_restore repo:core-data-latest --list") {
		t.Fatalf("unexpected docker call: %s", got)
	}
}

func TestContainerToolRunnerLeavesOtherCommandsAlone(t *testing.T) {
	inner := &recordingCommand{}
	runner := ContainerToolRunner{Inner: inner, Image: "repo:core-data-latest", WorkDir: "/w"}
	if _, err := runner.CombinedOutput(context.Background(), "tar", "xf", "x"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inner.calls[0], " "); got != "tar xf x" {
		t.Fatalf("unexpected passthrough: %s", got)
	}
}
