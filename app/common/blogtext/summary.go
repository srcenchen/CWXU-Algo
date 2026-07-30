// Package blogtext holds pure blog/solution text helpers (summary, surface rules)
// shared by user service, blogsync, and core_data community without import cycles.
package blogtext

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// VisibilityPublic is the only visibility that auto-surfaces.
const VisibilityPublic = "public"

// AutoSurface reports whether a public non-password article should auto-appear
// on discovery recommend, plaza, and author-org surfaces.
func AutoSurface(visibility string) bool {
	v := strings.ToLower(strings.TrimSpace(visibility))
	if v == "" {
		v = VisibilityPublic
	}
	return v == VisibilityPublic
}

// Default summary generation — **single source** for:
// blog 简述、题解镜像、发现流 / 题解列表 excerpt、评论预览等。
const (
	// DefaultSummaryMaxRunes is the max length of a system-generated 简述.
	DefaultSummaryMaxRunes = 280
	defaultSummaryEllipsis = "…"
)

var (
	reSummaryCodeFence   = regexp.MustCompile("(?s)```[\\w-]*\\n?(.*?)```")
	reSummaryInlineCode  = regexp.MustCompile("`([^`]+)`")
	reSummaryImage       = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reSummaryLink        = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	reSummaryAutolink    = regexp.MustCompile(`<(https?://[^>\s]+)>`)
	reSummaryHeading     = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	reSummaryQuote       = regexp.MustCompile(`(?m)^>\s?`)
	reSummaryUL          = regexp.MustCompile(`(?m)^\s*[-*+]\s+`)
	reSummaryOL          = regexp.MustCompile(`(?m)^\s*\d+\.\s+`)
	reSummaryBoldStar    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reSummaryBoldUnder   = regexp.MustCompile(`__(.+?)__`)
	reSummaryItalicStar  = regexp.MustCompile(`\*([^*\n]+)\*`)
	reSummaryItalicUnder = regexp.MustCompile(`_([^_\n]+)_`)
	reSummaryStrike      = regexp.MustCompile(`~~(.+?)~~`)
	reSummaryDisplayMath = regexp.MustCompile(`(?s)\$\$(.+?)\$\$`)
	reSummaryInlineMath  = regexp.MustCompile(`\$(.+?)\$`)
	reSummaryParenMath   = regexp.MustCompile(`(?s)\\\((.+?)\\\)`)
	reSummaryBracketMath = regexp.MustCompile(`(?s)\\\[(.+?)\\\]`)
	reSummaryWikiLink    = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	reSummaryHTMLTag     = regexp.MustCompile(`(?is)<[^>]+>`)
	reSummarySpaces      = regexp.MustCompile(`[ \t\r\n\f\v]+`)
)

// DefaultSummary builds a content-derived brief for list cards (blog + recommend + solutions).
func DefaultSummary(content string) string {
	return Excerpt(content, DefaultSummaryMaxRunes)
}

// Excerpt strips Markdown/noise then truncates to max runes (shared by all list previews).
// max <= 0 uses DefaultSummaryMaxRunes.
func Excerpt(content string, max int) string {
	if max <= 0 {
		max = DefaultSummaryMaxRunes
	}
	s := PlainText(content)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := max
	window := runes[:cut]
	// Prefer breaking at sentence/clause punctuation near the end
	for i := len(window) - 1; i >= cut-40 && i >= 0; i-- {
		switch window[i] {
		case '。', '！', '？', '；', '.', '!', '?', ';', '，', ',':
			return string(window[:i+1]) + defaultSummaryEllipsis
		}
	}
	return string(window) + defaultSummaryEllipsis
}

// PlainText peels Markdown/HTML/math markers to a single-line readable string.
// Used by Excerpt and any caller that only needs de-markdown text.
func PlainText(content string) string {
	s := strings.ReplaceAll(content, "\r\n", "\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// HTML 粗剥
	if strings.Contains(s, "<") && strings.Contains(s, ">") {
		s = strings.ReplaceAll(s, "<br>", "\n")
		s = strings.ReplaceAll(s, "<br/>", "\n")
		s = strings.ReplaceAll(s, "<br />", "\n")
		s = strings.ReplaceAll(s, "</p>", "\n")
		s = reSummaryHTMLTag.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, "&nbsp;", " ")
		s = strings.ReplaceAll(s, "&lt;", "<")
		s = strings.ReplaceAll(s, "&gt;", ">")
		s = strings.ReplaceAll(s, "&amp;", "&")
		s = strings.ReplaceAll(s, "&quot;", "\"")
	}

	// 整段代码块直接丢掉（列表预览不需要代码）
	s = stripFencedCodeBlocks(s)
	// 兜底：残留 fence
	s = reSummaryCodeFence.ReplaceAllString(s, " ")

	s = reSummaryDisplayMath.ReplaceAllString(s, " $1 ")
	s = reSummaryBracketMath.ReplaceAllString(s, " $1 ")
	s = reSummaryParenMath.ReplaceAllString(s, " $1 ")
	s = reSummaryInlineMath.ReplaceAllString(s, " $1 ")
	s = reSummaryInlineCode.ReplaceAllString(s, "$1")
	s = reSummaryImage.ReplaceAllString(s, "")
	s = reSummaryLink.ReplaceAllString(s, "$1")
	s = reSummaryAutolink.ReplaceAllString(s, "$1")
	s = reSummaryWikiLink.ReplaceAllString(s, "$1")
	s = reSummaryHeading.ReplaceAllString(s, "")
	s = reSummaryQuote.ReplaceAllString(s, "")
	s = reSummaryUL.ReplaceAllString(s, "")
	s = reSummaryOL.ReplaceAllString(s, "")
	s = reSummaryBoldStar.ReplaceAllString(s, "$1")
	s = reSummaryBoldUnder.ReplaceAllString(s, "$1")
	s = reSummaryItalicStar.ReplaceAllString(s, "$1")
	s = reSummaryItalicUnder.ReplaceAllString(s, "$1")
	s = reSummaryStrike.ReplaceAllString(s, "$1")
	s = reSummarySpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// IsDefaultSummary reports whether summary is system-generated (always true now:
// hand-written summaries are no longer accepted on save).
func IsDefaultSummary(summary, content string) bool {
	_ = summary
	_ = content
	return true
}

// ResolveSummaryForSave always regenerates from content; userSummary is ignored.
// Kept for call-site compatibility (API may still send a summary field).
func ResolveSummaryForSave(userSummary, content string) string {
	_ = userSummary
	return DefaultSummary(content)
}

func stripFencedCodeBlocks(s string) string {
	var b strings.Builder
	lines := strings.Split(s, "\n")
	inFence := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// RuneCount is a tiny helper for tests / callers.
func RuneCount(s string) int {
	return utf8.RuneCountInString(s)
}
