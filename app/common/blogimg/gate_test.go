package blogimg_test

import (
	"testing"

	"cwxu-algo/app/common/blogimg"
)

// Upload gate pure function — mirrors server check before UpYun I/O.
func TestUploadGateMatrix(t *testing.T) {
	cases := []struct {
		configured, authorized, want bool
	}{
		{false, false, false},
		{false, true, false},
		{true, false, false},
		{true, true, true},
	}
	for _, c := range cases {
		got := blogimg.CanUpload(c.configured, c.authorized)
		if got != c.want {
			t.Fatalf("CanUpload(%v,%v)=%v want %v", c.configured, c.authorized, got, c.want)
		}
	}
}

func TestGCDiffDoesNotDeleteReferenced(t *testing.T) {
	base := "http://zhiyuansofts.cn"
	content := `![a|200](http://zhiyuansofts.cn/blog/9/keep.webp)
![b](https://free.picui.cn/external.webp)`
	cover := "http://zhiyuansofts.cn/blog/9/cover.jpg"
	used := blogimg.KeysFromContent(content, cover, base)
	reg := []string{
		"/blog/9/keep.webp",
		"/blog/9/cover.jpg",
		"/blog/9/orphan.webp",
	}
	orphans := blogimg.OrphanKeys(reg, used)
	if len(orphans) != 1 || orphans[0] != "/blog/9/orphan.webp" {
		t.Fatalf("orphans=%v", orphans)
	}
	// external never registered → not in orphan list
	for _, o := range orphans {
		if o == "/external.webp" || o == "https://free.picui.cn/external.webp" {
			t.Fatal("must not GC third-party")
		}
	}
}
