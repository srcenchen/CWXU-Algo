package opsexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
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

func TestRealEnvOverlayOverridesExistingAndKeepsOthers(t *testing.T) {
	t.Setenv("GOALGO_ROOT", "")
	t.Setenv("KEEP_ME", "stay")
	env := mergedEnvForTest(map[string]string{"GOALGO_ROOT": "/custom/root"})
	var goalgo, keep string
	for _, kv := range env {
		switch {
		case kv == "GOALGO_ROOT=/custom/root":
			goalgo = kv
		case kv == "KEEP_ME=stay":
			keep = kv
		}
	}
	if goalgo != "GOALGO_ROOT=/custom/root" {
		t.Fatalf("GOALGO_ROOT not injected: %v", env)
	}
	if keep != "KEEP_ME=stay" {
		t.Fatalf("unrelated env dropped: %v", env)
	}
	var count int
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOALGO_ROOT=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one GOALGO_ROOT, got %d: %v", count, env)
	}
}

func mergedEnvForTest(overlay map[string]string) []string {
	return (Real{Env: overlay}).mergedEnv()
}
