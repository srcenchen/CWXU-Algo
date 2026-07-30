package blogimg

import (
	"strings"
	"testing"
)

func TestNormalizeStoredImageRefs(t *testing.T) {
	in := `pic ![a](http://old.cdn/blog/12/a.webp) and <img src="https://other/blog/12/b.png"> ext ![x](https://free.picui.cn/z.webp)`
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
	stored := `![a](/blog/12/a.webp) legacy ![b](http://old.host/blog/12/b.webp) out ![x](https://free.picui.cn/z.webp)`
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
	if got := NormalizeCoverURL("https://old/blog/9/c.webp"); got != "/blog/9/c.webp" {
		t.Fatalf("norm cover: %q", got)
	}
	if got := NormalizeCoverURL("https://cdn.other/photo.jpg"); got != "https://cdn.other/photo.jpg" {
		t.Fatalf("external cover: %q", got)
	}
	if got := ExpandCoverURL("/blog/9/c.webp", "https://cdn.new"); got != "https://cdn.new/blog/9/c.webp" {
		t.Fatalf("expand cover: %q", got)
	}
	if got := ExpandCoverURL("http://old/blog/9/c.webp", "https://cdn.new"); got != "https://cdn.new/blog/9/c.webp" {
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
