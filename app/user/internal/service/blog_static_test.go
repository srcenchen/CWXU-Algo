package service

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/gorilla/mux"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackStaticZipIndex(t *testing.T) {
	raw := zipOf(t, map[string]string{
		"index.html": "<html><body>hi</body></html>",
		"css/a.css":  "body{color:red}",
	})
	files, msg := unpackStaticZip(raw)
	if msg != "" {
		t.Fatal(msg)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d", len(files))
	}
	if got := defaultStaticEntry(files); got != "index.html" {
		t.Fatalf("entry=%s", got)
	}
}

func TestUnpackStaticZipRejectsTraversal(t *testing.T) {
	raw := zipOf(t, map[string]string{"../etc/passwd": "x"})
	_, msg := unpackStaticZip(raw)
	if msg == "" {
		t.Fatal("expected reject")
	}
}

func TestNormalizeStaticSlug(t *testing.T) {
	if _, msg := normalizeStaticSlug("static"); msg == "" {
		t.Fatal("reserved")
	}
	if _, msg := normalizeStaticSlug("My-Page"); msg != "" {
		t.Fatal(msg)
	}
	slug, msg := normalizeStaticSlug("hello-1")
	if msg != "" || slug != "hello-1" {
		t.Fatalf("slug=%s msg=%s", slug, msg)
	}
}

func TestStaticSitePublicNavQuery(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.BlogStaticSite{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "sanen", Password: "x", Email: "a@b.c"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	rows := []model.BlogStaticSite{
		{UserID: user.ID, Title: "简历", Slug: "cv", Entry: "index.html", Prefix: "/blog-static/1/1", ShowInNav: true, NavLabel: "简历"},
		{UserID: user.ID, Title: "草稿", Slug: "draft", Entry: "index.html", Prefix: "/blog-static/1/2", ShowInNav: false},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var list []model.BlogStaticSite
	if err := db.Where("user_id = ? AND show_in_nav = ?", user.ID, true).Find(&list).Error; err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Slug != "cv" {
		t.Fatalf("nav list=%+v", list)
	}
	svc := &BlogService{db: db}
	info := svc.staticSiteToProto(&list[0], user.Username)
	if info.PublicPath != "/blog/sanen/static/cv/" || info.NavLabel != "简历" {
		t.Fatalf("info=%+v", info)
	}
}

func TestServeBlogStaticRedirectsToEntry(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.BlogStaticSite{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "sanen", Password: "x", Email: "a@b.c"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BlogStaticSite{
		UserID: user.ID, Title: "页", Slug: "hello", Entry: "index.html",
		Prefix: "/blog-static/1/1",
	}).Error; err != nil {
		t.Fatal(err)
	}
	d := &data.Data{DB: db}
	r := mux.NewRouter()
	r.HandleFunc("/v1/user/blog/static/{username}/{slug}", func(w http.ResponseWriter, req *http.Request) {
		req = mux.SetURLVars(req, map[string]string{"username": "sanen", "slug": "hello"})
		ctx := &staticHTTPCtx{req: req, w: w}
		_ = serveBlogStaticCtx(d, ctx)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Get(srv.URL + "/v1/user/blog/static/sanen/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusFound || !strings.HasSuffix(loc, "/blog/sanen/static/hello/index.html") {
		t.Fatalf("status=%d loc=%s", res.StatusCode, loc)
	}
}

type staticHTTPCtx struct {
	req *http.Request
	w   http.ResponseWriter
}

func (c *staticHTTPCtx) Vars() url.Values {
	raw := mux.Vars(c.req)
	out := make(url.Values, len(raw))
	for k, v := range raw {
		out.Set(k, v)
	}
	return out
}

func (c *staticHTTPCtx) Request() *http.Request        { return c.req }
func (c *staticHTTPCtx) Response() http.ResponseWriter { return c.w }
func (c *staticHTTPCtx) JSON(code int, v interface{}) error {
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(code)
	return nil
}

func TestBlogStaticFromPath(t *testing.T) {
	cases := []struct {
		in   string
		user string
		slug string
		rel  string
		ok   bool
	}{
		{"/blog/sanen/static/hello/", "sanen", "hello", "", true},
		{"/blog/sanen/static/hello", "sanen", "hello", "", true},
		{"/blog/sanen/static/hello/index.html", "sanen", "hello", "index.html", true},
		{"/blog/sanen/static/hello/css/a.css?v=1", "sanen", "hello", "css/a.css", true},
		{"/blog/sanen/manage/static", "", "", "", false},
		{"/blog/sanen/cv", "", "", "", false},
		{"/blog/sanen/static", "", "", "", false},
	}
	for _, c := range cases {
		user, slug, rel, ok := blogStaticFromPath(c.in)
		if ok != c.ok || user != c.user || slug != c.slug || rel != c.rel {
			t.Fatalf("%s => (%s,%s,%s,%v)", c.in, user, slug, rel, ok)
		}
	}
}

func TestSEOFallbackServesBlogStatic(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.BlogStaticSite{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "sanen", Password: "x", Email: "a@b.c"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BlogStaticSite{
		UserID: user.ID, Title: "页", Slug: "hello", Entry: "index.html",
		Prefix: "/blog-static/1/1",
	}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &SEOService{data: &data.Data{DB: db}, pageCache: map[string]seoPageCacheEntry{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/blog/sanen/static/hello/", nil)
	ctx := &staticHTTPCtx{req: req, w: rec}
	handled, err := svc.tryServeBlogStatic(ctx, "/blog/sanen/static/hello/")
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("expected static path to be handled by SEO fallback")
	}
	if rec.Code != http.StatusFound || !strings.HasSuffix(rec.Header().Get("Location"), "/blog/sanen/static/hello/index.html") {
		t.Fatalf("code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestDefaultStaticEntryNestedOnce(t *testing.T) {
	raw := zipOf(t, map[string]string{
		"deck/index.html": "<html>ok</html>",
		"deck/a.css":      "body{}",
	})
	files, msg := unpackStaticZip(raw)
	if msg != "" {
		t.Fatal(msg)
	}
	if got := defaultStaticEntry(files); got != "deck/index.html" {
		t.Fatalf("entry=%s", got)
	}
	raw = zipOf(t, map[string]string{
		"a/index.html": "<html></html>",
		"b/index.html": "<html></html>",
	})
	files, msg = unpackStaticZip(raw)
	if msg != "" {
		t.Fatal(msg)
	}
	if got := defaultStaticEntry(files); got != "" {
		t.Fatalf("two roots should not unwrap, got %s", got)
	}
}

func TestRejectPHPInHTML(t *testing.T) {
	raw := zipOf(t, map[string]string{"index.html": "<?php echo 1; ?>"})
	_, msg := unpackStaticZip(raw)
	if !strings.Contains(msg, "脚本") {
		t.Fatalf("msg=%s", msg)
	}
}
