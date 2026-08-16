// Command avatar-migrate migrates locally-stored user avatars to UpYun.
//
// 迁移对象：users.avatar 非空、且仍指向本地静态路径（/api/user/static/avatar/… 或
// /v1/user/static/avatar/…）的头像。已迁移（/avatar/…）与外部 URL 原样保留。
// 迁移后写入 path-only key（/avatar/{uid}/{sha256}{ext}），失效 user:{id}:profile
// 缓存并删除本地文件；全部完成后（-delete-local）删除 uploads/avatar 目录。
//
// 需在 user 服务宿主机运行（读取本地磁盘 + config.yaml 中的 DB/Redis）。
//
//	./avatar-migrate -conf ./configs -dry-run
//	./avatar-migrate -conf ./configs -delete-local
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/conf"
	gorminit "cwxu-algo/app/common/data/gorm"
	redisinit "cwxu-algo/app/common/data/redis"
	"cwxu-algo/app/common/security"
	"cwxu-algo/app/common/upyun"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	staticURLPrefix   = "/api/user/static/"
	staticRoutePrefix = "/v1/user/static/"
	batchSize         = 500
)

func uploadDir() string {
	if d := os.Getenv("CWXU_UPLOAD_DIR"); d != "" {
		return d
	}
	return "./data/uploads"
}

// localAvatarRelPath 从头像存储值解析 uploads 内的相对路径；非本地头像返回 ""。
func localAvatarRelPath(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if u, err := url.Parse(v); err == nil {
			v = u.Path
		}
	}
	for _, prefix := range []string{staticURLPrefix, staticRoutePrefix} {
		if strings.HasPrefix(v, prefix) {
			rel := strings.TrimPrefix(v, prefix)
			if rel == "" || strings.Contains(rel, "..") {
				return ""
			}
			if !strings.HasPrefix(rel, "avatar/") {
				return ""
			}
			return rel
		}
	}
	return ""
}

func contentTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

func main() {
	var (
		flagconf    = flag.String("conf", "./configs", "config path (dir containing config.yaml)")
		dryRun      = flag.Bool("dry-run", false, "只预览将迁移的头像，不做任何修改")
		deleteLocal = flag.Bool("delete-local", false, "迁移完成后删除本地 uploads/avatar 目录")
	)
	flag.Parse()

	logger := log.NewStdLogger(os.Stdout)
	log.SetLogger(logger)

	c := config.New(config.WithSource(file.NewSource(*flagconf)))
	defer c.Close()
	if err := c.Load(); err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		log.Fatalf("解析配置失败: %v", err)
	}
	if bc.Data == nil || bc.Data.Database == nil {
		log.Fatalf("配置缺少 data.database")
	}
	// 加载 JWT 安全配置。
	if err := security.Configure(bc.Server); err != nil {
		log.Fatalf("初始化安全配置失败: %v", err)
	}
	db := gorminit.InitGorm(bc.Data)
	// Redis 仅用于失效 profile 缓存；未配置/连不上不阻断迁移（只告警）
	var red *redis.Client
	if bc.Data.Redis != nil {
		red = redisinit.InitRedis(bc.Data)
		defer red.Close()
	}

	client := blogimg.LoadUpyunClient(db)
	if !client.Configured() {
		log.Fatalf("site_configs 未配置又拍云（bucket/operator/password），无法迁移")
	}
	base := client.PublicBaseURL()
	if base == "" {
		log.Fatalf("site_configs 未配置又拍云访问域名（upyun_domain），无法迁移")
	}
	log.Infof("又拍云已就绪: %s", base)

	if err := run(db, red, client, *dryRun, *deleteLocal); err != nil {
		log.Fatalf("迁移失败: %v", err)
	}
}

