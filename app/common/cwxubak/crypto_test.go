package cwxubak

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := randomKey(t)
	payload := bytes.Repeat([]byte("payload-data-"), 300000)
	var encrypted bytes.Buffer
	if err := EncryptStream(context.Background(), bytes.NewReader(payload), &encrypted, key); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !bytes.Equal(encrypted.Bytes()[:len(Magic)], Magic) {
		t.Fatal("archive must start with magic")
	}
	var plain bytes.Buffer
	if err := DecryptStream(context.Background(), bytes.NewReader(encrypted.Bytes()), &plain, key); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(plain.Bytes(), payload) {
		t.Fatal("round-trip payload mismatch")
	}
}

func TestDecryptRejectsTamperedArchive(t *testing.T) {
	key := randomKey(t)
	payload := []byte("hello world backup data")
	var encrypted bytes.Buffer
	if err := EncryptStream(context.Background(), bytes.NewReader(payload), &encrypted, key); err != nil {
		t.Fatal(err)
	}
	tampered := encrypted.Bytes()
	tampered[12] ^= 0xFF
	var plain bytes.Buffer
	if err := DecryptStream(context.Background(), bytes.NewReader(tampered), &plain, key); err == nil {
		t.Fatal("expected error for tampered archive")
	}
	if plain.Len() != 0 {
		t.Fatal("no plaintext must be emitted after auth failure")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	key := randomKey(t)
	wrong := randomKey(t)
	var encrypted bytes.Buffer
	if err := EncryptStream(context.Background(), bytes.NewReader([]byte("data")), &encrypted, key); err != nil {
		t.Fatal(err)
	}
	if err := DecryptStream(context.Background(), bytes.NewReader(encrypted.Bytes()), &bytes.Buffer{}, wrong); err == nil {
		t.Fatal("expected error for wrong key")
	}
}

// buildTar 构造标准 tar（未压缩，测试内不经真实 zstd）。
func buildTar(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	now := time.Now()
	manifest, _ := json.Marshal(map[string]interface{}{
		"version": 1, "createdAt": now.UTC().Format(time.RFC3339),
		"databases": []string{"algo_core_data", "algo_user", "sanenchen", "support"},
	})
	add := func(name string, data []byte) {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add("manifest.json", manifest)
	add("globals.sql", []byte("-- globals\n"))
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("database-%03d.dump", i+1), bytes.Repeat([]byte("dump"), 100))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// copyCmd 模拟 zstd：把输入文件复制为 -o 目标（测试 tar 未压缩）。
type copyCmd struct{ calls []string }

func (c *copyCmd) Run(ctx context.Context, name string, args ...string) error {
	c.calls = append(c.calls, name)
	if name != "zstd" {
		return nil
	}
	var source, destination string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			if i+1 < len(args) {
				destination = args[i+1]
			}
		default:
			if !strings.HasPrefix(args[i], "-") && source == "" {
				source = args[i]
			}
		}
	}
	if destination == "" || source == "" {
		return fmt.Errorf("missing -o or source: %v", args)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer output.Close()
	_, err = io.Copy(output, input)
	return err
}

func TestVerifyEncryptedRoundTrip(t *testing.T) {
	key := randomKey(t)
	tarPayload := buildTar(t)
	var encrypted bytes.Buffer
	if err := EncryptStream(context.Background(), bytes.NewReader(tarPayload), &encrypted, key); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	runner := &copyCmd{}
	result, err := Verify(context.Background(), bytes.NewReader(encrypted.Bytes()), key, workDir, runner)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(result.Dumps) != 4 {
		t.Fatalf("expected 4 dumps, got %d", len(result.Dumps))
	}
	if _, err := os.Stat(result.Globals); err != nil {
		t.Fatalf("globals missing: %v", err)
	}
}

func TestVerifyRejectsBadStructure(t *testing.T) {
	key := randomKey(t)
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	now := time.Now()
	manifest, _ := json.Marshal(map[string]interface{}{
		"version": 1, "createdAt": now.UTC().Format(time.RFC3339),
		"databases": []string{"algo_user"},
	})
	if err := writer.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(manifest); err != nil {
		t.Fatal(err)
	}
	// 缺少 globals.sql 与 dump。
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := EncryptStream(context.Background(), bytes.NewReader(buffer.Bytes()), &encrypted, key); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(context.Background(), bytes.NewReader(encrypted.Bytes()), key, t.TempDir(), &copyCmd{})
	if err == nil {
		t.Fatal("expected structure error for incomplete archive")
	}
}
