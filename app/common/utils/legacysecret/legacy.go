package legacysecret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const prefix = "enc:v1:"

func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, prefix)
}

func ResolveKey(yamlValue string) string {
	if value := strings.TrimSpace(os.Getenv("CWXU_CONFIG_ENCRYPTION_KEY")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("CONFIG_ENCRYPTION_KEY")); value != "" {
		return value
	}
	return strings.TrimSpace(yamlValue)
}

func legacyKey() string { return ResolveKey("") }

// Decrypt only supports migration of the retired enc:v1 site-config format.
func Decrypt(value string) (string, error) {
	return DecryptWithKey(value, ResolveKey(""))
}

func DecryptWithKey(value, rawKey string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	rawKey = strings.TrimSpace(rawKey)
	if len(rawKey) < 32 {
		return "", errors.New("legacy CWXU_CONFIG_ENCRYPTION_KEY is unavailable")
	}
	sum := sha256.Sum256([]byte(rawKey))
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("decode legacy encrypted secret: %w", err)
	}
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(payload) < gcm.NonceSize() {
		return "", errors.New("legacy encrypted secret is truncated")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("legacy encrypted secret authentication failed")
	}
	return string(plain), nil
}
