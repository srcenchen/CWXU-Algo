// Package blogimg provides pure helpers for blog image upload GC and compression policy.
package blogimg

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// MaxUploadBytes is the soft limit after compression for blog images (10MB).
const MaxUploadBytes = 10 << 20

// MaxDim is the longest edge kept when re-encoding oversized photos.
const MaxDim = 2560

// JPEGQuality prefers clarity over aggressive size reduction.
const JPEGQuality = 88

// mdImageRe matches markdown images ![alt](url)
var mdImageRe = regexp.MustCompile(`!\[[^\]]*]\(\s*<?([^)\s>]+)>?\s*(?:["'][^"']*["'])?\s*\)`)

// ExtractImageURLs collects http(s) image URLs from markdown content and cover.
func ExtractImageURLs(content, cover string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return
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
// Non-empty cover wins; otherwise if useFirst, the first http(s) image URL from
// content that fits maxLen (bytes) is used. Invalid auto candidates are skipped
// without error; empty result means no cover.
func ResolveCoverURL(cover, content string, useFirst bool, maxLen int) string {
	cover = strings.TrimSpace(cover)
	if cover != "" {
		return cover
	}
	if !useFirst {
		return ""
	}
	for _, u := range ExtractImageURLs(content, "") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		if maxLen > 0 && len(u) > maxLen {
			continue
		}
		return u
	}
	return ""
}

// ObjectKeyFromURL extracts the path key for a URL hosted on our public base.
// publicBase examples: "http://zhiyuansofts.cn", "https://cdn.example.com"
// Returns "" if URL is not under that host.
func ObjectKeyFromURL(imageURL, publicBase string) string {
	imageURL = strings.TrimSpace(imageURL)
	publicBase = strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if imageURL == "" || publicBase == "" {
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
		p := path.Clean("/" + strings.TrimPrefix(u.EscapedPath(), "/"))
		if p == "/" {
			return ""
		}
		return p
	}
	if !strings.EqualFold(u.Host, b.Host) {
		return ""
	}
	p := path.Clean("/" + strings.TrimPrefix(u.EscapedPath(), "/"))
	if p == "/" {
		return ""
	}
	return p
}

// KeysFromContent returns object keys referenced in content+cover for a given public base.
func KeysFromContent(content, cover, publicBase string) map[string]struct{} {
	keys := map[string]struct{}{}
	for _, u := range ExtractImageURLs(content, cover) {
		if k := ObjectKeyFromURL(u, publicBase); k != "" {
			keys[k] = struct{}{}
		}
	}
	return keys
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
