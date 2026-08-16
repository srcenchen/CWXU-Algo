package opsinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cwxu-algo/internal/opsroot"
)

type Installer struct {
	Root *opsroot.Root
}

func New(root *opsroot.Root) *Installer {
	return &Installer{Root: root}
}

func (i *Installer) Install(ctx context.Context) error {
	if err := i.Scaffold(); err != nil {
		return err
	}
	return i.Secrets()
}

func (i *Installer) Scaffold() error {
	if err := i.Root.EnsureLayout(); err != nil {
		return err
	}
	for _, managed := range managedAssets() {
		if err := writeManaged(i.Root.Path, managed); err != nil {
			return err
		}
	}
	return writeEnvIfMissing(i.Root.Path)
}

func (i *Installer) Secrets() error {
	for _, secret := range []string{"postgres_password", "redis_password", "rabbitmq_password", "jwt_secret"} {
		if err := writeHexSecret(i.Root.Path, "secrets/"+secret, 32); err != nil {
			return err
		}
	}
	if err := writeSecret(i.Root.Path, "secrets/backup_encryption_key", 32); err != nil {
		return err
	}
	if err := generateRSAKeyPair(i.Root.Path, 3072); err != nil {
		return err
	}
	if err := writeAppEnv(i.Root.Path); err != nil {
		return err
	}
	if err := applyOwnership(i.Root.Path); err != nil {
		return err
	}
	return writeInstallMarker(i.Root)
}

type managedAsset struct {
	source      string
	destination string
}

func managedAssets() []managedAsset {
	return []managedAsset{
		{source: "compose.yaml", destination: "compose.yaml"},
		{source: "config/gateway.yaml", destination: "config/gateway.yaml"},
		{source: "config/user.yaml", destination: "config/user.yaml"},
		{source: "config/core-data.yaml", destination: "config/core-data.yaml"},
		{source: "config/agent.yaml", destination: "config/agent.yaml"},
		{source: "config/postgres-init.sh", destination: "config/postgres-init.sh"},
		{source: "docker/nginx.conf", destination: "config/nginx.conf"},
		{source: "docker/rabbitmq-entrypoint.sh", destination: "config/rabbitmq-entrypoint.sh"},
	}
}

func writeManaged(root string, asset managedAsset) error {
	content, err := ReadAsset(asset.source)
	if err != nil {
		return err
	}
	target := filepath.Join(root, asset.destination)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if filepath.Ext(asset.destination) == ".sh" {
		mode = 0o755
	}
	return atomicWrite(target, content, mode)
}

func writeEnvIfMissing(root string) error {
	target := filepath.Join(root, ".env")
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	content, err := ReadAsset("env.example")
	if err != nil {
		return err
	}
	rendered := []byte(fmt.Sprintf("GOALGO_ROOT=%s\n%s", root, string(content)))
	if err := atomicWrite(target, rendered, 0o600); err != nil {
		return err
	}
	return nil
}

func writeAppEnv(root string) error {
	read := func(relative string) string {
		value, err := readSecret(root, relative)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(value)
	}
	postgresPassword := read("secrets/postgres_password")
	redisPassword := read("secrets/redis_password")
	rabbitmqPassword := read("secrets/rabbitmq_password")
	jwtSecret := read("secrets/jwt_secret")
	postgresUser := readEnv(root, "POSTGRES_USER", "goalgo")
	rabbitmqUser := readEnv(root, "RABBITMQ_USER", "goalgo")
	content := fmt.Sprintf(
		"POSTGRES_USER=%s\nRABBITMQ_USER=%s\n"+
			"USER_DATABASE_DSN=host=postgres user=%s password=%s dbname=algo_user port=5432 sslmode=disable TimeZone=Asia/Shanghai\n"+
			"CORE_DATABASE_DSN=host=postgres user=%s password=%s dbname=algo_core_data port=5432 sslmode=disable TimeZone=Asia/Shanghai\n"+
			"REDIS_ADDR=redis:6379\nREDIS_PASSWORD=%s\n"+
			"AMQP_DSN=amqp://%s:%s@rabbitmq:5672/goalgo\n"+
			"CWXU_JWT_SECRET=%s\n"+
			"CWXU_BACKUP_PG_DSN=%s\n"+
			"JWT_PRIVATE_KEY_FILE=/run/secrets/jwt_private_key.pem\n"+
			"JWT_PUBLIC_KEY_FILE=/run/secrets/jwt_public_key.pem\n",
		postgresUser, rabbitmqUser, postgresUser, postgresPassword, postgresUser, postgresPassword,
		redisPassword, rabbitmqUser, rabbitmqPassword, jwtSecret, backupPostgresDSN(postgresUser, postgresPassword))
	target := filepath.Join(root, "secrets", "app.env")
	return atomicWrite(target, []byte(content), 0o600)
}

func backupPostgresDSN(user, password string) string {
	u := &url.URL{Scheme: "postgres", Host: "postgres:5432", Path: "/postgres"}
	u.User = url.UserPassword(user, password)
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

func readEnv(root, key, fallback string) string {
	content, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		return fallback
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
			value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), key+"="))
			if value != "" {
				return value
			}
		}
	}
	return fallback
}

func applyOwnership(root string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	// Container data directories expect specific UIDs.
	owners := map[string]int{
		"data/postgres": 999,
		"data/redis":    999,
		"data/rabbitmq": 100,
		"data/consul":   100,
		"data/backups":  10001,
	}
	for relative, uid := range owners {
		target := filepath.Join(root, relative)
		if err := os.Chown(target, uid, 1000); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("chown %s：%w", relative, err)
		}
	}
	for _, relative := range []string{"secrets/jwt_private_key.pem", "secrets/jwt_public_key.pem", "secrets/backup_encryption_key"} {
		target := filepath.Join(root, relative)
		if err := os.Chown(target, 10001, 10001); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("chown %s：%w", relative, err)
		}
	}
	return nil
}

type installMarker struct {
	SchemaVersion int    `json:"schemaVersion"`
	InstalledAt   string `json:"installedAt"`
	AdminCreated  bool   `json:"adminCreated"`
}

func writeInstallMarker(root *opsroot.Root) error {
	if _, err := os.Stat(root.Join("state", "install.json")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	marker := installMarker{
		SchemaVersion: 1,
		InstalledAt:   time.Now().UTC().Format(time.RFC3339),
		AdminCreated:  false,
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(root.Join("state", "install.json"), data, 0o644)
}

func AdminCreated(root *opsroot.Root) bool {
	data, err := os.ReadFile(root.Join("state", "install.json"))
	if err != nil {
		return false
	}
	var marker installMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return false
	}
	return marker.AdminCreated
}

func MarkAdminCreated(root *opsroot.Root) error {
	path := root.Join("state", "install.json")
	marker := installMarker{SchemaVersion: 1, AdminCreated: true}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &marker)
	}
	marker.AdminCreated = true
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o644)
}

// ReadTemplateEnv 返回带注释的 .env 模板（首行写入 GOALGO_ROOT），供 init 打印。
func ReadTemplateEnv(root string) string {
	content, err := ReadAsset("env.example")
	if err != nil {
		return "GOALGO_ROOT=" + root + "\n"
	}
	rendered := fmt.Sprintf("GOALGO_ROOT=%s\n%s", root, string(content))
	return rendered + "\n# 其余密钥与凭据由 install 自动生成，无需填写。\n"
}
