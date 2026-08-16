package opscompose

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"cwxu-algo/internal/opsrelease"
	"cwxu-algo/internal/opsroot"
)

type fakeRunner struct {
	outputs map[string]string
}

func (f *fakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return "", nil
}

func (f *fakeRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	return nil
}

func TestResolveLatest(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{}
	expected := map[string]string{}
	for i, service := range []string{"frontend", "gateway", "user", "core-data", "agent"} {
		digest := "sha256:" + strings.Repeat(string(rune('a'+i)), 64)
		outputs["docker manifest inspect --verbose "+opsrelease.Repository+":"+service+"-latest"] = `{"Descriptor":{"digest":"` + digest + `"}}`
		key := strings.ToUpper(strings.ReplaceAll(service, "-", "_")) + "_IMAGE"
		expected[key] = opsrelease.Repository + "@" + digest
	}
	compose := &Compose{Root: root, Run: &fakeRunner{outputs: outputs}}
	release, err := compose.ResolveLatest(context.Background())
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if len(release.Images) != 5 {
		t.Fatalf("expected 5 images, got %d", len(release.Images))
	}
	for key, want := range expected {
		if got := release.Images[key]; got != want {
			t.Errorf("%s: got %s, want %s", key, got, want)
		}
	}
}

func TestResolveLatestRejectsMissingDigest(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compose := &Compose{Root: root, Run: &fakeRunner{outputs: map[string]string{
		"docker manifest inspect --verbose " + opsrelease.Repository + ":frontend-latest": `{}`,
	}}}
	if _, err := compose.ResolveLatest(context.Background()); err == nil {
		t.Fatal("expected error when digest missing")
	}
}

type streamRunner struct{ ran int }

func (f *streamRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	return "", nil
}

func (f *streamRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	f.ran++
	return nil
}

func TestPullAndUpUseStreamingRun(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".env", "release.env", "compose.yaml"} {
		if err := os.WriteFile(root.Join(relative), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &streamRunner{}
	compose := &Compose{Root: root, Run: runner}
	if err := compose.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := compose.Up(context.Background(), "300"); err != nil {
		t.Fatal(err)
	}
	if runner.ran != 2 {
		t.Fatalf("Pull/Up must use Run (streaming), got %d streaming calls", runner.ran)
	}
}
