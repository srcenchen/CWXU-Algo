// Package blogimg provides pure helpers for blog image upload GC and compression policy.
package blogimg

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"net/url"
	"path"
	"regexp"
	"strings"

	_ "golang.org/x/image/webp"
)

// MaxUploadBytes is the soft limit after compression for blog images (10MB).
const MaxUploadBytes = 10 << 20

// MaxDim is the longest edge kept when re-encoding oversized photos.
const MaxDim = 2560

const (
	MaxInputImageSide   = 12_000
	MaxInputImagePixels = 40_000_000
)

var (
	ErrImageDimensionsExceeded = errors.New("image side exceeds limit")
	ErrImagePixelsExceeded     = errors.New("image pixel count exceeds limit")
)

// JPEGQuality prefers clarity over aggressive size reduction.
const JPEGQuality = 88

// mdImageRe matches markdown images ![alt](url)
var mdImageRe = regexp.MustCompile(`!\[[^\]]*]\(\s*<?([^)\s>]+)>?\s*(?:["'][^"']*["'])?\s*\)`)

// isStoredImageRef reports whether u is a usable image ref in content/cover:
// absolute http(s), or path-only blog object key (/blog/{uid}/…).
func isStoredImageRef(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return true
	}
	if BlogObjectKeyFromAnyURL(u) != "" {
		return true
	}
	k := NormalizeObjectKey(u)
	return strings.HasPrefix(strings.ToLower(k), "/blog/")
}

// ExtractImageURLs collects image refs from markdown content and cover.
// Accepts http(s) URLs and path-only blog object keys (canonical storage form).
func ExtractImageURLs(content, cover string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if !isStoredImageRef(u) {
			return
		}
		// Prefer path-only for blog objects so callers see stable keys.
		if k := BlogObjectKeyFromAnyURL(u); k != "" {
			u = k
		} else if k := NormalizeObjectKey(u); strings.HasPrefix(strings.ToLower(k), "/blog/") {
			u = k
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	add(cover)
	for _, m := range mdImageRe.FindAllStringSubmatch(content, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	// bare HTML <img src="...">
	htmlRe := regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)
	for _, m := range htmlRe.FindAllStringSubmatch(content, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	return out
}

// ResolveCoverURL picks the article cover for write paths.
// Non-empty cover wins; otherwise if useFirst, the first image ref from content
// that fits maxLen (bytes) is used. Blog object refs are returned path-only.
// Invalid auto candidates are skipped without error; empty result means no cover.
func ResolveCoverURL(cover, content string, useFirst bool, maxLen int) string {
	cover = strings.TrimSpace(cover)
	if cover != "" {
		return NormalizeCoverURL(cover)
	}
	if !useFirst {
		return ""
	}
	for _, u := range ExtractImageURLs(content, "") {
		u = strings.TrimSpace(u)
		if u == "" || !isStoredImageRef(u) {
			continue
		}
		u = NormalizeCoverURL(u)
		if maxLen > 0 && len(u) > maxLen {
			continue
		}
		return u
	}
	return ""
}

// blogObjectPathRe 匹配又拍云博客对象路径（与 host 无关），如 /blog/27/xxx.webp
var blogObjectPathRe = regexp.MustCompile(`(?i)(/blog/\d+/[^/?#\s)>"']+)`)

// NormalizeObjectKey forces leading slash and cleans path.
func NormalizeObjectKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	k := path.Clean("/" + strings.TrimPrefix(key, "/"))
	if k == "/" || k == "." {
		return ""
	}
	return k
}

// BlogObjectKeyFromAnyURL extracts /blog/{uid}/… path from a URL even when the
// public base host differs (http vs https、CDN 换域等历史脏数据）。
func BlogObjectKeyFromAnyURL(imageURL string) string {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return ""
	}
	if m := blogObjectPathRe.FindStringSubmatch(imageURL); len(m) > 1 {
		return NormalizeObjectKey(m[1])
	}
	u, err := url.Parse(imageURL)
	if err != nil || u.Path == "" {
		return ""
	}
	p := NormalizeObjectKey(u.EscapedPath())
	if strings.HasPrefix(strings.ToLower(p), "/blog/") {
		return p
	}
	return ""
}

