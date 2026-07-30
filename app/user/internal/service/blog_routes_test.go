package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

func TestBlogImageCleanupRoutesAreAdminOnly(t *testing.T) {
	srv := khttp.NewServer()
	RegisterBlogRoutes(srv, &BlogService{})

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