func run(db *gorm.DB, red *redis.Client, client *upyun.Client, dryRun, deleteLocal bool) error {
	dir := uploadDir()
	ctx := context.Background()

	var (
		total           int
		migrated        int
		normalized      int
		skippedExternal int
		skippedMissing  int
		failed          int
	)
	var lastID uint
	for {
		var rows []model.User
		if err := db.Select("id", "avatar").
			Where("id > ? AND avatar <> '' AND avatar IS NOT NULL", lastID).
			Order("id ASC").Limit(batchSize).Find(&rows).Error; err != nil {
			return fmt.Errorf("查询用户: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, u := range rows {
			lastID = u.ID
			total++
			avatar := strings.TrimSpace(u.Avatar)

			// 已是又拍云头像（path-only key 或绝对 URL）：仅归一化存储为 path-only
			if key := blogimg.AvatarObjectKeyFromAnyURL(avatar); key != "" {
				if key != avatar {
					if !dryRun {
						if err := db.Model(&model.User{}).Where("id = ?", u.ID).Update("avatar", key).Error; err != nil {
							log.Errorf("user %d 归一化头像失败: %v", u.ID, err)
							failed++
							continue
						}
						if red != nil {
							_ = red.Del(ctx, fmt.Sprintf("user:%d:profile", u.ID)).Err()
						}
					}
					normalized++
					log.Infof("user %d 归一化头像 → %s", u.ID, key)
				}
				continue
			}

			rel := localAvatarRelPath(avatar)
			if rel == "" {
				skippedExternal++
				log.Infof("user %d 跳过（非本地头像）: %s", u.ID, avatar)
				continue
			}
			abs := filepath.Join(dir, rel)
			data, err := os.ReadFile(abs)
			if err != nil {
				skippedMissing++
				log.Warnf("user %d 本地头像缺失，跳过: %s (%v)", u.ID, abs, err)
				continue
			}
			ext := strings.ToLower(filepath.Ext(abs))
			sum := sha256.Sum256(data)
			hash := fmt.Sprintf("%x", sum[:])
			objectKey := blogimg.AvatarObjectKeyForHash(u.ID, hash, ext)
			if objectKey == "" {
				objectKey = fmt.Sprintf("/avatar/%d/%x%s", u.ID, sum[:], ext)
			}
			if dryRun {
				migrated++
				log.Infof("user %d 将迁移: %s → %s (%d bytes)", u.ID, abs, objectKey, len(data))
				continue
			}
			if err := client.Put(objectKey, data, contentTypeFromExt(ext)); err != nil {
				log.Errorf("user %d 上传失败 %s: %v", u.ID, objectKey, err)
				failed++
				continue
			}
			if err := db.Model(&model.User{}).Where("id = ?", u.ID).Update("avatar", objectKey).Error; err != nil {
				log.Errorf("user %d 更新头像字段失败: %v", u.ID, err)
				failed++
				continue
			}
			if red != nil {
				_ = red.Del(ctx, fmt.Sprintf("user:%d:profile", u.ID)).Err()
			}
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				log.Warnf("user %d 删除本地头像失败 %s: %v", u.ID, abs, err)
			}
			migrated++
			log.Infof("user %d 已迁移 → %s", u.ID, client.PublicURL(objectKey))
		}
	}

	if dryRun {
		log.Infof("dry-run 完成：共 %d 个头像，将迁移 %d，将归一化 %d，跳过非本地 %d，缺失 %d",
			total, migrated, normalized, skippedExternal, skippedMissing)
		return nil
	}

	if deleteLocal {
		if failed > 0 {
			log.Warnf("有 %d 个头像迁移失败，跳过删除本地 avatar 目录", failed)
		} else {
			avatarDir := filepath.Join(dir, "avatar")
			if _, err := os.Stat(avatarDir); err == nil {
				if err := os.RemoveAll(avatarDir); err != nil {
					log.Warnf("删除本地 avatar 目录失败: %v", err)
				} else {
					log.Infof("已删除本地头像目录: %s", avatarDir)
				}
			}
		}
	}
	log.Infof("迁移完成：共 %d 个头像，迁移 %d，归一化 %d，失败 %d，跳过非本地 %d，缺失 %d",
		total, migrated, normalized, failed, skippedExternal, skippedMissing)
	if failed > 0 {
		return fmt.Errorf("%d 个头像迁移失败，请检查日志", failed)
	}
	return nil
}
