package service

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// avatarPublicBase 返回当前又拍云公开访问基址（scheme://domain）。
// 每次都从 site_configs 实时读取 → 换域/切 https 即时生效（与博客图一致）。
func avatarPublicBase(db *gorm.DB) string {
	if db == nil {
		return ""
	}
	return loadUpyunFromDB(db).PublicBaseURL()
}

// expandAvatarBase 将头像扩展为当前域名完整 URL；非又拍云头像原样返回。
func expandAvatarBase(base, avatar string) string {
	return blogimg.ExpandAvatarURL(avatar, base)
}

// normalizeAvatarForStore 头像入库规范化（绝对 URL → path-only key）。
func normalizeAvatarForStore(avatar string) string {
	return blogimg.NormalizeAvatarForStore(avatar)
}

type avatarExistsFunc func(string) (bool, error)

func validateAvatarReference(userID uint, avatar string, exists avatarExistsFunc) error {
	key := blogimg.AvatarObjectKeyFromAnyURL(avatar)
	if key == "" {
		return fmt.Errorf("头像必须来自本站上传")
	}
	ownerID, ok := blogimg.AvatarObjectOwnerID(key)
	if !ok || ownerID != userID {
		return fmt.Errorf("头像不属于当前用户")
	}
	if exists == nil {
		return fmt.Errorf("头像存储不可用")
	}
	existsNow, err := exists(key)
	if err != nil {
		return err
	}
	if !existsNow {
		return fmt.Errorf("头像文件不存在，请重新上传")
	}
	return nil
}

func resolveAvatarChange(userID uint, oldAvatar, requestedAvatar string, clear bool, exists avatarExistsFunc) (string, bool, error) {
	oldAvatar = strings.TrimSpace(oldAvatar)
	if clear {
		return "", oldAvatar != "", nil
	}
	requestedAvatar = strings.TrimSpace(requestedAvatar)
	if requestedAvatar == "" || requestedAvatar == oldAvatar || blogimg.SameAvatarObject(oldAvatar, requestedAvatar) {
		return oldAvatar, false, nil
	}
	next := normalizeAvatarForStore(requestedAvatar)
	if err := validateAvatarReference(userID, next, exists); err != nil {
		return oldAvatar, false, err
	}
	return next, true, nil
}

func staleAvatarObjectKey(userID uint, oldAvatar, newAvatar string) string {
	if blogimg.SameAvatarObject(oldAvatar, newAvatar) {
		return ""
	}
	key := blogimg.AvatarObjectKeyFromAnyURL(oldAvatar)
	ownerID, ok := blogimg.AvatarObjectOwnerID(key)
	if key == "" || !ok || ownerID != userID {
		return ""
	}
	return key
}

func avatarObjectReferenced(db *gorm.DB, key string) (bool, error) {
	key = blogimg.AvatarObjectKeyFromAnyURL(key)
	if db == nil || key == "" {
		return false, nil
	}
	var avatars []string
	if err := db.Model(&model.User{}).Where("avatar <> ''").Pluck("avatar", &avatars).Error; err != nil {
		return false, err
	}
	for _, avatar := range avatars {
		if blogimg.SameAvatarObject(avatar, key) {
			return true, nil
		}
	}
	return false, nil
}

// localAvatarRelPath 解析旧本地头像静态路径 → uploads 内相对路径；非本地路径返回 ""。
func localAvatarRelPath(avatar string) string {
	v := strings.TrimSpace(avatar)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if u, err := url.Parse(v); err == nil {
			v = u.Path
		}
	}
	for _, prefix := range []string{staticURLPrefix + "/", staticRoutePrefix + "/"} {
		if strings.HasPrefix(v, prefix) {
			rel := strings.TrimPrefix(v, prefix)
			if rel == "" || strings.Contains(rel, "..") {
				return ""
			}
			if !strings.HasPrefix(rel, "avatar/") {
				return ""
			}
			return filepath.FromSlash(rel)
		}
	}
	return ""
}

// deleteStaleAvatar 仅清理已不再被任何用户引用的托管头像对象；失败仅记日志。
// 遗留本地头像由显式迁移工具统一处理，资料更新路径绝不自动删除本地文件。
func deleteStaleAvatar(db *gorm.DB, userID uint, oldAvatar, newAvatar string) {
	oldAvatar = strings.TrimSpace(oldAvatar)
	newAvatar = strings.TrimSpace(newAvatar)
	if oldAvatar == "" || oldAvatar == newAvatar {
		return
	}
	if key := staleAvatarObjectKey(userID, oldAvatar, newAvatar); key != "" {
		if db == nil {
			return
		}
		referenced, err := avatarObjectReferenced(db, key)
		if err != nil {
			log.Warnf("check stale avatar reference %s: %v", key, err)
			return
		}
		if referenced {
			return
		}
		if err := loadUpyunFromDB(db).Delete(key); err != nil {
			log.Warnf("delete stale avatar %s: %v", key, err)
		}
		return
	}
}
