package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func encryptedLegacyTestValue(t *testing.T, key, plain string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	payload := append(nonce, gcm.Seal(nil, nonce, []byte(plain), nil)...)
	return "enc:v1:" + base64.RawStdEncoding.EncodeToString(payload)
}

func writeLegacySiteBackup(t *testing.T, key string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Version: FormatVersion, Scopes: []string{ScopeSite}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ciphertext := encryptedLegacyTestValue(t, key, "old-smtp-secret")
	row, _ := json.Marshal(map[string]any{"id": 1, "smtp_password": ciphertext, "agent_secret": "plain"})
	if err := os.WriteFile(filepath.Join(dir, "data", "site_configs.ndjson"), append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestImportRejectsLegacyCiphertextBeforeAnyWriteWithoutCorrectKey(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE site_configs (id integer primary key, smtp_password text, agent_secret text)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO site_configs(id,smtp_password) VALUES(1,'keep-me')`).Error; err != nil {
		t.Fatal(err)
	}
	dir := writeLegacySiteBackup(t, strings.Repeat("k", 32))
	_, err = Import(ImportOptions{DBs: DBs{User: db}, Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "旧配置") {
		t.Fatalf("missing migration guidance: %v", err)
	}
	var got string
	if err := db.Raw(`SELECT smtp_password FROM site_configs WHERE id=1`).Scan(&got).Error; err != nil || got != "keep-me" {
		t.Fatalf("target changed before preflight: %q, %v", got, err)
	}
}

func TestPreprocessLegacySiteConfigConvertsAllCiphertextToPlaintext(t *testing.T) {
	key := strings.Repeat("k", 32)
	dir := writeLegacySiteBackup(t, key)
	if err := preprocessLegacySiteConfig(filepath.Join(dir, "data", "site_configs.ndjson"), key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "data", "site_configs.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "enc:v1:") || !strings.Contains(string(raw), "old-smtp-secret") {
		t.Fatalf("legacy backup was not rewritten to plaintext: %s", raw)
	}
}
