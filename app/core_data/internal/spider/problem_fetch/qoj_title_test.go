package problem_fetch

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestExtractQOJTitle_SkipsBrandH1(t *testing.T) {
	html := `
<html><head><title>I/O Test - Problem - QOJ.ac</title></head>
<body>
  <h1>QOJ.ac</h1>
  <h1>QOJ</h1>
  <div class="uoj-content">
    <h1 class="page-header text-center"># 1. I/O Test</h1>
    <div class="problem-content"><p>Sample body</p></div>
  </div>
</body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	got := extractQOJTitle(doc, "1")
	if got != "#1. I/O Test" {
		t.Fatalf("got %q want #1. I/O Test", got)
	}
}

func TestExtractQOJTitle_FromHTMLTitle(t *testing.T) {
	html := `
<html><head><title>Tetrahedrons - Problem - QOJ.ac</title></head>
<body><h1>QOJ.ac</h1></body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	got := extractQOJTitle(doc, "100")
	if got != "#100. Tetrahedrons" {
		t.Fatalf("got %q want #100. Tetrahedrons", got)
	}
}

func TestIsQOJBrandTitle(t *testing.T) {
	for _, s := range []string{"QOJ.ac", "qoj.ac", "QOJ", "  QOJ.ac  ", ""} {
		if !IsQOJBrandTitle(s) {
			t.Fatalf("%q should be brand", s)
		}
	}
	if IsQOJBrandTitle("#19004. Foo") {
		t.Fatal("real title marked brand")
	}
}

func TestNormalizeQOJTitle(t *testing.T) {
	got := normalizeQOJTitle("# 19004.\n  Local Maxima", "19004")
	if got != "#19004. Local Maxima" {
		t.Fatalf("got %q", got)
	}
}
