package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestUpyunStoreAuthenticatesAndProtectsImmutableArchive(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "UPYUN operator:") || r.Header.Get("Date") == "" {
			t.Errorf("missing authentication headers: %v", r.Header)
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len("archive")) || r.Header.Get("Content-Length") != "7" {
			t.Errorf("content length = (%d, %q), want 7", r.ContentLength, r.Header.Get("Content-Length"))
		}
		if string(body) != "archive" {
			t.Errorf("body = %q", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	store := NewUpyunStore("bucket", "operator", "password", server.Client())
	store.endpoint = server.URL
	file := createUploadFile(t, "archive")
	defer file.Close()
	if err := store.Put(context.Background(), "backups/core/a.cwxubak", file, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "HEAD,PUT" {
		t.Fatalf("methods = %s, want HEAD,PUT", got)
	}
}

func createUploadFile(t *testing.T, contents string) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "upload-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(contents); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestUpyunStoreRejectsExistingImmutableArchive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store := NewUpyunStore("bucket", "operator", "password", server.Client())
	store.endpoint = server.URL
	err := store.Put(context.Background(), "backups/core/a.cwxubak", bytes.NewBufferString("archive"), "application/octet-stream")
	if !errors.Is(err, ErrObjectExists) {
		t.Fatalf("Put error = %v, want ErrObjectExists", err)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection reset after write")
}

func TestUpyunStoreMovesTemporaryObjectOverFixedObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/bucket/backups/core/algobak" {
			t.Fatalf("move request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Upyun-Move-Source"); got != "/bucket/backups/core/algobak.uploading-id" {
			t.Fatalf("move source = %q", got)
		}
		if r.ContentLength != 0 {
			t.Fatalf("move content length = %d", r.ContentLength)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store := NewUpyunStore("bucket", "operator", "password", server.Client())
	store.endpoint = server.URL
	if err := store.Move(context.Background(), "backups/core/algobak.uploading-id", "backups/core/algobak"); err != nil {
		t.Fatal(err)
	}
}

func TestUpyunStoreMarksMoveTransportFailureAmbiguous(t *testing.T) {
	store := NewUpyunStore("bucket", "operator", "password", &http.Client{Transport: failingTransport{}})
	err := store.Move(context.Background(), "algobak.uploading-id", "algobak")
	if !errors.Is(err, ErrAmbiguousPublish) {
		t.Fatalf("Move error = %v", err)
	}
}

func TestUpyunStoreTerminalIteratorEndsPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upyun-List-Iter", "g2gCZAAEbmV4dGQAA2VvZg")
		_, _ = io.WriteString(w, "a.cwxubak\tN\t1\t1\n")
	}))
	defer server.Close()
	store := NewUpyunStore("bucket", "operator", "password", server.Client())
	store.endpoint = server.URL
	keys, next, err := store.List(context.Background(), "backups/core/", "")
	if err != nil || next != "" || !reflect.DeepEqual(keys, []string{"backups/core/a.cwxubak"}) {
		t.Fatalf("List = (%v, %q, %v), terminal iterator must become empty", keys, next, err)
	}
}

func TestDefaultUpyunClientHasNoWholeRequestTimeout(t *testing.T) {
	store := NewUpyunStore("bucket", "operator", "password", nil)
	if store.client.Timeout != 0 {
		t.Fatalf("client timeout = %v, want context-controlled transfer", store.client.Timeout)
	}
}
