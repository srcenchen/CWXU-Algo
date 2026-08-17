package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsdata"
	"cwxu-algo/internal/opsrelease"
	"cwxu-algo/internal/opsroot"
)

func TestRuntimeUninstallPreservesEnvFile(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	envContent := "GOALGO_ROOT=" + root.Path + "\nGOALGO_HTTP_PORT=8988\n"
	if err := os.WriteFile(root.Join(".env"), []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("compose.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := &opscompose.Compose{Root: root, Run: &transactionRunner{}}
	if code := runtimeUninstall(context.Background(), compose, []string{"--yes"}); code != 0 {
		t.Fatalf("uninstall exit code = %d", code)
	}
	got, err := os.ReadFile(root.Join(".env"))
	if err != nil {
		t.Fatalf("preserved .env not readable: %v", err)
	}
	if string(got) != envContent {
		t.Fatalf("preserved .env changed: %q", got)
	}
}

func TestRuntimeUninstallWithoutEnvStillSucceeds(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compose := &opscompose.Compose{Root: root, Run: &transactionRunner{}}
	if code := runtimeUninstall(context.Background(), compose, []string{"--yes"}); code != 0 {
		t.Fatalf("uninstall exit code = %d", code)
	}
	if _, err := os.Stat(root.Path); !os.IsNotExist(err) {
		t.Fatalf("root dir should be removed, got %v", err)
	}
}

func TestDotenvValueOrDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("GOALGO_ROOT=/a/b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dotenvValueOrDefault(path, "GOALGO_ROOT"); got != "/a/b" {
		t.Fatalf("got %q", got)
	}
	if got := dotenvValueOrDefault(path, "NOPE"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := dotenvValueOrDefault(filepath.Join(dir, "missing.env"), "GOALGO_ROOT"); got != "" {
		t.Fatalf("expected empty for missing file, got %q", got)
	}
}

type recordingRunner struct {
	output string
	calls  [][]string
}

func TestRuntimeUpgradePullsLatestTagsAndStoresResolvedDigests(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "ops.data.json")
	t.Setenv("GOALGO_OPS_DATA_FILE", dataPath)
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("compose.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testRelease('a').WriteFile(root.Join("release.env")); err != nil {
		t.Fatal(err)
	}

	runner := &upgradeRunner{}
	compose := &opscompose.Compose{Root: root, Run: runner, SmokeCheck: func(context.Context) error { return nil }}
	if code := runtimeUpgrade(context.Background(), compose); code != 0 {
		t.Fatalf("upgrade exit code = %d", code)
	}

	active, err := opsrelease.ParseFile(root.Join("release.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !releaseSame(active, opsrelease.LatestTagRelease()) {
		t.Fatalf("upgrade must keep latest tags in release.env, got %v", active.Images)
	}
	data, err := opsdata.Load()
	if err != nil {
		t.Fatal(err)
	}
	for i, service := range []string{"frontend", "gateway", "user", "core-data", "agent"} {
		key := strings.ToUpper(strings.ReplaceAll(service, "-", "_")) + "_IMAGE"
		want := opsrelease.Repository + "@sha256:" + strings.Repeat(string(rune('b'+i)), 64)
		if got := data.Deploy.LastDigests[key]; got != want {
			t.Errorf("stored %s = %q, want %q", key, got, want)
		}
	}
}

type upgradeRunner struct{}

func (r *upgradeRunner) CombinedOutput(_ context.Context, name string, args ...string) (string, error) {
	joined := name + " " + strings.Join(args, " ")
	for i, service := range []string{"frontend", "gateway", "user", "core-data", "agent"} {
		if joined == "docker manifest inspect --verbose "+opsrelease.Repository+":"+service+"-latest" {
			return `{"Descriptor":{"digest":"sha256:` + strings.Repeat(string(rune('b'+i)), 64) + `"}}`, nil
		}
	}
	if strings.Contains(joined, " ps --format json") {
		services := []string{"frontend", "gateway", "user", "core-data", "agent", "postgres", "redis", "rabbitmq", "consul", "nginx"}
		parts := make([]string, 0, len(services))
		for _, service := range services {
			parts = append(parts, fmt.Sprintf(`{"Service":%q,"State":"running","Health":"healthy"}`, service))
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	}
	return "", nil
}

func (r *upgradeRunner) Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error {
	return nil
}

func TestApplyReleaseFailureRestoresFilesAndRunningRelease(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("compose.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	current := testRelease('a')
	previous := testRelease('b')
	candidate := testRelease('c')
	if err := current.WriteFile(root.Join("release.env")); err != nil {
		t.Fatal(err)
	}
	if err := previous.WriteFile(root.Join("release.previous.env")); err != nil {
		t.Fatal(err)
	}
	runner := &transactionRunner{runErrors: []error{nil, errors.New("candidate up failed"), nil, nil}}
	compose := &opscompose.Compose{Root: root, Run: runner, SmokeCheck: func(context.Context) error { return nil }}
	err = applyRelease(context.Background(), compose, candidate, current, previous)
	if err == nil || !strings.Contains(err.Error(), "candidate up failed") {
		t.Fatalf("expected candidate error, got %v", err)
	}
	active, _ := opsrelease.ParseFile(root.Join("release.env"))
	oldPrevious, _ := opsrelease.ParseFile(root.Join("release.previous.env"))
	if !releaseSame(active, current) || !releaseSame(oldPrevious, previous) {
		t.Fatalf("files not restored: active=%v previous=%v", active.Images, oldPrevious.Images)
	}
	if runner.upCalls != 2 {
		t.Fatalf("old release was not started after failure, up calls=%d", runner.upCalls)
	}
}

func TestRestoreRunningReleaseRestoresFilesAndContainers(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("compose.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	current := testRelease('a')
	previous := testRelease('b')
	if err := testRelease('c').WriteFile(root.Join("release.env")); err != nil {
		t.Fatal(err)
	}
	if err := current.WriteFile(root.Join("release.previous.env")); err != nil {
		t.Fatal(err)
	}
	runner := &transactionRunner{runErrors: []error{nil, nil}}
	compose := &opscompose.Compose{Root: root, Run: runner, SmokeCheck: func(context.Context) error { return nil }}
	if err := restoreRunningRelease(context.Background(), compose, current, previous); err != nil {
		t.Fatal(err)
	}
	active, _ := opsrelease.ParseFile(root.Join("release.env"))
	oldPrevious, _ := opsrelease.ParseFile(root.Join("release.previous.env"))
	if !releaseSame(active, current) || !releaseSame(oldPrevious, previous) || runner.upCalls != 1 {
		t.Fatalf("release not restored: active=%v previous=%v up=%d", active.Images, oldPrevious.Images, runner.upCalls)
	}
}

func TestApplyReleaseRollbackUsesLiveContextAfterMainContextCanceled(t *testing.T) {
	root, _ := opsroot.Resolve(t.TempDir())
	_ = os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600)
	_ = os.WriteFile(root.Join("compose.yaml"), []byte("x"), 0o600)
	current, previous, candidate := testRelease('a'), testRelease('b'), testRelease('c')
	_ = current.WriteFile(root.Join("release.env"))
	_ = previous.WriteFile(root.Join("release.previous.env"))
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelThenRequireLiveRunner{cancel: cancel}
	compose := &opscompose.Compose{Root: root, Run: runner, SmokeCheck: func(ctx context.Context) error { return ctx.Err() }}
	err := applyRelease(ctx, compose, candidate, current, previous)
	if err == nil || runner.liveRollbackCalls == 0 {
		t.Fatalf("rollback did not run with independent context: err=%v live=%d", err, runner.liveRollbackCalls)
	}
}

type cancelThenRequireLiveRunner struct {
	cancel            context.CancelFunc
	calls             int
	liveRollbackCalls int
}

func (r *cancelThenRequireLiveRunner) CombinedOutput(ctx context.Context, _ string, _ ...string) (string, error) {
	if ctx.Err() == nil && r.calls > 0 {
		r.liveRollbackCalls++
	}
	return "", ctx.Err()
}

func (r *cancelThenRequireLiveRunner) Run(ctx context.Context, _ io.Reader, _, _ io.Writer, _ string, _ ...string) error {
	r.calls++
	if r.calls == 1 {
		r.cancel()
		return context.Canceled
	}
	if ctx.Err() == nil {
		r.liveRollbackCalls++
	}
	return ctx.Err()
}

func testRelease(ch byte) *opsrelease.Release {
	images := map[string]string{}
	for _, key := range []string{"FRONTEND_IMAGE", "GATEWAY_IMAGE", "USER_IMAGE", "CORE_DATA_IMAGE", "AGENT_IMAGE"} {
		images[key] = opsrelease.Repository + "@sha256:" + strings.Repeat(string(ch), 64)
	}
	return &opsrelease.Release{Images: images}
}

type transactionRunner struct {
	runErrors []error
	runIndex  int
	upCalls   int
}

func (r *transactionRunner) CombinedOutput(_ context.Context, _ string, args ...string) (string, error) {
	if len(args) > 0 && args[len(args)-3] == "ps" {
		services := []string{"frontend", "gateway", "user", "core-data", "agent", "postgres", "redis", "rabbitmq", "consul", "nginx"}
		parts := make([]string, 0, len(services))
		for _, service := range services {
			parts = append(parts, fmt.Sprintf(`{"Service":%q,"State":"running","Health":"healthy"}`, service))
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	}
	return "", nil
}

func (r *transactionRunner) Run(_ context.Context, _ io.Reader, _, _ io.Writer, _ string, args ...string) error {
	for _, arg := range args {
		if arg == "up" {
			r.upCalls++
		}
	}
	if r.runIndex >= len(r.runErrors) {
		return nil
	}
	err := r.runErrors[r.runIndex]
	r.runIndex++
	return err
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

func TestExtractRootRemovesFlagOnce(t *testing.T) {
	root, rest, err := extractRoot([]string{"candidate.env", "--root", "/srv/goalgo", "--tail", "20"})
	if err != nil {
		t.Fatal(err)
	}
	if root != "/srv/goalgo" || !reflect.DeepEqual(rest, []string{"candidate.env", "--tail", "20"}) {
		t.Fatalf("root=%q rest=%v", root, rest)
	}
	if _, _, err := extractRoot([]string{"--root=a", "--root=b"}); err == nil {
		t.Fatal("duplicate --root must fail")
	}
}

func TestRuntimeLogsStreamsAndDoesNotForwardRoot(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		".env":         "GOALGO_ROOT=" + root.Path + "\n",
		"release.env":  "x\n",
		"compose.yaml": "x\n",
	} {
		if err := os.WriteFile(filepath.Join(root.Path, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &streamCaptureRunner{text: "live log\n"}
	compose := &opscompose.Compose{Root: root, Run: runner}
	if code := runtimeLogs(context.Background(), compose, []string{"--tail", "1"}); code != 0 {
		t.Fatalf("logs exit %d", code)
	}
	if !strings.Contains(runner.stdout.String(), "live log") {
		t.Fatalf("logs discarded output: %q", runner.stdout.String())
	}
	if strings.Contains(strings.Join(runner.calls[0], " "), "--root") {
		t.Fatalf("root leaked to compose: %v", runner.calls[0])
	}
}

type streamCaptureRunner struct {
	text   string
	stdout bytes.Buffer
	calls  [][]string
}

func (r *streamCaptureRunner) CombinedOutput(context.Context, string, ...string) (string, error) {
	return "", nil
}

func (r *streamCaptureRunner) Run(_ context.Context, _ io.Reader, stdout, _ io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	_, _ = io.WriteString(stdout, r.text)
	_, _ = r.stdout.WriteString(r.text)
	return nil
}

func TestLockPathIsGlobalForDifferentRoots(t *testing.T) {
	t.Setenv("GOALGO_LOCK_FILE", "")
	a, _ := opsroot.Resolve(t.TempDir())
	b, _ := opsroot.Resolve(t.TempDir())
	if lockPath(a) != "/run/lock/goalgo-ops.lock" || lockPath(a) != lockPath(b) {
		t.Fatalf("locks differ: %q %q", lockPath(a), lockPath(b))
	}
}
