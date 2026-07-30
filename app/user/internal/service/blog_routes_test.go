package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

func TestBlogImageOrphanPreviewRouteUsesGET(t *testing.T) {
	srv := khttp.NewServer()
	RegisterBlogRoutes(srv, &BlogService{})

	getRecorder := httptest.NewRecorder()
	srv.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/v1/user/blog/images/orphans", nil))
	if getRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET preview route status=%d want handler's 401", getRecorder.Code)
	}

	postRecorder := httptest.NewRecorder()
	srv.ServeHTTP(postRecorder, httptest.NewRequest(http.MethodPost, "/v1/user/blog/images/orphans", nil))
	if postRecorder.Code == http.StatusUnauthorized {
		t.Fatal("POST must not dispatch to the orphan preview handler")
	}
}
