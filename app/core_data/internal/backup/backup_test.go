package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadConfigReportsMissingStaticConfigurationWithoutEnabledEnv(t *testing.T) {
	t.Setenv("CWXU_BACKUP_PG_DSN", "")
	cfg, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "CWXU_BACKUP_PG_DSN") {
		t.Fatalf("invalid config = (%+v, %v), want disabled explicit DSN error", cfg, err)
	}
}

func TestLoadConfigAcceptsValidKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "backup.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"CWXU_BACKUP_PG_DSN":              "postgres://user:pass@db/core?sslmode=disable",
		"CWXU_BACKUP_ENCRYPTION_KEY_FILE": keyFile,
		"CWXU_BACKUP_WORK_DIR":            t.TempDir(),
	} {
		t.Setenv(key, value)
	}
	cfg, err := LoadConfig()
	if err != nil || len(cfg.EncryptionKey) != 32 || cfg.WorkDir == "" {
		t.Fatalf("valid config = (%+v, %v)", cfg, err)
	}
}

func TestLoadConfigRejectsWorkspaceFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "backup.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	workFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(workFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"CWXU_BACKUP_PG_DSN":              "postgres://user:pass@db/core",
		"CWXU_BACKUP_ENCRYPTION_KEY_FILE": keyFile,
		"CWXU_BACKUP_WORK_DIR":            workFile,
	} {
		t.Setenv(key, value)
	}
	cfg, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Fatalf("workspace file config = (%+v, %v), want explicit disabled error", cfg, err)
	}
}

type commandCall struct {
	name string
	args []string
	env  []string
}

type fakeCommands struct {
	mu    sync.Mutex
	calls []commandCall
}

func (f *fakeCommands) Run(ctx context.Context, cmd Command) error {
	f.mu.Lock()
	f.calls = append(f.calls, commandCall{name: cmd.Name, args: append([]string(nil), cmd.Args...), env: append([]string(nil), cmd.Env...)})
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	switch cmd.Name {
	case "psql":
		_, _ = io.WriteString(cmd.Stdout, "postgres\napp\nname with / and ?\n")
	case "pg_dumpall":
		_, _ = io.WriteString(cmd.Stdout, "CREATE ROLE app PASSWORD 'secret-hash';")
	case "pg_dump":
		_, _ = io.WriteString(cmd.Stdout, "custom dump "+strings.Join(cmd.Args, " "))
	case "pg_restore":
		_, _ = io.WriteString(cmd.Stdout, "; Archive created\n")
	case "zstd":
		_, _ = io.Copy(cmd.Stdout, cmd.Stdin)
	}
	return nil
}

type memoryStore struct {
	mu         sync.Mutex
	objects    map[string][]byte
	events     []string
	putError   map[string]error
	getError   map[string]error
	listPages  [][]string
	listTokens []string
	deleted    []string
	putReaders map[string]string
	moveError  error
}

func (s *memoryStore) Move(_ context.Context, source, destination string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "move:"+source+":"+destination)
	if s.moveError != nil {
		return s.moveError
	}
	s.objects[destination] = append([]byte(nil), s.objects[source]...)
	delete(s.objects, source)
	return nil
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}, putError: map[string]error{}, getError: map[string]error{}, putReaders: map[string]string{}}
}

func (s *memoryStore) Put(_ context.Context, key string, body io.Reader, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "put:"+key)
	s.putReaders[key] = reflect.TypeOf(body).String()
	if err := s.putError[key]; err != nil {
		return err
	}
	b, err := io.ReadAll(body)
	if err == nil {
		s.objects[key] = b
	}
	return err
}

func (s *memoryStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "get:"+key)
	if err := s.getError[key]; err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(s.objects[key])), nil
}

func (s *memoryStore) List(_ context.Context, _ string, token string) ([]string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "list:"+token)
	s.listTokens = append(s.listTokens, token)
	i := len(s.listTokens) - 1
	if i >= len(s.listPages) {
		return nil, "", nil
	}
	next := ""
	if i+1 < len(s.listPages) {
		next = "page-" + string(rune('2'+i))
	}
	return append([]string(nil), s.listPages[i]...), next, nil
}

