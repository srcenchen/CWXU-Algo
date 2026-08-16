package backup

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const defaultMinFreeBytes = 5 << 30

// Config contains the complete, validated backup configuration.
type Config struct {
	PGDSN         string
	EncryptionKey []byte
	WorkDir       string
	MinFreeBytes  uint64
}

// LoadConfig reads static backup execution configuration from the environment.
func LoadConfig() (Config, error) {
	cfg := Config{
		PGDSN:        strings.TrimSpace(os.Getenv("CWXU_BACKUP_PG_DSN")),
		WorkDir:      strings.TrimSpace(os.Getenv("CWXU_BACKUP_WORK_DIR")),
		MinFreeBytes: defaultMinFreeBytes,
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	info, err := os.Stat(cfg.WorkDir)
	if err != nil {
		return cfg, fmt.Errorf("backup disabled: CWXU_BACKUP_WORK_DIR must be an existing directory: %w", err)
	}
	if !info.IsDir() {
		return cfg, errors.New("backup disabled: CWXU_BACKUP_WORK_DIR must be an existing directory")
	}
	var missing []string
	for name, value := range map[string]string{
		"CWXU_BACKUP_PG_DSN":              cfg.PGDSN,
		"CWXU_BACKUP_ENCRYPTION_KEY_FILE": strings.TrimSpace(os.Getenv("CWXU_BACKUP_ENCRYPTION_KEY_FILE")),
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("backup disabled: missing %s", strings.Join(missing, ", "))
	}
	key, err := os.ReadFile(strings.TrimSpace(os.Getenv("CWXU_BACKUP_ENCRYPTION_KEY_FILE")))
	if err != nil {
		return cfg, fmt.Errorf("backup disabled: read CWXU_BACKUP_ENCRYPTION_KEY_FILE: %w", err)
	}
	if len(key) != 32 {
		return cfg, errors.New("backup disabled: CWXU_BACKUP_ENCRYPTION_KEY_FILE must contain exactly 32 bytes")
	}
	cfg.EncryptionKey = append([]byte(nil), key...)
	return cfg, nil
}
