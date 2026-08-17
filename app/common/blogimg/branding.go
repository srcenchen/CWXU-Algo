package blogimg

import "strings"

// BrandingObjectKeyForHash builds a content-addressed key for site and organization branding.
func BrandingObjectKeyForHash(userID uint, contentHash, ext string) string {
	hash := NormalizeHash(contentHash)
	if userID == 0 || hash == "" {
		return ""
	}
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if ext == "" {
		ext = ".bin"
	}
	return NormalizeObjectKey("/branding/" + itoaUint(userID) + "/" + hash + ext)
}