func (s *memoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "delete:"+key)
	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		PGDSN:         "postgres://user:pass@db/core?sslmode=disable&dbname=wrong",
		EncryptionKey: bytes.Repeat([]byte{9}, 32), WorkDir: t.TempDir(), MinFreeBytes: 1,
	}
}

func TestCleanupAtBucketRootOnlyDeletesOwnedLegacyBackupNames(t *testing.T) {
	store := newMemoryStore()
	store.listPages = [][]string{{
		"latest.json", "core-20260816T000000Z.cwxubak", "algobak.uploading-old",
		"algobak", "customer.txt", "nested/latest.json", "other-core-1.cwxubak",
	}}
	runner := &Runner{}
	if err := runner.cleanup(context.Background(), store, "", "algobak"); err != nil {
		t.Fatal(err)
	}
	want := []string{"latest.json", "core-20260816T000000Z.cwxubak", "algobak.uploading-old"}
	if !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("bucket-root cleanup deleted wrong objects: %v, want %v", store.deleted, want)
	}
}

func TestRunnerCommandsArtifactAndPublicationOrder(t *testing.T) {
	commands := &fakeCommands{}
	store := newMemoryStore()
	store.listPages = [][]string{{"backups/core/latest.json", "backups/core/note.txt", "backups/core/core-20260816T000000Z.cwxubak"}}
	now := time.Date(2026, 8, 16, 12, 34, 56, 0, time.UTC)
	runner, err := NewRunner(validConfig(t), Dependencies{Commands: commands, Store: store, CloudConfig: func(context.Context) (CloudConfig, error) {
		return CloudConfig{Bucket: "bucket", Operator: "operator", Password: "password", Prefix: "backups/core"}, nil
	}, UploadID: func() string { return "test-id" }, Now: func() time.Time { return now }, DiskFree: func(string) (uint64, error) { return 1 << 40, nil }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "backups/core/algobak"
	if result.ArchiveKey != wantKey || result.SHA256 == "" || result.Databases != 2 {
		t.Fatalf("result = %+v", result)
	}

	var names []string
	for _, call := range commands.calls {
		names = append(names, call.name)
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "user:pass") || strings.Contains(joined, "?dbname=") || strings.Contains(joined, "&dbname=") {
			t.Fatalf("secret or query dbname leaked on argv: %v", call.args)
		}
		if call.name == "pg_dumpall" && !reflect.DeepEqual(call.args[1:], []string{"--globals-only"}) {
			t.Fatalf("globals args = %v, want password/ownership preserving dump", call.args)
		}
		if call.name == "pg_dump" && (!strings.Contains(joined, "--format=custom") || !strings.Contains(joined, "--create") || strings.Contains(joined, "--no-owner") || strings.Contains(joined, "--no-privileges")) {
			t.Fatalf("non-restore-ready database dump command: %v", call.args)
		}
		if (call.name == "psql" || strings.HasPrefix(call.name, "pg_")) && !reflect.DeepEqual(call.env, []string{"PGPASSWORD=pass"}) {
			t.Fatalf("%s env = %v, want isolated PGPASSWORD", call.name, call.env)
		}
	}
	if !reflect.DeepEqual(names, []string{"psql", "pg_dumpall", "pg_dump", "pg_restore", "pg_dump", "pg_restore", "zstd", "zstd", "zstd", "zstd"}) {
		t.Fatalf("commands = %v", names)
	}
	wantArgs := [][]string{
		{"--dbname=postgres://user@db/core?sslmode=disable", "--no-align", "--tuples-only", "--quiet", "--command=SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn AND datname <> 'postgres' ORDER BY datname"},
		{"--dbname=postgres://user@db/core?sslmode=disable", "--globals-only"},
		{"--dbname=postgres://user@db/app?sslmode=disable", "--format=custom", "--create"},
		{"--list", commands.calls[2].args[0][:0] + commands.calls[3].args[1]},
		{"--dbname=postgres://user@db/name%20with%20%2F%20and%20%3F?sslmode=disable", "--format=custom", "--create"},
		{"--list", commands.calls[5].args[1]},
		{"--quiet", "--stdout", "--threads=0"},
		{"--decompress", "--quiet", "--stdout"},
	}
	for i := range wantArgs {
		if !reflect.DeepEqual(commands.calls[i].args, wantArgs[i]) {
			t.Fatalf("command %d %s args = %v, want %v", i, commands.calls[i].name, commands.calls[i].args, wantArgs[i])
		}
	}
	for _, call := range commands.calls {
		if call.name == "pg_dump" && strings.Contains(strings.Join(call.args, " "), "/postgres?") {
			t.Fatalf("postgres database was dumped: %v", call.args)
		}
		if call.name == "pg_dump" && strings.Contains(strings.Join(call.args, " "), "wrong") {
			t.Fatalf("query dbname overrode target database: %v", call.args)
		}
	}

	archive := store.objects[wantKey]
	tempKey := "backups/core/algobak.uploading-test-id"
	if store.putReaders[tempKey] != "*os.File" {
		t.Fatalf("archive uploaded from %s, want bounded-memory file stream", store.putReaders[tempKey])
	}
	plain, err := DecryptArchive(archive, validConfig(t).EncryptionKey)
	if err != nil || !bytes.HasPrefix(plain, []byte("manifest.json")) {
		t.Fatalf("decrypt archive: prefix=%q err=%v", plain[:min(len(plain), 16)], err)
	}
	tampered := append([]byte(nil), archive...)
	tampered[len(tampered)-1] ^= 1
	if _, err := DecryptArchive(tampered, validConfig(t).EncryptionKey); err == nil {
		t.Fatal("tampered archive passed authenticated integrity check")
	}

	wantEventsPrefix := []string{"put:" + tempKey, "get:" + tempKey, "move:" + tempKey + ":" + wantKey, "get:" + wantKey}
	if !reflect.DeepEqual(store.events[:4], wantEventsPrefix) {
		t.Fatalf("publication events = %v", store.events)
	}
	if !reflect.DeepEqual(store.listTokens, []string{""}) {
		t.Fatalf("list tokens = %v", store.listTokens)
	}
	if !reflect.DeepEqual(store.deleted, []string{"backups/core/latest.json", "backups/core/core-20260816T000000Z.cwxubak", tempKey}) {
		t.Fatalf("deleted = %v", store.deleted)
	}
	digest := sha256.Sum256(archive)
	if result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("sha256 = %s, want %x", result.SHA256, digest)
	}
}