// ObjectKeyFromURL extracts the path key for a URL hosted on our public base.
// publicBase examples: "http://zhiyuansofts.cn", "https://cdn.example.com"
// Returns "" if URL is not under that host.
// 若 host 不匹配但仍是 /blog/{id}/… 上传路径，仍返回 key（防止 GC 因换域误删）。
func ObjectKeyFromURL(imageURL, publicBase string) string {
	imageURL = strings.TrimSpace(imageURL)
	publicBase = strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if imageURL == "" {
		return ""
	}
	// 优先：标准博客对象路径（与 host 无关）
	if k := BlogObjectKeyFromAnyURL(imageURL); k != "" {
		return k
	}
	if publicBase == "" {
		return ""
	}
	u, err := url.Parse(imageURL)
	if err != nil || u.Host == "" {
		return ""
	}
	b, err := url.Parse(publicBase)
	if err != nil || b.Host == "" {
		// bare host
		baseHost := strings.TrimPrefix(strings.TrimPrefix(publicBase, "https://"), "http://")
		baseHost = strings.Split(baseHost, "/")[0]
		if !strings.EqualFold(u.Host, baseHost) {
			return ""
		}
		return NormalizeObjectKey(u.EscapedPath())
	}
	if !strings.EqualFold(u.Host, b.Host) {
		return ""
	}
	return NormalizeObjectKey(u.EscapedPath())
}

// KeysFromContent returns object keys referenced in content+cover for a given public base.
func KeysFromContent(content, cover, publicBase string) map[string]struct{} {
	keys := map[string]struct{}{}
	add := func(k string) {
		k = NormalizeObjectKey(k)
		if k == "" {
			return
		}
		keys[k] = struct{}{}
	}
	for _, u := range ExtractImageURLs(content, cover) {
		if k := ObjectKeyFromURL(u, publicBase); k != "" {
			add(k)
		}
	}
	// 裸路径 / 非标准 markdown 中的 /blog/n/… 也算引用
	blob := content + "\n" + cover
	for _, m := range blogObjectPathRe.FindAllStringSubmatch(blob, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	return keys
}

// AssetReferenced reports whether a registered asset is still cited in article text.
// 用 object key、完整 URL、无 scheme URL 做子串匹配，避免 host/scheme 漂移导致误删。
func AssetReferenced(objectKey, assetURL string, texts ...string) bool {
	key := NormalizeObjectKey(objectKey)
	urlFull := strings.TrimSpace(assetURL)
	var needles []string
	if key != "" {
		needles = append(needles, key, strings.TrimPrefix(key, "/"))
	}
	if urlFull != "" {
		needles = append(needles, urlFull)
		if u, err := url.Parse(urlFull); err == nil && u.Host != "" {
			// //host/path 与 host/path
			needles = append(needles, "//"+u.Host+u.Path)
			needles = append(needles, u.Host+u.Path)
		}
	}
	if len(needles) == 0 {
		return false
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		for _, n := range needles {
			if n != "" && strings.Contains(text, n) {
				return true
			}
		}
	}
	return false
}

// OrphanKeys returns registered keys that are not in the used set.
func OrphanKeys(registered []string, used map[string]struct{}) []string {
	var out []string
	for _, k := range registered {
		k = "/" + strings.TrimPrefix(strings.TrimSpace(k), "/")
		if k == "/" || k == "" {
			continue
		}
		if _, ok := used[k]; ok {
			continue
		}
		// also try without forcing if used has variants
		alt := strings.TrimPrefix(k, "/")
		if _, ok := used["/"+alt]; ok {
			continue
		}
		if _, ok := used[alt]; ok {
			continue
		}
		out = append(out, k)
	}
	return out
}

// CompressResult is the (possibly re-encoded) image bytes.
type CompressResult struct {
	Data        []byte
	ContentType string
	Ext         string // including dot, e.g. ".jpg"
	// Passthrough true if original bytes kept
	Passthrough bool
}

func validateImageConfig(cfg image.Config) error {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("图片尺寸无效")
	}
	if cfg.Width > MaxInputImageSide || cfg.Height > MaxInputImageSide {
		return fmt.Errorf("图片尺寸过大，单边不得超过 %d 像素: %w", MaxInputImageSide, ErrImageDimensionsExceeded)
	}
	if uint64(cfg.Width)*uint64(cfg.Height) > MaxInputImagePixels {
		return fmt.Errorf("图片总像素过大，不得超过 4000 万像素: %w", ErrImagePixelsExceeded)
	}
	return nil
}

