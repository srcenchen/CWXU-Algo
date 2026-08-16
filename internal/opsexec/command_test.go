package opsexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
)

type fake struct {
	index int
	calls [][]string
}

func (f *fake) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return "fake output", nil
}

func (f *fake) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func TestSanitizeRedactsSecrets(t *testing.T) {
	out := Sanitize("supersecret", "failed with supersecret inside")
	if bytes.Contains([]byte(out), []byte("supersecret")) {
		t.Fatal("expected secret to be redacted")
	}
	if out != "failed with <redacted> inside" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestQuietRun(t *testing.T) {
	if err := QuietRun(context.Background(), &fake{}, "docker", "version"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShowError(t *testing.T) {
	runner := &fake{}
	output, err := runner.CombinedOutput(context.Background(), "docker", "compose", "up")
	if err != nil {
		t.Fatal(err)
	}
	if err := ShowError(runner, fmt.Errorf("boom"), output); err == nil {
		t.Fatal("expected wrapped error")
	}
	if err := ShowError(runner, nil, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
