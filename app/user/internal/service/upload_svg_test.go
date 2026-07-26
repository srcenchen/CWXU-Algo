package service

import "testing"

func TestLooksLikeSVG(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"plain", `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`, true},
		{"xml decl", "<?xml version=\"1.0\"?>\n<svg viewBox=\"0 0 1 1\"></svg>", true},
		{"doctype", "<!DOCTYPE svg PUBLIC \"-//W3C//DTD SVG 1.1//EN\" \"x.dtd\">\n<svg></svg>", true},
		{"comment", "<!-- made by x -->\n<svg></svg>", true},
		{"bom", "\xef\xbb\xbf<svg></svg>", true},
		{"uppercase", `<SVG xmlns="http://www.w3.org/2000/svg"></SVG>`, true},
		{"html wrapper", `<html><body><svg></svg></body></html>`, false},
		{"not svg", `{"hello":"world"}`, false},
		{"empty", ``, false},
	}
	for _, c := range cases {
		if got := looksLikeSVG([]byte(c.data)); got != c.want {
			t.Errorf("%s: looksLikeSVG = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSafeSVG(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"clean", `<svg><path d="M0 0 1 1" fill="#fff"/></svg>`, true},
		{"gradient + mask", `<svg><defs><linearGradient id="a"/><mask id="b"/></defs><rect mask="url(#b)"/></svg>`, true},
		{"internal use", `<svg><defs><g id="m"/></defs><use href="#m"/></svg>`, true},
		{"script", `<svg><script>alert(1)</script></svg>`, false},
		{"onload", `<svg onload="alert(1)"></svg>`, false},
		{"onload spaced", `<svg onload = "alert(1)"></svg>`, false},
		{"onclick uppercase", `<svg><rect onClick="x()"/></svg>`, false},
		{"foreignObject", `<svg><foreignObject><body/></foreignObject></svg>`, false},
		{"javascript href", `<svg><a href="javascript:alert(1)"><rect/></a></svg>`, false},
		{"data html", `<svg><image href="data:text/html,<script>x</script>"/></svg>`, false},
		{"iframe", `<svg><iframe src="/x"/></svg>`, false},
		// "on" 出现在普通属性名/正文里不应误伤
		{"font shorthand", `<svg><text font="12px Inter">hi</text></svg>`, true},
		{"font-family", `<svg><text font-family="Inter">on the way</text></svg>`, true},
		{"orientation attr", `<svg><marker orientation="auto"/></svg>`, true},
		{"text content", `<svg><text>turn on = go</text></svg>`, true},
	}
	for _, c := range cases {
		if got := safeSVG([]byte(c.data)); got != c.want {
			t.Errorf("%s: safeSVG = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestValidImageDataSVG(t *testing.T) {
	ok := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><circle r="8"/></svg>`)
	if !validImageData(ok, "image/svg+xml") {
		t.Error("clean svg should pass validImageData")
	}
	bad := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>fetch("/steal")</script></svg>`)
	if validImageData(bad, "image/svg+xml") {
		t.Error("svg with script must be rejected")
	}
	if !allowedImage("image/svg+xml") {
		t.Error("image/svg+xml should be an allowed content type")
	}
	if contentTypeFromExt(".svg") != "image/svg+xml" {
		t.Error("contentTypeFromExt(.svg) should be image/svg+xml")
	}
	if !isImageExt(".svg") {
		t.Error(".svg should be servable by the static handler")
	}
}
