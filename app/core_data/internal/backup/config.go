package backup

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const defaultMinFreeBytes = 5 << 30

// Config contains the complete, validated backup configuration.
type Config struct {
	Enabled       bool
	PGDSN         string
	Bucket        string
	Operator      string
	Password      string
	Prefix        string
	EncryptionKey []byte
	WorkDir       string
	MinFreeBytes  uint64
}

// LoadConfig reads backup configuration from the environment. Invalid enabled
// configuration is returned disabled with an explicit error, allowing startup
// to continue safely.
func LoadConfig() (Config, error) {
	switch enabled := strings.ToLower(strings.TrimSpace(os.Getenv("CWXU_BACKUP_ENABLED"))); enabled {
	case "", "false":
		return Config{}, nil
	case "true":
	default:
		return Config{}, fmt.Errorf("backup disabled: CWXU_BACKUP_ENABLED must be true or false, got %q", enabled)
	}
	cfg := Config{
		Enabled:      true,
		PGDSN:        strings.TrimSpace(os.Getenv("CWXU_BACKUP_PG_DSN")),
		Bucket:       strings.TrimSpace(os.Getenv("CWXU_BACKUP_UPYUN_BUCKET")),
		Operator:     strings.TrimSpace(os.Getenv("CWXU_BACKUP_UPYUN_OPERATOR")),
		Password:     os.Getenv("CWXU_BACKUP_UPYUN_PASSWORD"),
		Prefix:       strings.Trim(strings.TrimSpace(os.Getenv("CWXU_BACKUP_UPYUN_PREFIX")), "/"),
		WorkDir:      strings.TrimSpace(os.Getenv("CWXU_BACKUP_WORK_DIR")),
		MinFreeBytes: defaultMinFreeBytes,
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if value := strings.TrimSpace(os.Getenv("CWXU_BACKUP_MIN_FREE_BYTES")); value != "" {
		minimum, err := strconv.ParseUint(value, 10, 64)
		if err != nil || minimum == 0 {
			cfg.Enabled = false
			return cfg, errors.New("backup disabled: CWXU_BACKUP_MIN_FREE_BYTES must be a positive integer")
		}
		cfg.MinFreeBytes = minimum
	}
	info, err := os.Stat(cfg.WorkDir)
	if err != nil {
		cfg.Enabled = false
		return cfg, fmt.Errorf("backup disabled: CWXU_BACKUP_WORK_DIR must be an existing directory: %w", err)
	}
	if !info.IsDir() {
		cfg.Enabled = false
		return cfg, errors.New("backup disabled: CWXU_BACKUP_WORK_DIR must be an existing directory")
	}
	var missing []string
	for name, value := range map[string]string{
		"CWXU_BACKUP_PG_DSN":              cfg.PGDSN,
		"CWXU_BACKUP_UPYUN_BUCKET":        cfg.Bucket,
		"CWXU_BACKUP_UPYUN_OPERATOR":      cfg.Operator,
		"CWXU_BACKUP_UPYUN_PASSWORD":      cfg.Password,
		"CWXU_BACKUP_UPYUN_PREFIX":        cfg.Prefix,
		"CWXU_BACKUP_ENCRYPTION_KEY_FILE": strings.TrimSpace(os.Getenv("CWXU_BACKUP_ENCRYPTION_KEY_FILE")),
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		cfg.Enabled = false
		return cfg, fmt.Errorf("backup disabled: missing %s", strings.Join(missing, ", "))
	}
	key, err := os.ReadFile(strings.TrimSpace(os.Getenv("CWXU_BACKUP_ENCRYPTION_KEY_FILE")))
	if err != nil {
		cfg.Enabled = false
		return cfg, fmt.Errorf("backup disabled: read CWXU_BACKUP_ENCRYPTION_KEY_FILE: %w", err)
	}
	if len(key) != 32 {
		cfg.Enabled = false
		return cfg, errors.New("backup disabled: CWXU_BACKUP_ENCRYPTION_KEY_FILE must contain exactly 32 bytes")
	}
	cfg.EncryptionKey = append([]byte(nil), key...)
	return cfg, nil
}
