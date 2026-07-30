package blogimg

import (
	"strings"
	"testing"
)

func TestNormalizeStoredImageRefs(t *testing.T) {
	in := `pic ![a](http://old.zhiyuansofts.cn/blog/12/a.webp) and <img src="https://algo.zhiyuansofts.cn/blog/12/b.png"> ext ![x](https://free.picui.cn/z.webp)`
	got := NormalizeStoredImageRefs(in)
	if !strings.Contains(got, `![a](/blog/12/a.webp)`) {
		t.Fatalf("md not normalized: %s", got)
	}
	if !strings.Contains(got, `src="/blog/12/b.png"`) {
		t.Fatalf("html not normalized: %s", got)
	}
	if !strings.Contains(got, `https://free.picui.cn/z.webp`) {
		t.Fatalf("external should stay: %s", got)
	}
	// idempotent
	if again := NormalizeStoredImageRefs(got); again != got {
		t.Fatalf("not idempotent:\n%s\n%s", got, again)
	}
}

func TestExpandStoredImageRefs(t *testing.T) {
	base := "https://cdn.example.com"
	stored := `![a](/blog/12/a.webp) legacy ![b](http://old.zhiyuansofts.cn/blog/12/b.webp) out ![x](https://free.picui.cn/z.webp)`
	got := ExpandStoredImageRefs(stored, base)
	if !strings.Contains(got, `![a](https://cdn.example.com/blog/12/a.webp)`) {
		t.Fatalf("path expand: %s", got)
	}
	if !strings.Contains(got, `![b](https://cdn.example.com/blog/12/b.webp)`) {
		t.Fatalf("legacy host rewrite: %s", got)
	}
	if !strings.Contains(got, `https://free.picui.cn/z.webp`) {
		t.Fatalf("external: %s", got)
	}
	if ExpandStoredImageRefs(stored, "") != stored {
		t.Fatal("empty base should no-op")
	}
}

func TestNormalizeExpandCoverURL(t *testing.T) {
	if got := NormalizeCoverURL("https://old.zhiyuansofts.cn/blog/9/c.webp"); got != "/blog/9/c.webp" {
		t.Fatalf("norm cover: %q", got)
	}
	if got := NormalizeCoverURL("https://cdn.other/photo.jpg"); got != "https://cdn.other/photo.jpg" {
		t.Fatalf("external cover: %q", got)
	}
	if got := ExpandCoverURL("/blog/9/c.webp", "https://cdn.new"); got != "https://cdn.new/blog/9/c.webp" {
		t.Fatalf("expand cover: %q", got)
	}
	if got := ExpandCoverURL("http://old.zhiyuansofts.cn/blog/9/c.webp", "https://cdn.new"); got != "https://cdn.new/blog/9/c.webp" {
		t.Fatalf("expand abs cover: %q", got)
	}
}

func TestValidCoverInput(t *testing.T) {
	if !ValidCoverInput("") || !ValidCoverInput("https://x/y") || !ValidCoverInput("/blog/1/a.webp") {
		t.Fatal("should accept empty, http, path")
	}
	if ValidCoverInput("file://x") || ValidCoverInput("not-a-url") {
		t.Fatal("should reject")
	}
}

func TestExtractImageURLsIncludesPathOnly(t *testing.T) {
	md := `![a](/blog/1/a.webp) ![b](https://cdn/blog/1/b.webp)`
	urls := ExtractImageURLs(md, "/blog/1/cover.webp")
	if len(urls) < 3 {
		t.Fatalf("expected path+http covers, got %v", urls)
	}
	foundPath := false
	for _, u := range urls {
		if u == "/blog/1/a.webp" || u == "/blog/1/cover.webp" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("path-only missing: %v", urls)
	}
}

func TestResolveCoverURLPathOnly(t *testing.T) {
	md := `text ![a](/blog/3/first.webp)`
	if got := ResolveCoverURL("", md, true, 1024); got != "/blog/3/first.webp" {
		t.Fatalf("first path image: %q", got)
	}
}

