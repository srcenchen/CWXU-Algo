package service

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"cwxu-algo/app/common/blogimg"

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

// deleteStaleAvatar 尽力删除被替换的旧头像对象（又拍云对象或本地文件），失败仅记日志。
func deleteStaleAvatar(db *gorm.DB, oldAvatar, newAvatar string) {
	oldAvatar = strings.TrimSpace(oldAvatar)
	newAvatar = strings.TrimSpace(newAvatar)
	if oldAvatar == "" || oldAvatar == newAvatar {
		return
	}
	if key := blogimg.AvatarObjectKeyFromAnyURL(oldAvatar); key != "" {
		if db == nil {
			return
		}
		if err := loadUpyunFromDB(db).Delete(key); err != nil {
			log.Warnf("delete stale avatar %s: %v", key, err)
		}
		return
	}
	if rel := localAvatarRelPath(oldAvatar); rel != "" {
		_ = os.Remove(filepath.Join(UploadDir(), rel))
	}
}