// ValidateAndCompressForUpload rejects decompression bombs before full decoding.
func ValidateAndCompressForUpload(data []byte, contentType string) (CompressResult, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return CompressResult{}, fmt.Errorf("无法读取图片尺寸: %w", err)
	}
	if err := validateImageConfig(cfg); err != nil {
		return CompressResult{}, err
	}
	return CompressForUpload(data, contentType)
}

// CompressForUpload keeps small/clear images as-is; re-encodes large rasters with high quality.
// GIF/WebP/SVG/ICO are passed through when under MaxUploadBytes (no re-encode to avoid quality loss).
func CompressForUpload(data []byte, contentType string) (CompressResult, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if len(data) == 0 {
		return CompressResult{}, fmt.Errorf("empty image")
	}
	// Always pass through if already small enough for non-jpeg/png or animated formats
	switch ct {
	case "image/gif", "image/webp", "image/svg+xml", "image/x-icon", "image/vnd.microsoft.icon":
		if len(data) <= MaxUploadBytes {
			return CompressResult{
				Data: data, ContentType: ct, Ext: extForCT(ct), Passthrough: true,
			}, nil
		}
		return CompressResult{}, fmt.Errorf("图片过大（最大 %dMB）", MaxUploadBytes>>20)
	}

	// jpeg/png: if small, keep; if large or huge dims, re-encode
	if len(data) <= 1<<20 && (ct == "image/jpeg" || ct == "image/png") {
		return CompressResult{
			Data: data, ContentType: ct, Ext: extForCT(ct), Passthrough: true,
		}, nil
	}

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// undecodable but claimed image and under limit → pass through
		if len(data) <= MaxUploadBytes {
			return CompressResult{
				Data: data, ContentType: ct, Ext: extForCT(ct), Passthrough: true,
			}, nil
		}
		return CompressResult{}, fmt.Errorf("无法解码图片: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	outImg := img
	if w > MaxDim || h > MaxDim {
		outImg = resizeMax(img, MaxDim)
	}
	var buf bytes.Buffer
	// Prefer JPEG for photos (clarity @ q88); keep PNG if source was PNG and not huge
	if format == "png" && len(data) <= 2<<20 && w <= MaxDim && h <= MaxDim {
		if err := png.Encode(&buf, outImg); err != nil {
			return CompressResult{}, err
		}
		return CompressResult{Data: buf.Bytes(), ContentType: "image/png", Ext: ".png"}, nil
	}
	if err := jpeg.Encode(&buf, outImg, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return CompressResult{}, err
	}
	out := buf.Bytes()
	if len(out) > MaxUploadBytes {
		return CompressResult{}, fmt.Errorf("压缩后仍超过 %dMB", MaxUploadBytes>>20)
	}
	return CompressResult{Data: out, ContentType: "image/jpeg", Ext: ".jpg"}, nil
}

func extForCT(ct string) string {
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico"
	default:
		return ".bin"
	}
}

// resizeMax scales so the longest edge is ≤ maxDim (nearest-neighbor, no extra deps).
func resizeMax(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return src
	}
	scale := float64(maxDim) / float64(w)
	if h > w {
		scale = float64(maxDim) / float64(h)
	}
	nw := int(float64(w) * scale)
	nh := int(float64(h) * scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			sx := b.Min.X + int(float64(x)/float64(nw)*float64(w))
			sy := b.Min.Y + int(float64(y)/float64(nh)*float64(h))
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// CanUpload reports whether image upload is allowed for a user.
func CanUpload(siteUpyunConfigured, userAuthorized bool) bool {
	return siteUpyunConfigured && userAuthorized
}