func TestPublicURLForKey(t *testing.T) {
	if got := PublicURLForKey("https://cdn.x/", "/blog/1/a.webp"); got != "https://cdn.x/blog/1/a.webp" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeStoredImageRefsOnlyTrustedImageSyntax(t *testing.T) {
	in := strings.Join([]string{
		`![trusted](https://zhiyuansofts.cn/blog/12/a.webp)`,
		`<img src="https://zhiyuansofts.cn/blog/12/b.png">`,
		`<img src=https://zhiyuansofts.cn/blog/12/unquoted.png>`,
		`![external](https://evil.example/blog/12/c.webp)`,
		`![trusted-nonblog](https://zhiyuansofts.cn/assets/logo.png)`,
		`plain https://zhiyuansofts.cn/blog/12/plain.webp`,
		"`![inline](https://zhiyuansofts.cn/blog/12/inline.webp)`",
		"```md\n![fenced](https://zhiyuansofts.cn/blog/12/fenced.webp)\n```",
	}, "\n")

	got := NormalizeStoredImageRefs(in)
	if !strings.Contains(got, `![trusted](/blog/12/a.webp)`) || !strings.Contains(got, `src="/blog/12/b.png"`) ||
		!strings.Contains(got, `src=/blog/12/unquoted.png`) {
		t.Fatalf("trusted image refs were not normalized:\n%s", got)
	}
	for _, unchanged := range []string{
		`![external](https://evil.example/blog/12/c.webp)`,
		`![trusted-nonblog](https://zhiyuansofts.cn/assets/logo.png)`,
		`plain https://zhiyuansofts.cn/blog/12/plain.webp`,
		"`![inline](https://zhiyuansofts.cn/blog/12/inline.webp)`",
		`![fenced](https://zhiyuansofts.cn/blog/12/fenced.webp)`,
	} {
		if !strings.Contains(got, unchanged) {
			t.Fatalf("non-target reference changed: %q\n%s", unchanged, got)
		}
	}
}

func TestExpandStoredImageRefsOnlyImageSyntax(t *testing.T) {
	in := strings.Join([]string{
		`![image](/blog/12/a.webp)`,
		`<img src='/blog/12/b.png'>`,
		`plain /blog/12/plain.webp`,
		"`![inline](/blog/12/inline.webp)`",
	}, "\n")
	got := ExpandStoredImageRefs(in, "https://cdn.example.com")
	if !strings.Contains(got, `![image](https://cdn.example.com/blog/12/a.webp)`) ||
		!strings.Contains(got, `src='https://cdn.example.com/blog/12/b.png'`) {
		t.Fatalf("image refs were not expanded:\n%s", got)
	}
	if !strings.Contains(got, `plain /blog/12/plain.webp`) ||
		!strings.Contains(got, "`![inline](/blog/12/inline.webp)`") {
		t.Fatalf("plain/code refs changed:\n%s", got)
	}
}

func TestNormalizeStoredImageRefsPreservesEscapedAndCodeSyntax(t *testing.T) {
	trusted := "https://zhiyuansofts.cn/blog/12/x.webp"
	in := strings.Join([]string{
		`\![escaped](` + trusted + `)`,
		`    ![indented](` + trusted + `)`,
		"````md\n![four](" + trusted + ")\n```\n![still-four](" + trusted + ")\n````",
		"~~~md\n![tilde](" + trusted + ")\n```\n![still-tilde](" + trusted + ")\n~~~",
	}, "\n")
	if got := NormalizeStoredImageRefs(in); got != in {
		t.Fatalf("escaped or code syntax changed:\n%s", got)
	}
}

func TestNormalizeStoredImageRefsPreservesRawHTMLCodeContainers(t *testing.T) {
	trusted := "https://zhiyuansofts.cn/blog/12/x.webp"
	in := strings.Join([]string{
		`<pre>![pre](` + trusted + `)<img src="` + trusted + `"></pre>`,
		`<code>![code](` + trusted + `)</code>`,
		"<script>\nconst x = '<img src=\"" + trusted + "\">';\n</script>",
	}, "\n")
	if got := NormalizeStoredImageRefs(in); got != in {
		t.Fatalf("raw HTML code container changed:\n%s", got)
	}
}

func TestNormalizeStoredImageRefsPreservesAngleDestinationFormatting(t *testing.T) {
	in := `![angle](<https://zhiyuansofts.cn/blog/12/a.webp> "title")`
	want := `![angle](</blog/12/a.webp> "title")`
	if got := NormalizeStoredImageRefs(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNormalizeStoredImageRefsPreservesSpacesInAngleDestination(t *testing.T) {
	in := `![angle](<https://zhiyuansofts.cn/blog/12/a b.webp> "title")`
	want := `![angle](</blog/12/a b.webp> "title")`
	if got := NormalizeStoredImageRefs(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNormalizeStoredImageRefsPreservesHTMLComments(t *testing.T) {
	in := strings.Join([]string{
		`<!-- <img src="https://zhiyuansofts.cn/blog/12/comment.webp"> -->`,
		"<!--\n<img src=\"https://zhiyuansofts.cn/blog/12/multiline.webp\">\n-->",
		`<img src="https://zhiyuansofts.cn/blog/12/live.webp">`,
	}, "\n")
	want := strings.Replace(in, `https://zhiyuansofts.cn/blog/12/live.webp`, `/blog/12/live.webp`, 1)
	if got := NormalizeStoredImageRefs(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNormalizeStoredImageRefsPreservesStyleRawText(t *testing.T) {
	in := `<style>.x::after { content: '<img src="https://zhiyuansofts.cn/blog/12/css.webp">'; }</style>`
	if got := NormalizeStoredImageRefs(in); got != in {
		t.Fatalf("style raw text changed: %s", got)
	}
}
