package blogimg

import (
	"regexp"
	"strings"
)

// absoluteBlogObjectURLRe matches full http(s) URLs whose path contains a blog
// object key (/blog/{uid}/…). Host/scheme may be historical CDN domains.
var absoluteBlogObjectURLRe = regexp.MustCompile(`https?://[^\s)>"']+/blog/\d+/[^\s)>"']*`)

// bareBlogObjectPathRe matches a path-only blog object key, not already part of
// an absolute URL (lookbehind via consuming a non-URL prefix).
// Used after Normalize so content only has bare keys for our images.
var bareBlogObjectPathExpandRe = regexp.MustCompile(`(^|[^a-zA-Z0-9:/])(/blog/\d+/[^/?#\s)>"']+)`)

// NormalizeStoredImageRefs rewrites absolute blog-object image URLs to path-only
// form (/blog/{uid}/file). External links and non-blog paths are left unchanged.
// Idempotent: already path-only refs stay path-only.
func NormalizeStoredImageRefs(text string) string {
	if text == "" || !strings.Contains(text, "/blog/") {
		return text
	}
	return absoluteBlogObjectURLRe.ReplaceAllStringFunc(text, func(raw string) string {
		// Trim common trailing punctuation that may have been captured
		u, trail := splitTrailingPunct(raw)
		if k := BlogObjectKeyFromAnyURL(u); k != "" {
			return k + trail
		}
		return raw
	})
}

// ExpandStoredImageRefs rewrites stored blog image refs (path-only or any-host
// absolute) to publicBase + object key. publicBase is e.g. "https://cdn.example.com".
// When publicBase is empty, returns text unchanged (callers may still serve paths).
func ExpandStoredImageRefs(text, publicBase string) string {
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if text == "" || base == "" {
		return text
	}
	if !strings.Contains(text, "/blog/") {
		return text
	}
	// 1) Collapse any absolute blog URLs to bare keys first (avoids double-prefix).
	normalized := NormalizeStoredImageRefs(text)
	// 2) Expand bare /blog/n/… keys.
	return bareBlogObjectPathExpandRe.ReplaceAllStringFunc(normalized, func(m string) string {
		sub := bareBlogObjectPathExpandRe.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		prefix, path := sub[1], sub[2]
		key := NormalizeObjectKey(path)
		if key == "" {
			return m
		}
		return prefix + base + key
	})
}

// NormalizeCoverURL stores cover as path-only when it is a blog object; otherwise
// returns trimmed original (external http(s) covers stay absolute).
func NormalizeCoverURL(cover string) string {
	cover = strings.TrimSpace(cover)
	if cover == "" {
		return ""
	}
	if k := BlogObjectKeyFromAnyURL(cover); k != "" {
		return k
	}
	// bare path already
	if k := NormalizeObjectKey(cover); strings.HasPrefix(strings.ToLower(k), "/blog/") {
		return k
	}
	return cover
}

// ExpandCoverURL expands a stored cover (path or absolute blog URL) to the
// current public base. Non-blog covers returned as-is.
func ExpandCoverURL(cover, publicBase string) string {
	cover = strings.TrimSpace(cover)
	if cover == "" {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if k := BlogObjectKeyFromAnyURL(cover); k != "" {
		if base == "" {
			return k
		}
		return base + k
	}
	if k := NormalizeObjectKey(cover); strings.HasPrefix(strings.ToLower(k), "/blog/") {
		if base == "" {
			return k
		}
		return base + k
	}
	return cover
}

// ValidCoverInput reports whether a client-supplied cover is acceptable:
// empty, http(s) URL, or a blog object path/URL.
func ValidCoverInput(cover string) bool {
	cover = strings.TrimSpace(cover)
	if cover == "" {
		return true
	}
	if strings.HasPrefix(cover, "http://") || strings.HasPrefix(cover, "https://") {
		return true
	}
	if BlogObjectKeyFromAnyURL(cover) != "" {
		return true
	}
	k := NormalizeObjectKey(cover)
	return strings.HasPrefix(strings.ToLower(k), "/blog/")
}

// PublicURLForKey joins publicBase + object key (both optional-safe).
func PublicURLForKey(publicBase, objectKey string) string {
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	key := NormalizeObjectKey(objectKey)
	if key == "" {
		return ""
	}
	if base == "" {
		return key
	}
	return base + key
}

// splitTrailingPunct peels off trailing )]>.,; that often glue to URLs in prose.
func splitTrailingPunct(s string) (core, trail string) {
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if c == ')' || c == ']' || c == '>' || c == '"' || c == '\'' || c == ',' || c == ';' || c == '.' {
			i--
			continue
		}
		break
	}
	if i == len(s) {
		return s, ""
	}
	return s[:i], s[i:]
}
