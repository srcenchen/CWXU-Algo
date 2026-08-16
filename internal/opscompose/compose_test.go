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

func TestDockerCommandsRejectMismatchedEnvRoot(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT=/different/root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &streamRunner{}
	compose := &Compose{Root: root, Run: runner}
	if err := compose.Pull(context.Background()); err == nil || !strings.Contains(err.Error(), "GOALGO_ROOT") {
		t.Fatalf("expected root mismatch, got %v", err)
	}
	if runner.ran != 0 {
		t.Fatal("docker must not run with a split root")
	}
}

func TestHealthRequiresEveryExpectedService(t *testing.T) {
	root, _ := opsroot.Resolve(t.TempDir())
	output := `[{"Service":"postgres","State":"running","Health":"healthy"}]`
	compose := &Compose{Root: root, Run: &fakeRunner{outputs: map[string]string{
		"docker compose --env-file " + root.Join(".env") + " --env-file " + root.Join("release.env") + " -f " + root.Join("compose.yaml") + " ps --format json": output,
	}}}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compose.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "frontend") {
		t.Fatalf("expected missing service error, got %v", err)
	}
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
	for _, relative := range []string{"release.env", "compose.yaml"} {
		if err := os.WriteFile(root.Join(relative), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
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

func TestRunServiceStreams(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"release.env", "compose.yaml"} {
		if err := os.WriteFile(root.Join(relative), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &streamRunner{}
	compose := &Compose{Root: root, Run: runner}
	if _, err := compose.RunService(context.Background(), "user", []string{"--user", "root"}, "--admin-config", "/run/admin.env"); err != nil {
		t.Fatal(err)
	}
	if runner.ran != 1 {
		t.Fatalf("RunService must use Run (streaming), got %d streaming calls", runner.ran)
	}
}
