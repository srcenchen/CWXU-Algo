package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cwxu-algo/internal/opscompose"
)

type versionRunner struct{ version string }

func (r versionRunner) CombinedOutput(_ context.Context, name string, args ...string) (string, error) {
	if name == "pg_restore" && len(args) == 1 && args[0] == "--version" {
		return r.version, nil
	}
	return "ok", nil
}
func (versionRunner) Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error {
	return nil
}

func TestRequireBackupToolsAcceptsPgRestoreNewerThan18(t *testing.T) {
	if err := requireBackupTools(context.Background(), versionRunner{version: "pg_restore (PostgreSQL) 19.2"}); err != nil {
		t.Fatalf("pg_restore >=18 rejected: %v", err)
	}
	if err := requireBackupTools(context.Background(), versionRunner{version: "pg_restore (PostgreSQL) 17.9"}); err == nil {
		t.Fatal("pg_restore 17 accepted")
	}
}

func TestRestoreConfirmationOnlyRequiredForExistingData(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data", "postgres")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateRestoreConfirmation(dataDir, false, ""); err != nil {
		t.Fatalf("empty instance should not require replacement: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "PG_VERSION"), []byte("18"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		replace bool
		confirm string
	}{
		{false, ""},
		{true, ""},
		{false, "RESTORE"},
		{true, "wrong"},
	} {
		if err := validateRestoreConfirmation(dataDir, tc.replace, tc.confirm); err == nil {
			t.Fatalf("existing data accepted replace=%v confirm=%q", tc.replace, tc.confirm)
		}
	}
	if err := validateRestoreConfirmation(dataDir, true, "RESTORE"); err != nil {
		t.Fatalf("valid confirmation rejected: %v", err)
	}
}

func TestRollbackPostgresVolumeReportsCombinedFailure(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "postgres")
	backupDir := filepath.Join(root, "postgres.bak")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "old"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rollbackPostgresVolume(dataDir, backupDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "old")); err != nil {
		t.Fatalf("old volume not restored: %v", err)
	}
}

func TestRecoverRestoreCombinesCauseWhenOldServicesFail(t *testing.T) {
	root := mustRoot(t, t.TempDir())
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := root.Join("data", "postgres")
	backupDir := dataDir + ".bak"
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &transactionRunner{runErrors: []error{errors.New("old up failed")}}
	err := recoverRestore(&opscompose.Compose{Root: root, Run: runner}, dataDir, backupDir, true, true, errors.New("restore failed"))
	if err == nil || !strings.Contains(err.Error(), "restore failed") || !strings.Contains(err.Error(), "old up failed") {
		t.Fatalf("errors not combined: %v", err)
	}
}

