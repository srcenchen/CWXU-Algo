package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	blogpb "cwxu-algo/api/user/v1/blog"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

func TestBlogImageCleanupRoutesAreAdminOnly(t *testing.T) {
	srv := khttp.NewServer()
	blogpb.RegisterBlogHTTPServer(srv, &BlogService{})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/user/blog/images/orphans", nil),
		httptest.NewRequest(http.MethodPost, "/v1/user/blog/images/orphans", nil),
		httptest.NewRequest(http.MethodPost, "/v1/user/blog/images/gc", nil),
	} {
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("legacy route %s %s status=%d", request.Method, request.URL.Path, recorder.Code)
		}
	}

	adminRecorder := httptest.NewRecorder()
	srv.ServeHTTP(adminRecorder, httptest.NewRequest(http.MethodGet, "/v1/user/blog/admin/images", nil))
	if adminRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("admin image route status=%d want 401", adminRecorder.Code)
	}
}

func TestBlogPinnedRoutesRequireLogin(t *testing.T) {
	srv := khttp.NewServer()
	blogpb.RegisterBlogHTTPServer(srv, &BlogService{})
	request := httptest.NewRequest(http.MethodGet, "/v1/user/blog/article/pinned/mine", nil)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("pinned mine route status=%d want 401", recorder.Code)
	}
	for _, path := range []string{"/v1/user/blog/article/pin", "/v1/user/blog/article/pinned/reorder"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound {
			t.Fatalf("pinned route was not registered: %s", path)
		}
	}
}
