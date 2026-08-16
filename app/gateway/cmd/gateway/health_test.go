package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithLocalHealthDoesNotProxyLiveness(t *testing.T) {
	proxied := false
	handler := withLocalHealth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		proxied = true
	}))

	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || proxied {
			t.Fatalf("%s: code=%d proxied=%v", path, recorder.Code, proxied)
		}
	}
}