func TestRecoverRestoreContinuesServiceRecoveryWhenRollbackPostgresStopFails(t *testing.T) {
	compose, runner := newRestoreTransactionFake(t)
	runner.stopErrors["postgres"] = errors.New("rollback postgres stop failed")
	dataDir, backupDir := restoreFakeDataDirs(t, compose)
	if err := os.WriteFile(filepath.Join(dataDir, "new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "old"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := recoverRestore(compose, dataDir, backupDir, true, true, errors.New("restore failed"))

	if err == nil || !strings.Contains(err.Error(), "restore failed") || !strings.Contains(err.Error(), "rollback postgres stop failed") {
		t.Fatalf("restore and rollback failures not combined: %v", err)
	}
	if runner.upCalls != 1 || runner.healthCalls != 1 || runner.smokeCalls != 2 {
		t.Fatalf("service recovery stopped early: up=%d health=%d smoke=%d", runner.upCalls, runner.healthCalls, runner.smokeCalls)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "old")); err != nil {
		t.Fatalf("old volume not restored: %v", err)
	}
}

func TestPrepareRestoreRecoversOriginalReleaseWhenApplicationStopFails(t *testing.T) {
	compose, runner := newRestoreTransactionFake(t)
	runner.stopErrors["user"] = errors.New("user stop failed")
	renameCalls := 0
	dataDir, backupDir := restoreFakeDataDirs(t, compose)

	_, err := prepareRestore(context.Background(), compose, dataDir, backupDir, func(string, string) error {
		renameCalls++
		return nil
	})

	assertRestorePreparationRecovered(t, err, runner, "user stop failed")
	if renameCalls != 0 {
		t.Fatalf("data volume changed after application stop failure: rename calls=%d", renameCalls)
	}
}

func TestPrepareRestoreRecoversOriginalReleaseWhenPostgresStopFails(t *testing.T) {
	compose, runner := newRestoreTransactionFake(t)
	runner.stopErrors["postgres"] = errors.New("postgres stop failed")
	renameCalls := 0
	dataDir, backupDir := restoreFakeDataDirs(t, compose)

	_, err := prepareRestore(context.Background(), compose, dataDir, backupDir, func(string, string) error {
		renameCalls++
		return nil
	})

	assertRestorePreparationRecovered(t, err, runner, "postgres stop failed")
	if renameCalls != 0 {
		t.Fatalf("data volume changed after postgres stop failure: rename calls=%d", renameCalls)
	}
}

func TestPrepareRestoreRecoversOriginalReleaseWhenDataRenameFails(t *testing.T) {
	compose, runner := newRestoreTransactionFake(t)
	dataDir, backupDir := restoreFakeDataDirs(t, compose)

	_, err := prepareRestore(context.Background(), compose, dataDir, backupDir, func(string, string) error {
		return errors.New("rename failed")
	})

	assertRestorePreparationRecovered(t, err, runner, "rename failed")
}

func TestPrepareRestoreRollbackUsesIndependentContextAfterCancellation(t *testing.T) {
	compose, runner := newRestoreTransactionFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	runner.cancelOnStop = "user"
	runner.cancel = cancel
	dataDir, backupDir := restoreFakeDataDirs(t, compose)

	_, err := prepareRestore(ctx, compose, dataDir, backupDir, func(string, string) error {
		t.Fatal("data volume changed after canceled stop")
		return nil
	})

	assertRestorePreparationRecovered(t, err, runner, context.Canceled.Error())
	if runner.rollbackCanceled {
		t.Fatal("rollback inherited the canceled restore context")
	}
}

func restoreFakeDataDirs(t *testing.T, compose *opscompose.Compose) (string, string) {
	t.Helper()
	dataDir := compose.Root.Join("data", "postgres")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dataDir, dataDir + ".bak"
}

type restoreTransactionFake struct {
	stopErrors       map[string]error
	cancelOnStop     string
	cancel           context.CancelFunc
	upCalls          int
	healthCalls      int
	smokeCalls       int
	rollbackCanceled bool
}

func newRestoreTransactionFake(t *testing.T) (*opscompose.Compose, *restoreTransactionFake) {
	t.Helper()
	root := mustRoot(t, t.TempDir())
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &restoreTransactionFake{stopErrors: map[string]error{}}
	compose := &opscompose.Compose{Root: root, Run: runner, SmokeCheck: func(ctx context.Context) error {
		runner.smokeCalls++
		if ctx.Err() != nil {
			runner.rollbackCanceled = true
		}
		return ctx.Err()
	}}
	return compose, runner
}

func (r *restoreTransactionFake) CombinedOutput(ctx context.Context, _ string, args ...string) (string, error) {
	if service, ok := composeStopService(args); ok {
		if service == r.cancelOnStop {
			r.cancel()
			return "", ctx.Err()
		}
		return "", r.stopErrors[service]
	}
	if containsArgument(args, "ps") {
		r.healthCalls++
		if ctx.Err() != nil {
			r.rollbackCanceled = true
		}
		services := []string{"frontend", "gateway", "user", "core-data", "agent", "postgres", "redis", "rabbitmq", "consul", "nginx"}
		states := make([]string, 0, len(services))
		for _, service := range services {
			states = append(states, `{"Service":"`+service+`","State":"running","Health":"healthy"}`)
		}
		return "[" + strings.Join(states, ",") + "]", ctx.Err()
	}
	return "", ctx.Err()
}

func (r *restoreTransactionFake) Run(ctx context.Context, _ io.Reader, _, _ io.Writer, _ string, args ...string) error {
	if containsArgument(args, "up") {
		r.upCalls++
		if ctx.Err() != nil {
			r.rollbackCanceled = true
		}
	}
	return ctx.Err()
}

func composeStopService(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "stop" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func containsArgument(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func assertRestorePreparationRecovered(t *testing.T, err error, runner *restoreTransactionFake, cause string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), cause) {
		t.Fatalf("original failure missing: %v", err)
	}
	if runner.upCalls != 1 || runner.healthCalls != 1 || runner.smokeCalls != 2 {
		t.Fatalf("original release not fully recovered: up=%d health=%d smoke=%d", runner.upCalls, runner.healthCalls, runner.smokeCalls)
	}
}
