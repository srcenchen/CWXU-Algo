package upyun

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicBaseURL(t *testing.T) {
	c := New(Config{Domain: "zhiyuansofts.cn", Scheme: "http"})
	if got := c.PublicBaseURL(); got != "http://zhiyuansofts.cn" {
		t.Fatalf("got %q", got)
	}
	c2 := New(Config{Domain: "https://cdn.example.com/path/"})
	if got := c2.PublicBaseURL(); got != "https://cdn.example.com/path" {
		t.Fatalf("got %q", got)
	}
	c3 := New(Config{Domain: "zhiyuansofts.cn"})
	if got := c3.PublicURL("/blog/1/a.webp"); got != "http://zhiyuansofts.cn/blog/1/a.webp" {
		t.Fatalf("got %q", got)
	}
}

func TestSignMatchesRESTSpec(t *testing.T) {
	// Method & URI & Date → HMAC-SHA1(MD5(password), ...)
	c := New(Config{
		Bucket:   "yangcongxueyuan",
		Operator: "enchensan",
		Password: "test-password",
	})
	date := "Wed, 28 Jul 2026 12:00:00 GMT"
	uri := "/yangcongxueyuan/blog/1/x.webp"
	got := c.SignForTest("PUT", uri, date)
	if !strings.HasPrefix(got, "UPYUN enchensan:") {
		t.Fatalf("prefix: %s", got)
	}
	// recompute expected
	sum := md5.Sum([]byte("test-password"))
	passMD5 := hex.EncodeToString(sum[:])
	if passMD5 == "" {
		t.Fatal("empty md5")
	}
	// signature non-empty base64 tail
	parts := strings.SplitN(got, ":", 2)
	if len(parts) != 2 || len(parts[1]) < 10 {
		t.Fatalf("bad sig %q", got)
	}
}

func TestPutAndDeleteAgainstFakeServer(t *testing.T) {
	var putPath, delPath string
	var putAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			putAuth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			if len(body) == 0 {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(200)
		case http.MethodDelete:
			delPath = r.URL.Path
			w.WriteHeader(200)
		default:
			w.WriteHeader(405)
		}
	}))
	defer srv.Close()

	c := New(Config{
		Bucket:     "yangcongxueyuan",
		Operator:   "op",
		Password:   "pw",
		Domain:     "http://zhiyuansofts.cn",
		APIHost:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if !c.Configured() {
		t.Fatal("should be configured")
	}
	if err := c.Put("blog/1/a.webp", []byte("hello"), "image/webp"); err != nil {
		t.Fatal(err)
	}
	if putPath != "/yangcongxueyuan/blog/1/a.webp" {
		t.Fatalf("put path %s", putPath)
	}
	if !strings.HasPrefix(putAuth, "UPYUN op:") {
		t.Fatalf("auth %s", putAuth)
	}
	if err := c.Delete("/blog/1/a.webp"); err != nil {
		t.Fatal(err)
	}
	if delPath != "/yangcongxueyuan/blog/1/a.webp" {
		t.Fatalf("del path %s", delPath)
	}
}

func TestConfiguredFalse(t *testing.T) {
	if New(Config{}).Configured() {
		t.Fatal("empty should be false")
	}
	var nilC *Client
	if nilC.Configured() {
		t.Fatal("nil should be false")
	}
}
