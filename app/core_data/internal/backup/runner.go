package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	ErrDisabled         = errors.New("backup is disabled")
	ErrAlreadyRunning   = errors.New("backup is already running")
	ErrAmbiguousPublish = errors.New("pointer publication outcome is ambiguous")
)

// Command describes a context-bound process without exposing secrets in argv.
type Command struct {
	Name   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
}

// CommandRunner executes PostgreSQL and compression tools and is injectable for tests.
type CommandRunner interface {
	Run(ctx context.Context, command Command) error
}

// ObjectStore is the minimal remote storage contract required by Runner.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix, token string) (keys []string, nextToken string, err error)
	Delete(ctx context.Context, key string) error
}

// Dependencies provides replaceable side effects.
type Dependencies struct {
	Commands CommandRunner
	Store    ObjectStore
	Now      func() time.Time
	DiskFree func(path string) (uint64, error)
}

// Result describes a successfully published backup.
type Result struct {
	ArchiveKey string    `json:"archiveKey"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Databases  int       `json:"databases"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Runner performs one complete backup transaction.
type Runner struct {
	cfg Config
	dep Dependencies
}

// NewRunner validates dependencies without starting work.
func NewRunner(cfg Config, dep Dependencies) (*Runner, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if len(cfg.EncryptionKey) != 32 || cfg.PGDSN == "" || cfg.Prefix == "" {
		return nil, errors.New("invalid backup configuration")
	}
	if dep.Commands == nil {
		dep.Commands = ExecCommandRunner{}
	}
	if dep.Store == nil {
		dep.Store = NewUpyunStore(cfg.Bucket, cfg.Operator, cfg.Password, nil)
	}
	if dep.Now == nil {
		dep.Now = time.Now
	}
	if dep.DiskFree == nil {
		dep.DiskFree = diskFree
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if cfg.MinFreeBytes == 0 {
		cfg.MinFreeBytes = defaultMinFreeBytes
	}
	return &Runner{cfg: cfg, dep: dep}, nil
}

// Run creates, uploads, verifies, publishes, and retains one backup.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	free, err := r.dep.DiskFree(r.cfg.WorkDir)
	if err != nil {
		return Result{}, fmt.Errorf("check workspace free space: %w", err)
	}
	if free < r.cfg.MinFreeBytes {
		return Result{}, fmt.Errorf("insufficient workspace free space: have %d bytes, require %d", free, r.cfg.MinFreeBytes)
	}
	dir, err := os.MkdirTemp(r.cfg.WorkDir, "cwxu-core-backup-")
	if err != nil {
		return Result{}, fmt.Errorf("create workspace: %w", err)
	}
	defer os.RemoveAll(dir)

	dbs, err := r.discoverDatabases(ctx)
	if err != nil {
		return Result{}, err
	}
	pgDSN, password, err := commandDSN(r.cfg.PGDSN, "")
	if err != nil {
		return Result{}, err
	}
	pgEnv := passwordEnv(password)
	files := make([]string, 0, len(dbs)+2)
	globals := filepath.Join(dir, "globals.sql")
	if err := r.runToFile(ctx, globals, Command{Name: "pg_dumpall", Args: []string{"--dbname=" + pgDSN, "--globals-only"}, Env: pgEnv}); err != nil {
		return Result{}, fmt.Errorf("dump globals: %w", err)
	}
	files = append(files, globals)
	for i, db := range dbs {
		file := filepath.Join(dir, fmt.Sprintf("database-%03d.dump", i+1))
		dbDSN, _, err := commandDSN(r.cfg.PGDSN, db)
		if err != nil {
			return Result{}, err
		}
		if err := r.runToFile(ctx, file, Command{Name: "pg_dump", Args: []string{"--dbname=" + dbDSN, "--format=custom", "--create"}, Env: pgEnv}); err != nil {
			return Result{}, fmt.Errorf("dump database %q: %w", db, err)
		}
		if err := r.dep.Commands.Run(ctx, Command{Name: "pg_restore", Args: []string{"--list", file}, Env: pgEnv, Stdout: io.Discard}); err != nil {
			return Result{}, fmt.Errorf("verify database dump %q: %w", db, err)
		}
		files = append(files, file)
	}
	created := r.dep.Now().UTC()
	manifestPath := filepath.Join(dir, "manifest.json")
	manifest, _ := json.MarshalIndent(struct {
		Version   int       `json:"version"`
		CreatedAt time.Time `json:"createdAt"`
		Databases []string  `json:"databases"`
	}{1, created, dbs}, "", "  ")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		return Result{}, fmt.Errorf("write manifest: %w", err)
	}
	files = append([]string{manifestPath}, files...)
	compressedPath := filepath.Join(dir, "backup.tar.zst")
	compressed, err := os.OpenFile(compressedPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("create compressed artifact: %w", err)
	}
	tarReader, tarWriter := io.Pipe()
	tarDone := make(chan error, 1)
	go func() {
		tarDone <- writeTar(ctx, tarWriter, files)
	}()
	compressErr := r.dep.Commands.Run(ctx, Command{Name: "zstd", Args: []string{"--quiet", "--stdout", "--threads=0"}, Stdin: tarReader, Stdout: compressed})
	_ = tarReader.CloseWithError(compressErr)
	tarErr := <-tarDone
	if err := errors.Join(compressErr, tarErr); err != nil {
		_ = compressed.Close()
		return Result{}, fmt.Errorf("compress backup: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return Result{}, fmt.Errorf("close compressed artifact: %w", err)
	}
	for _, file := range files {
		if file != manifestPath {
			if err := os.Remove(file); err != nil {
				return Result{}, fmt.Errorf("remove intermediate dump: %w", err)
			}
		}
	}
	compressed, err = os.Open(compressedPath)
	if err != nil {
		return Result{}, err
	}
	encryptedPath := filepath.Join(dir, "backup.cwxubak")
	encrypted, err := os.OpenFile(encryptedPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		_ = compressed.Close()
		return Result{}, err
	}
	if err := encryptArchiveStream(ctx, compressed, encrypted, r.cfg.EncryptionKey); err != nil {
		_ = compressed.Close()
		_ = encrypted.Close()
		return Result{}, fmt.Errorf("encrypt backup: %w", err)
	}
	if err := errors.Join(compressed.Close(), encrypted.Close()); err != nil {
		return Result{}, fmt.Errorf("close encrypted artifact: %w", err)
	}
	if err := os.Remove(compressedPath); err != nil {
		return Result{}, fmt.Errorf("remove compressed intermediate: %w", err)
	}
	archiveKey := path.Join(r.cfg.Prefix, "core-"+created.Format("20060102T150405Z")+".cwxubak")
	archive, err := os.Open(encryptedPath)
	if err != nil {
		return Result{}, err
	}
	digest := sha256.New()
	size, err := io.Copy(digest, archive)
	if err != nil {
		_ = archive.Close()
		return Result{}, fmt.Errorf("hash encrypted archive: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		_ = archive.Close()
		return Result{}, err
	}
	result := Result{ArchiveKey: archiveKey, SHA256: hex.EncodeToString(digest.Sum(nil)), Size: size, Databases: len(dbs), CreatedAt: created}
	if err := r.dep.Store.Put(ctx, archiveKey, archive, "application/octet-stream"); err != nil {
		_ = archive.Close()
		return Result{}, fmt.Errorf("upload immutable archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		return Result{}, err
	}
	if err := r.verify(ctx, archiveKey, result.SHA256, len(dbs), dir); err != nil {
		return Result{}, err
	}
	pointer, _ := json.Marshal(result)
	if err := r.dep.Store.Put(ctx, path.Join(r.cfg.Prefix, "latest.json"), bytes.NewReader(pointer), "application/json"); err != nil {
		return Result{}, fmt.Errorf("publish latest pointer: %w", err)
	}
	if err := r.cleanup(ctx, archiveKey); err != nil {
		return Result{}, fmt.Errorf("cleanup archives: %w", err)
	}
	return result, nil
}

func (r *Runner) discoverDatabases(ctx context.Context) ([]string, error) {
	const query = "SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn AND datname <> 'postgres' ORDER BY datname"
	var out bytes.Buffer
	dsn, password, err := commandDSN(r.cfg.PGDSN, "")
	if err != nil {
		return nil, err
	}
	if err := r.dep.Commands.Run(ctx, Command{Name: "psql", Args: []string{"--dbname=" + dsn, "--no-align", "--tuples-only", "--quiet", "--command=" + query}, Env: passwordEnv(password), Stdout: &out}); err != nil {
		return nil, fmt.Errorf("discover databases: %w", err)
	}
	var dbs []string
	for _, line := range strings.Split(out.String(), "\n") {
		if db := strings.TrimSpace(line); db != "" && db != "postgres" {
			dbs = append(dbs, db)
		}
	}
	sort.Strings(dbs)
	return dbs, nil
}

func (r *Runner) runToFile(ctx context.Context, file string, command Command) error {
	out, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	command.Stdout = out
	runErr := r.dep.Commands.Run(ctx, command)
	closeErr := out.Close()
	return errors.Join(runErr, closeErr)
}

func (r *Runner) verify(ctx context.Context, key, want string, databases int, dir string) error {
	body, err := r.dep.Store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("download archive for verification: %w", err)
	}
	defer body.Close()
	downloaded, err := os.OpenFile(filepath.Join(dir, "downloaded.cwxubak"), os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer downloaded.Close()
	h := sha256.New()
	if _, err := copyContext(ctx, io.MultiWriter(h, downloaded), body); err != nil {
		return fmt.Errorf("hash downloaded archive: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("verify downloaded archive: SHA256 mismatch: got %s want %s", got, want)
	}
	if _, err := downloaded.Seek(0, io.SeekStart); err != nil {
		return err
	}
	plainReader, plainWriter := io.Pipe()
	decryptDone := make(chan error, 1)
	go func() {
		err := DecryptArchiveStream(ctx, downloaded, plainWriter, r.cfg.EncryptionKey)
		_ = plainWriter.CloseWithError(err)
		decryptDone <- err
	}()
	tarReader, tarWriter := io.Pipe()
	decompressDone := make(chan error, 1)
	go func() {
		err := r.dep.Commands.Run(ctx, Command{Name: "zstd", Args: []string{"--decompress", "--quiet", "--stdout"}, Stdin: plainReader, Stdout: tarWriter})
		_ = plainReader.CloseWithError(err)
		_ = tarWriter.CloseWithError(err)
		decompressDone <- err
	}()
	shapeErr := verifyTar(ctx, tarReader, databases)
	_ = tarReader.CloseWithError(shapeErr)
	return errors.Join(shapeErr, <-decompressDone, <-decryptDone)
}

func (r *Runner) cleanup(ctx context.Context, current string) error {
	token := ""
	for {
		keys, next, err := r.dep.Store.List(ctx, r.cfg.Prefix+"/", token)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if key != current && strings.HasSuffix(key, ".cwxubak") {
				if err := r.dep.Store.Delete(ctx, key); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func writeTar(ctx context.Context, out *io.PipeWriter, files []string) error {
	tw := tar.NewWriter(out)
	defer out.Close()
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			_ = tw.Close()
			return err
		}
		header, _ := tar.FileInfoHeader(info, "")
		header.Name = filepath.Base(file)
		header.Mode = 0o600
		in, err := os.Open(file)
		if err == nil {
			err = tw.WriteHeader(header)
		}
		if err == nil {
			_, err = copyContext(ctx, tw, in)
		}
		if in != nil {
			_ = in.Close()
		}
		if err != nil {
			_ = tw.Close()
			return fmt.Errorf("add tar entry: %w", err)
		}
	}
	return tw.Close()
}

func commandDSN(dsn, database string) (string, string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", "", errors.New("CWXU_BACKUP_PG_DSN must be a postgres URI")
	}
	password, _ := u.User.Password()
	u.User = url.User(u.User.Username())
	query := u.Query()
	query.Del("dbname")
	u.RawQuery = query.Encode()
	if database != "" {
		u.Path = "/" + database
		u.RawPath = "/" + url.PathEscape(database)
	}
	return u.String(), password, nil
}

// ExecCommandRunner is the production context-aware command implementation.
type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, command Command) error {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Stdout = command.Stdout
	cmd.Stdin = command.Stdin
	cmd.Env = commandEnvironment(os.Environ(), command.Env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", command.Name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func commandEnvironment(base, overrides []string) []string {
	replaced := make(map[string]bool, len(overrides))
	for _, value := range overrides {
		name, _, _ := strings.Cut(value, "=")
		replaced[name] = true
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, value := range base {
		name, _, _ := strings.Cut(value, "=")
		if !replaced[name] {
			result = append(result, value)
		}
	}
	return append(result, overrides...)
}

func passwordEnv(password string) []string {
	if password == "" {
		return nil
	}
	return []string{"PGPASSWORD=" + password}
}

func diskFree(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

func verifyTar(ctx context.Context, source io.Reader, databases int) error {
	reader := tar.NewReader(source)
	want := map[string]bool{"manifest.json": false, "globals.sql": false}
	for i := 1; i <= databases; i++ {
		want[fmt.Sprintf("database-%03d.dump", i)] = false
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read verified tar: %w", err)
		}
		if _, ok := want[header.Name]; !ok || want[header.Name] || header.Size < 1 {
			return fmt.Errorf("invalid backup tar entry %q", header.Name)
		}
		want[header.Name] = true
		if _, err := copyContext(ctx, io.Discard, reader); err != nil {
			return err
		}
	}
	for name, found := range want {
		if !found {
			return fmt.Errorf("backup tar missing %q", name)
		}
	}
	return nil
}
