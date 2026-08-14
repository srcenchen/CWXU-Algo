package blogimg

import (
	"net/url"
	"strconv"
	"strings"
)

// AvatarObjectKeyFromAnyURL 提取 /avatar/{uid}/… 对象 key。
// 兼容绝对 URL（任意 host）与裸路径；换域 / http↔https 不受影响（与博客图一致）。
// 仅当路径以 /avatar/{数字}/ 开头才视为本站头像对象，避免误判旧本地静态路径
// （/api/user/static/avatar/…）或外部 URL。
func AvatarObjectKeyFromAnyURL(avatar string) string {
	avatar = strings.TrimSpace(avatar)
	if avatar == "" {
		return ""
	}
	p := avatar
	if u, err := url.Parse(avatar); err == nil && u.Host != "" {
		p = u.Path
	} else if u, err := url.Parse("https:" + avatar); err == nil && u.Host != "" {
		p = u.Path
	}
	p = NormalizeObjectKey(p)
	if p == "" {
		return ""
	}
	rest := strings.TrimPrefix(p, "/")
	segs := strings.Split(rest, "/")
	if len(segs) < 2 {
		return ""
	}
	if !strings.EqualFold(segs[0], "avatar") {
		return ""
	}
	if !allDigits(segs[1]) {
		return ""
	}
	return p
}

// AvatarObjectOwnerID returns the user id encoded by /avatar/{uid}/….
func AvatarObjectOwnerID(avatar string) (uint, bool) {
	key := AvatarObjectKeyFromAnyURL(avatar)
	if key == "" {
		return 0, false
	}
	parts := strings.Split(strings.TrimPrefix(key, "/"), "/")
	if len(parts) < 3 {
		return 0, false
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}

// SameAvatarObject reports whether two managed avatar references resolve to the same object key.
// It handles legacy absolute URLs and canonical path-only values without treating external URLs as managed.
func SameAvatarObject(a, b string) bool {
	ka := AvatarObjectKeyFromAnyURL(a)
	kb := AvatarObjectKeyFromAnyURL(b)
	return ka != "" && kb != "" && ka == kb
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// AvatarObjectKeyForHash builds a content-addressed UpYun key for a user avatar.
// Format: /avatar/{userID}/{sha256hex}{ext}
func AvatarObjectKeyForHash(userID uint, contentHash, ext string) string {
	h := NormalizeHash(contentHash)
	if userID == 0 || h == "" {
		return ""
	}
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if ext == "" {
		ext = ".bin"
	}
	return NormalizeObjectKey("/avatar/" + itoaUint(userID) + "/" + h + ext)
}

// NormalizeAvatarForStore 入库规范化：又拍云头像（绝对 URL 或裸 key）→ path-only key；
// 其它值（外部 URL、旧本地静态路径等）原样返回。
func NormalizeAvatarForStore(avatar string) string {
	avatar = strings.TrimSpace(avatar)
	if avatar == "" {
		return ""
	}
	if k := AvatarObjectKeyFromAnyURL(avatar); k != "" {
		return k
	}
	return avatar
}

// ExpandAvatarURL 读时扩展：path-only 头像 key → publicBase + key；非头像值原样返回。
// publicBase 为空时返回 key（调用方通常保证已配置图床）。
func ExpandAvatarURL(avatar, publicBase string) string {
	avatar = strings.TrimSpace(avatar)
	if avatar == "" {
		return ""
	}
	key := AvatarObjectKeyFromAnyURL(avatar)
	if key == "" {
		return avatar
	}
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if base == "" {
		return key
	}
	return base + key
}