func TestRunnerFailureDoesNotPublishOrCleanup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*memoryStore, string)
	}{
		{"upload", func(s *memoryStore, archive string) { s.putError[archive] = errors.New("upload failed") }},
		{"verify", func(s *memoryStore, archive string) { s.getError[archive] = errors.New("download failed") }},
		{"replace", func(s *memoryStore, _ string) { s.moveError = errors.New("move failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			archive := "backups/core/algobak.uploading-test-id"
			store.objects["backups/core/algobak"] = []byte("old")
			tc.configure(store, archive)
			runner, err := NewRunner(validConfig(t), Dependencies{Commands: &fakeCommands{}, Store: store, CloudConfig: func(context.Context) (CloudConfig, error) {
				return CloudConfig{Bucket: "bucket", Operator: "operator", Password: "password", Prefix: "backups/core"}, nil
			}, UploadID: func() string { return "test-id" }, DiskFree: func(string) (uint64, error) { return 1 << 40, nil }, Now: func() time.Time {
				return time.Date(2026, 8, 16, 12, 34, 56, 0, time.UTC)
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.Run(context.Background()); err == nil {
				t.Fatal("Run succeeded, want failure")
			}
			if got := string(store.objects["backups/core/algobak"]); got != "old" {
				t.Fatalf("old fixed object changed on failure: %q", got)
			}
			if _, ok := store.objects[archive]; ok {
				t.Fatal("temporary object was not cleaned after failure")
			}
			wantEvents := map[string][]string{
				"upload":  {"put:" + archive, "delete:" + archive},
				"verify":  {"put:" + archive, "get:" + archive, "delete:" + archive},
				"replace": {"put:" + archive, "get:" + archive, "move:" + archive + ":backups/core/algobak", "delete:" + archive},
			}[tc.name]
			if !reflect.DeepEqual(store.events, wantEvents) {
				t.Fatalf("failure events = %v, want %v", store.events, wantEvents)
			}
		})
	}
}

func TestDatabaseDSNRemovesPasswordAndQueryDatabase(t *testing.T) {
	dsn, password, err := commandDSN("postgres://user:p%40ss@db/original?sslmode=require&dbname=wrong", "odd name/with?")
	if err != nil {
		t.Fatal(err)
	}
	if password != "p@ss" || strings.Contains(dsn, "p%40ss") || strings.Contains(dsn, "dbname=") || !strings.Contains(dsn, "odd%20name%2Fwith%3F") {
		t.Fatalf("commandDSN = (%q, %q)", dsn, password)
	}
}

func TestCommandEnvironmentReplacesInheritedPGPassword(t *testing.T) {
	environment := commandEnvironment([]string{"PATH=/bin", "PGPASSWORD=old", "HOME=/tmp"}, []string{"PGPASSWORD=new"})
	if !reflect.DeepEqual(environment, []string{"PATH=/bin", "HOME=/tmp", "PGPASSWORD=new"}) {
		t.Fatalf("command environment = %v", environment)
	}
}

func TestRunnerRejectsInsufficientWorkspaceBeforeCommands(t *testing.T) {
	cfg := validConfig(t)
	cfg.MinFreeBytes = 1024
	commands := &fakeCommands{}
	runner, err := NewRunner(cfg, Dependencies{Commands: commands, Store: newMemoryStore(), DiskFree: func(path string) (uint64, error) {
		if path != cfg.WorkDir {
			t.Fatalf("space checked at %q, want %q", path, cfg.WorkDir)
		}
		return 1023, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("Run error = %v", err)
	}
	if len(commands.calls) != 0 {
		t.Fatalf("commands ran before free-space preflight: %v", commands.calls)
	}
}

func TestRunnerValidateReportsIncompleteRuntimeUpyunConfiguration(t *testing.T) {
	runner, err := NewRunner(validConfig(t), Dependencies{CloudConfig: func(context.Context) (CloudConfig, error) {
		return CloudConfig{Bucket: "bucket"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "站点又拍云") {
		t.Fatalf("Validate error = %v", err)
	}
}

type maxWriteBuffer struct {
	bytes.Buffer
	max int
}

func (w *maxWriteBuffer) Write(p []byte) (int, error) {
	if len(p) > w.max {
		return 0, errors.New("non-streaming write of " + strconv.Itoa(len(p)) + " bytes")
	}
	return w.Buffer.Write(p)
}

func TestDecryptArchiveStreamAuthenticatesWithoutLargeWrites(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	plain := bytes.Repeat([]byte("restore-data"), encryptionChunkSize/4)
	encrypted, err := encryptArchive(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	out := &maxWriteBuffer{max: encryptionChunkSize}
	if err := DecryptArchiveStream(context.Background(), bytes.NewReader(encrypted), out, key); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain) {
		t.Fatal("streamed plaintext mismatch")
	}
	tampered := append([]byte(nil), encrypted...)
	tampered[len(tampered)-1] ^= 1
	out.Reset()
	if err := DecryptArchiveStream(context.Background(), bytes.NewReader(tampered), out, key); err == nil || out.Len() != 0 {
		t.Fatalf("tampered stream = (err %v, output %d), want authenticated failure before output", err, out.Len())
	}
}

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context) (Result, error) {
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func TestTaskStatusAndCancellation(t *testing.T) {
	task := NewTask(blockingRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := task.Run(ctx); done <- err }()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if task.Status().Stage == StageRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if task.Status().Stage != StageRunning {
		t.Fatalf("status = %+v", task.Status())
	}
	if _, err := task.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent Run error = %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Run error = %v", err)
	}
	status := task.Status()
	if status.Stage != StageFailed || status.Error == "" || status.FinishedAt.IsZero() {
		t.Fatalf("final status = %+v", status)
	}
}

func TestCloseFileTreatsAlreadyClosedAsSuccess(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closeFile(file); err != nil {
		t.Fatalf("closeFile on an already closed file: %v", err)
	}
}

func TestNormalizeCloseErrorTreatsRuntimeAlreadyClosedAsSuccess(t *testing.T) {
	err := &os.PathError{Op: "close", Path: "/tmp/archive", Err: errors.New("file already closed")}
	if got := normalizeCloseError(err); got != nil {
		t.Fatalf("normalizeCloseError: %v", got)
	}
}
