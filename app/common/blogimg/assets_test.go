package blogimg

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestValidateImageConfigLimitsDimensionsAndPixels(t *testing.T) {
	tests := []struct {
		name string
		cfg  image.Config
		want error
	}{
		{name: "side boundary", cfg: image.Config{Width: 12000, Height: 1}},
		{name: "width over", cfg: image.Config{Width: 12001, Height: 1}, want: ErrImageDimensionsExceeded},
		{name: "height over", cfg: image.Config{Width: 1, Height: 12001}, want: ErrImageDimensionsExceeded},
		{name: "pixel boundary", cfg: image.Config{Width: 8000, Height: 5000}},
		{name: "pixels over", cfg: image.Config{Width: 8000, Height: 5001}, want: ErrImagePixelsExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImageConfig(tt.cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("validateImageConfig() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestExtractImageURLs(t *testing.T) {
	md := `hello ![a|550](https://cdn.example.com/a.webp) and ![b](http://x.test/b.png)
<img src="https://cdn.example.com/c.jpg">`
	urls := ExtractImageURLs(md, "https://cdn.example.com/cover.png")
	if len(urls) != 4 {
		t.Fatalf("got %v", urls)
	}
}

func TestResolveCoverURL(t *testing.T) {
	md := `text ![a](https://cdn.example.com/a.webp) ![b](https://cdn.example.com/b.webp)`
	if got := ResolveCoverURL("https://hand.example/c.png", md, true, 1024); got != "https://hand.example/c.png" {
		t.Fatalf("explicit cover: got %q", got)
	}
	if got := ResolveCoverURL("", md, true, 1024); got != "https://cdn.example.com/a.webp" {
		t.Fatalf("first image: got %q", got)
	}
	if got := ResolveCoverURL("", md, false, 1024); got != "" {
		t.Fatalf("flag off: got %q", got)
	}
	if got := ResolveCoverURL("", "no images", true, 1024); got != "" {
		t.Fatalf("no image: got %q", got)
	}
	long := "https://cdn.example.com/" + strings.Repeat("x", 2000) + ".png"
	mdLong := "![x](" + long + ")\n![ok](https://cdn.example.com/ok.webp)"
	if got := ResolveCoverURL("", mdLong, true, 1024); got != "https://cdn.example.com/ok.webp" {
		t.Fatalf("skip overlong: got %q", got)
	}
}

func TestObjectKeyFromURL(t *testing.T) {
	base := "http://zhiyuansofts.cn"
	k := ObjectKeyFromURL("http://zhiyuansofts.cn/blog/12/x.webp", base)
	if k != "/blog/12/x.webp" {
		t.Fatalf("got %q", k)
	}
	// 换 host / https 仍应识别博客对象路径（防 GC 误删）
	k2 := ObjectKeyFromURL("https://cdn.other.example/blog/27/20260730_abc.webp", base)
	if k2 != "/blog/27/20260730_abc.webp" {
		t.Fatalf("cross-host blog path: got %q", k2)
	}
	if ObjectKeyFromURL("https://free.picui.cn/x.webp", base) != "" {
		t.Fatal("third-party non-blog path should be empty")
	}
}

func TestAssetReferenced(t *testing.T) {
	key := "/blog/27/a.webp"
	url := "https://zhiyuansofts.cn/blog/27/a.webp"
	if !AssetReferenced(key, url, "hello ![x]("+url+")") {
		t.Fatal("full url")
	}
	if !AssetReferenced(key, url, "see /blog/27/a.webp in text") {
		t.Fatal("path")
	}
	if AssetReferenced(key, url, "no images here") {
		t.Fatal("should not match")
	}
}

func TestOrphanKeys(t *testing.T) {
	used := KeysFromContent(
		`![x](http://zhiyuansofts.cn/blog/1/keep.webp)`,
		"",
		"http://zhiyuansofts.cn",
	)
	reg := []string{"/blog/1/keep.webp", "/blog/1/orphan.webp", "blog/1/also.webp"}
	orphans := OrphanKeys(reg, used)
	// keep should not be orphan; orphan + also should
	found := map[string]bool{}
	for _, o := range orphans {
		found[o] = true
	}
	if found["/blog/1/keep.webp"] {
		t.Fatal("keep should not be orphan")
	}
	if !found["/blog/1/orphan.webp"] {
		t.Fatal("orphan missing")
	}
}

func TestCanUpload(t *testing.T) {
	if CanUpload(false, true) {
		t.Fatal("need both")
	}
	if CanUpload(true, false) {
		t.Fatal("need both")
	}
	if !CanUpload(true, true) {
		t.Fatal("should allow")
	}
}

func TestCompressPassthroughSmallPNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	res, err := CompressForUpload(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passthrough {
		t.Fatal("small png should passthrough")
	}
	if len(res.Data) == 0 {
		t.Fatal("empty")
	}
}

func TestCompressLargeJPEGUnderCap(t *testing.T) {
	// large solid image → re-encode
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 400; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 80, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	// force non-passtrough by size path: use image/jpeg content type with png bytes still decodable?
	// Compress decodes via image.Decode which handles png magic.
	res, err := CompressForUpload(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data) == 0 || len(res.Data) > MaxUploadBytes {
		t.Fatalf("bad size %d", len(res.Data))
	}
	// output must still be valid image bytes (jpeg or png)
	_, _, err = image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("output not decodable: %v", err)
	}
}
