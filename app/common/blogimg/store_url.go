package blogimg

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	markdownImageRefRE = regexp.MustCompile(`(!\[[^\]\r\n]*\]\(\s*)(<[^>\r\n]*>|[^\s)>]+)([^\r\n)]*\))`)
	htmlImageTagRE     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	htmlImageSrcRE     = regexp.MustCompile(`(?i)(\bsrc\s*=\s*['"])([^'"]+)(['"])`)
	htmlImageBareSrcRE = regexp.MustCompile(`(?i)(\bsrc\s*=\s*)([^\s'"=<>` + "`" + `]+)`)
	rawCodeOpenRE      = regexp.MustCompile(`(?i)<(pre|code|script|style)\b[^>]*>`)
)

func trustedBlogImageHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == "zhiyuansofts.cn" || strings.HasSuffix(host, ".zhiyuansofts.cn")
}

func trustedBlogObjectKey(raw string) string {
	raw = strings.TrimSpace(strings.Trim(raw, "<>"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || !trustedBlogImageHost(u.Hostname()) {
		return ""
	}
	key := NormalizeObjectKey(u.Path)
	if !strings.HasPrefix(strings.ToLower(key), "/blog/") {
		return ""
	}
	return key
}

func rewriteImageURL(raw string, expand bool, publicBase string) string {
	wrapped := strings.HasPrefix(raw, "<") && strings.HasSuffix(raw, ">")
	value := strings.Trim(raw, "<>")
	key := ""
	if strings.HasPrefix(value, "/blog/") {
		key = BlogObjectKeyFromAnyURL(value)
	} else {
		key = trustedBlogObjectKey(value)
	}
	if key == "" {
		return raw
	}
	if expand {
		value = strings.TrimRight(publicBase, "/") + key
	} else {
		value = key
	}
	if wrapped {
		return "<" + value + ">"
	}
	return value
}

func rewriteImageSyntax(segment string, expand bool, publicBase string) string {
	var markdownOut strings.Builder
	last := 0
	for _, loc := range markdownImageRefRE.FindAllStringIndex(segment, -1) {
		backslashes := 0
		for i := loc[0] - 1; i >= 0 && segment[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continue
		}
		match := segment[loc[0]:loc[1]]
		parts := markdownImageRefRE.FindStringSubmatch(match)
		markdownOut.WriteString(segment[last:loc[0]])
		markdownOut.WriteString(parts[1] + rewriteImageURL(parts[2], expand, publicBase) + parts[3])
		last = loc[1]
	}
	markdownOut.WriteString(segment[last:])
	segment = markdownOut.String()
	return htmlImageTagRE.ReplaceAllStringFunc(segment, func(tag string) string {
		tag = htmlImageSrcRE.ReplaceAllStringFunc(tag, func(match string) string {
			parts := htmlImageSrcRE.FindStringSubmatch(match)
			return parts[1] + rewriteImageURL(parts[2], expand, publicBase) + parts[3]
		})
		return htmlImageBareSrcRE.ReplaceAllStringFunc(tag, func(match string) string {
			parts := htmlImageBareSrcRE.FindStringSubmatch(match)
			return parts[1] + rewriteImageURL(parts[2], expand, publicBase)
		})
	})
}

func rewriteOutsideInlineCode(line string, expand bool, publicBase string) string {
	var out strings.Builder
	for len(line) > 0 {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			out.WriteString(rewriteImageSyntax(line, expand, publicBase))
			break
		}
		out.WriteString(rewriteImageSyntax(line[:start], expand, publicBase))
		run := 1
		for start+run < len(line) && line[start+run] == '`' {
			run++
		}
		delim := strings.Repeat("`", run)
		rest := line[start+run:]
		end := strings.Index(rest, delim)
		if end < 0 {
			out.WriteString(line[start:])
			break
		}
		out.WriteString(line[start : start+run+end+run])
		line = rest[end+run:]
	}
	return out.String()
}

func rewriteOutsideRawCode(line string, rawTag *string, expand bool, publicBase string) string {
	var out strings.Builder
	for line != "" {
		if *rawTag != "" {
			closeTag := "</" + *rawTag + ">"
			if *rawTag == "!--" {
				closeTag = "-->"
			}
			at := strings.Index(strings.ToLower(line), closeTag)
			if at < 0 {
				out.WriteString(line)
				break
			}
			end := at + len(closeTag)
			out.WriteString(line[:end])
			line = line[end:]
			*rawTag = ""
			continue
		}
		loc := rawCodeOpenRE.FindStringSubmatchIndex(line)
		commentAt := strings.Index(line, "<!--")
		if loc == nil && commentAt < 0 {
			out.WriteString(rewriteOutsideInlineCode(line, expand, publicBase))
			break
		}
		if commentAt >= 0 && (loc == nil || commentAt < loc[0]) {
			out.WriteString(rewriteOutsideInlineCode(line[:commentAt], expand, publicBase))
			out.WriteString("<!--")
			*rawTag = "!--"
			line = line[commentAt+4:]
			continue
		}
		out.WriteString(rewriteOutsideInlineCode(line[:loc[0]], expand, publicBase))
		out.WriteString(line[loc[0]:loc[1]])
		*rawTag = strings.ToLower(line[loc[2]:loc[3]])
		line = line[loc[1]:]
	}
	return out.String()
}

func fenceRun(line string) (byte, int, string, bool) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	indent := 0
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent >= len(line) || (line[indent] != '`' && line[indent] != '~') {
		return 0, 0, "", false
	}
	char := line[indent]
	run := 1
	for indent+run < len(line) && line[indent+run] == char {
		run++
	}
	if run < 3 {
		return 0, 0, "", false
	}
	return char, run, line[indent+run:], true
}

// rewriteOutsideCode applies image-reference rewriting only to rendered Markdown.
func rewriteOutsideCode(text string, expand bool, publicBase string) string {
	lines := strings.SplitAfter(text, "\n")
	var fenceChar byte
	fenceLen := 0
	rawTag := ""
	for i, line := range lines {
		char, run, rest, fence := fenceRun(line)
		if fenceLen > 0 {
			if fence && char == fenceChar && run >= fenceLen && strings.TrimSpace(rest) == "" {
				fenceChar, fenceLen = 0, 0
			}
			continue
		}
		if rawTag == "" && fence {
			fenceChar, fenceLen = char, run
			continue
		}
		if rawTag == "" && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		lines[i] = rewriteOutsideRawCode(line, &rawTag, expand, publicBase)
	}
	return strings.Join(lines, "")
}

// NormalizeStoredImageRefs rewrites absolute blog-object image URLs to path-only
// form (/blog/{uid}/file). External links and non-blog paths are left unchanged.
// Idempotent: already path-only refs stay path-only.
func NormalizeStoredImageRefs(text string) string {
	if text == "" || !strings.Contains(text, "/blog/") {
		return text
	}
	return rewriteOutsideCode(text, false, "")
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
	return rewriteOutsideCode(text, true, base)
}

// NormalizeCoverURL stores cover as path-only when it is a blog object; otherwise
// returns trimmed original (external http(s) covers stay absolute).
func NormalizeCoverURL(cover string) string {
	cover = strings.TrimSpace(cover)
	if cover == "" {
		return ""
	}
	if k := trustedBlogObjectKey(cover); k != "" {
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
	if k := trustedBlogObjectKey(cover); k != "" {
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
	if trustedBlogObjectKey(cover) != "" {
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
