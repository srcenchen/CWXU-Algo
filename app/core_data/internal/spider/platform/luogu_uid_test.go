package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveLuoGuRecordUserUsesNumericUID(t *testing.T) {
	var searchPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searchPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"uid":1520672,"name":"YOLU_gargaring"}]}`))
	}))
	defer server.Close()

	lg := &NewLuoGu{}
	uid, err := lg.resolveUIDFromEndpoint(server.Client(), "YOLU_gargaring", server.URL+"?keyword=")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1520672 {
		t.Fatalf("uid=%d, want 1520672", uid)
	}
	if searchPath != "/?keyword=YOLU_gargaring" {
		t.Fatalf("unexpected search URI %q", searchPath)
	}
	if got := lg.recordListBaseURL(uid); got != "https://www.luogu.com.cn/record/list?user=1520672&page=" {
		t.Fatalf("record URL=%q", got)
	}
}

func TestResolveLuoGuRecordUserKeepsNumericUID(t *testing.T) {
	lg := &NewLuoGu{}
	uid, err := lg.resolveUIDFromEndpoint(http.DefaultClient, "1520672", "http://invalid.test/?keyword=")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1520672 {
		t.Fatalf("uid=%d", uid)
	}
}

func TestValidateLuoGuRecordsRejectsSuspiciousEmptyPage(t *testing.T) {
	if err := validateLuoGuRecords(12, nil); err == nil {
		t.Fatal("non-zero result count with empty records must fail")
	}
	if err := validateLuoGuRecords(0, nil); err != nil {
		t.Fatalf("genuinely empty account must pass: %v", err)
	}
}
