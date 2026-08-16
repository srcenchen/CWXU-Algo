package legacysecret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func legacyCiphertext(t *testing.T, key, plain string) string {
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
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	payload := append(nonce, gcm.Seal(nil, nonce, []byte(plain), nil)...)
	return "enc:v1:" + base64.RawStdEncoding.EncodeToString(payload)
}

func TestDecryptReportsMissingLegacyKey(t *testing.T) {
	t.Setenv("CWXU_CONFIG_ENCRYPTION_KEY", "")
	t.Setenv("CONFIG_ENCRYPTION_KEY", "")
	_, err := Decrypt("enc:v1:payload")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("expected unavailable legacy key error, got %v", err)
	}
}

func TestLegacyKeySupportsHistoricalRenderedEnvironmentName(t *testing.T) {
	t.Setenv("CWXU_CONFIG_ENCRYPTION_KEY", "")
	t.Setenv("CONFIG_ENCRYPTION_KEY", strings.Repeat("k", 32))
	if got := legacyKey(); got != strings.Repeat("k", 32) {
		t.Fatalf("legacy key = %q", got)
	}
}

func TestDecryptWithKeyUpgradesRealLegacyCiphertext(t *testing.T) {
	key := strings.Repeat("yaml-key-", 4)
	ciphertext := legacyCiphertext(t, key, "smtp-password")
	got, err := DecryptWithKey(ciphertext, key)
	if err != nil || got != "smtp-password" {
		t.Fatalf("DecryptWithKey = %q, %v", got, err)
	}
}

func TestResolveKeyPrefersCurrentEnvThenHistoricalEnvThenYAML(t *testing.T) {
	t.Setenv("CWXU_CONFIG_ENCRYPTION_KEY", "current-env-key-current-env-key-12")
	t.Setenv("CONFIG_ENCRYPTION_KEY", "historical-env-key-historical-123")
	if got := ResolveKey("yaml-key-yaml-key-yaml-key-yaml-key"); got != "current-env-key-current-env-key-12" {
		t.Fatalf("current env priority = %q", got)
	}
	t.Setenv("CWXU_CONFIG_ENCRYPTION_KEY", "")
	if got := ResolveKey("yaml-key-yaml-key-yaml-key-yaml-key"); got != "historical-env-key-historical-123" {
		t.Fatalf("historical env priority = %q", got)
	}
	t.Setenv("CONFIG_ENCRYPTION_KEY", "")
	if got := ResolveKey("yaml-key-yaml-key-yaml-key-yaml-key"); got != "yaml-key-yaml-key-yaml-key-yaml-key" {
		t.Fatalf("yaml fallback = %q", got)
	}
}
