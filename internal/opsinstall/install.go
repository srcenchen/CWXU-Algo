package opsinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	manifest := templatesManifest{Version: 1, Files: map[string]string{}}
	for _, managed := range managedAssets() {
		if err := writeManagedWithManifest(i.Root.Path, managed, &manifest); err != nil {
			return err
		}
	}
	if err := saveManifest(i.Root, manifest); err != nil {
		return err
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

func writeManagedWithManifest(root string, asset managedAsset, manifest *templatesManifest) error {
	content, err := ReadAsset(asset.source)
	if err != nil {
		return err
	}
	if err := writeAsset(root, asset.destination, content); err != nil {
		return err
	}
	manifest.Files[asset.destination] = hashContent(content)
	return nil
}

func writeAsset(root, relative string, content []byte) error {
	target := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if filepath.Ext(relative) == ".sh" {
		mode = 0o755
	}
	return atomicWrite(target, content, mode)
}

// templatesManifestFile 记录受管模板的发布基线哈希（install/刷新时写入），
// 用于识别本地手工改动：与基线一致才覆盖，不一致视为用户配置跳过。
const templatesManifestFile = "state/templates.json"

// templatesBackupDir 每次刷新被替换的旧内容快照目录（按时间戳分层），可人工恢复。
const templatesBackupDir = "state/templates.backup"

type templatesManifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

func loadManifest(root *opsroot.Root) (templatesManifest, bool) {
	m := templatesManifest{Version: 1, Files: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(root.Path, templatesManifestFile))
	if err != nil {
		return m, false
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return templatesManifest{Version: 1, Files: map[string]string{}}, false
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return m, true
}

func saveManifest(root *opsroot.Root, m templatesManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(root.Path, templatesManifestFile), data, 0o644)
}

func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// RefreshManaged 用内嵌模板刷新受管文件（compose.yaml、config/*.yaml、postgres-init.sh、
// nginx.conf、rabbitmq-entrypoint.sh），使升级同时带上编排与配置模板更新。
//
// 保护策略（避免覆盖用户配置）：
//   - 凭据与用户数据（.env、release.env、secrets/*）不在受管范围，绝不会被触碰；
//   - 以 state/templates.json 记录发布基线：文件与基线一致（我们写的、未被手改）才覆盖，
//     与基线不一致视为本地手工改动，跳过不碰并返回提示；
//   - 首次升级尚无基线时（老安装迁移），以新模板覆盖一次并留底，此后即受基线保护；
//   - 被替换的旧内容快照到 state/templates.backup/<时间戳>/，可随时人工恢复。
//
// 返回：是否有文件被更新；被跳过（本地手改）的路径列表；本次刷新使用的备份目录。
func RefreshManaged(root *opsroot.Root) (changed bool, skipped []string, backup string, err error) {
	if err := root.EnsureLayout(); err != nil {
		return false, nil, "", err
	}
	manifest, _ := loadManifest(root)
	backup = filepath.Join(root.Path, templatesBackupDir, time.Now().Format("20060102-150405"))
	for _, managed := range managedAssets() {
		content, err := ReadAsset(managed.source)
		if err != nil {
			return false, skipped, "", err
		}
		target := filepath.Join(root.Path, managed.destination)
		onDisk, err := os.ReadFile(target)
		hash := hashContent(content)
		switch {
		case err == nil && bytes.Equal(onDisk, content):
			// 已同步；刷新基线防止误判。
			manifest.Files[managed.destination] = hash
		case err != nil && os.IsNotExist(err):
			// 缺失：直接写入并记录基线。
			if err := writeAsset(root.Path, managed.destination, content); err != nil {
				return false, skipped, "", err
			}
			manifest.Files[managed.destination] = hash
			changed = true
		case err != nil:
			return false, skipped, "", err
		default:
			// 磁盘内容与内嵌模板不一致。
			baseline, tracked := manifest.Files[managed.destination]
			if tracked && baseline != hashContent(onDisk) {
				// 本地手工改动：跳过，不覆盖用户配置。
				skipped = append(skipped, managed.destination)
				continue
			}
			// 与基线一致（我们写的、未被改过）或首次迁移尚无基线 → 快照后覆盖。
			if len(onDisk) > 0 {
				snapshot := filepath.Join(backup, managed.destination)
				if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
					return false, skipped, "", err
				}
				if err := atomicWrite(snapshot, onDisk, 0o600); err != nil {
					return false, skipped, "", err
				}
			}
			if err := writeAsset(root.Path, managed.destination, content); err != nil {
				return false, skipped, "", err
			}
			manifest.Files[managed.destination] = hash
			changed = true
		}
	}
	if err := saveManifest(root, manifest); err != nil {
		return false, skipped, "", err
	}
	return changed, skipped, backup, nil
}

// RestoreBackup 从指定备份目录恢复本次刷新被替换的旧文件并删除该备份目录。
// backup 为空或目录不存在时静默跳过。返回是否有文件被恢复。
func RestoreBackup(root *opsroot.Root, backup string) (bool, error) {
	if backup == "" {
		return false, nil
	}
	info, err := os.Stat(backup)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("备份路径不是目录：%s", backup)
	}
	restored := false
	err = filepath.WalkDir(backup, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(backup, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := writeAsset(root.Path, rel, content); err != nil {
			return err
		}
		restored = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if restored {
		if err := os.RemoveAll(backup); err != nil {
			return false, err
		}
	}
	return restored, nil
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
