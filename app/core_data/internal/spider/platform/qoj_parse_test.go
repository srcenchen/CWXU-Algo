package platform

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeQOJResult(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		status  string
		wantErr bool
	}{
		{name: "integer full score", raw: "100", status: "AC"},
		{name: "decimal full score", raw: "100.0", status: "AC"},
		{name: "partial score", raw: "99", status: "WA"},
		{name: "zero score", raw: "0", status: "WA"},
		{name: "accepted text", raw: "Accepted", status: "AC"},
		{name: "compile error", raw: "CE", status: "CE"},
		{name: "judging", raw: "Judging", status: "JUDGING"},
		{name: "waiting", raw: "Waiting", status: "WAITING"},
		{name: "nan score", raw: "NaN", wantErr: true},
		{name: "infinite score", raw: "+Inf", wantErr: true},
		{name: "negative score", raw: "-1", wantErr: true},
		{name: "score above full", raw: "101", wantErr: true},
		{name: "ac prefix anomaly", raw: "Access denied", wantErr: true},
		{name: "ce prefix anomaly", raw: "Central failure", wantErr: true},
		{name: "unknown", raw: "unexpected result", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeQOJResult(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeQOJResult(%q) error=nil", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeQOJResult(%q): %v", tt.raw, err)
			}
			if got != tt.status {
				t.Fatalf("normalizeQOJResult(%q)=%q, want %q", tt.raw, got, tt.status)
			}
		})
	}
}

func TestQojProblemFromCell(t *testing.T) {
	cell := `<a href="/problem/19004">#19004. Local Maxima</a>`
	if got := qojProblemFromCell(cell); got != "#19004. Local Maxima" {
		t.Fatalf("got %q", got)
	}
	cell2 := `<td class="text-left"><a href="https://qoj.ac/problem/1">#1. I/O Test</a></td>`
	if got := qojProblemFromCell(cell2); got != "#1. I/O Test" {
		t.Fatalf("got %q", got)
	}
	if got := qojProblemFromCell(`plain text only`); got != "plain text only" {
		t.Fatalf("got %q", got)
	}
}

func TestParseQOJSubmissionPageRejectsBadRowsAndDuplicateIDs(t *testing.T) {
	valid := qojTestRow("1", "100", "2026-08-24 12:00:00")
	tests := []struct {
		name string
		html string
	}{
		{name: "too few columns", html: `<tbody><tr><td>#1</td></tr></tbody>`},
		{name: "empty submit id", html: `<tbody>` + qojTestRow("", "100", "2026-08-24 12:00:00") + `</tbody>`},
		{name: "bad status", html: `<tbody>` + qojTestRow("1", "mystery", "2026-08-24 12:00:00") + `</tbody>`},
		{name: "bad time", html: `<tbody>` + qojTestRow("1", "100", "not-a-time") + `</tbody>`},
		{name: "duplicate submit id", html: `<tbody>` + valid + valid + `</tbody>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseQOJSubmissionPage(tt.html, 7, 2, map[string]struct{}{}); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestFetchQOJSubmitLogCompleteSemantics(t *testing.T) {
	if qojProductionMaxPages != 0 {
		t.Fatalf("production max pages=%d want=0 (unlimited)", qojProductionMaxPages)
	}

	t.Run("incremental is never complete", func(t *testing.T) {
		client, baseURL, closeServer := qojPageServer(t, 1)
		defer closeServer()
		logs, complete, err := fetchQOJSubmitLogs(context.Background(), client, baseURL, 7, false, 200, nil)
		if err != nil || len(logs) != 1 || complete {
			t.Fatalf("logs=%d complete=%v err=%v", len(logs), complete, err)
		}
	})

	t.Run("full fetch reaching last page is complete", func(t *testing.T) {
		client, baseURL, closeServer := qojPageServer(t, 2)
		defer closeServer()
		logs, complete, err := fetchQOJSubmitLogs(context.Background(), client, baseURL, 7, true, 200, nil)
		if err != nil || len(logs) != 2 || !complete {
			t.Fatalf("logs=%d complete=%v err=%v", len(logs), complete, err)
		}
	})

	t.Run("page limit with next remains incomplete", func(t *testing.T) {
		client, baseURL, closeServer := qojPageServer(t, 201)
		defer closeServer()
		logs, complete, err := fetchQOJSubmitLogs(context.Background(), client, baseURL, 7, true, 200, nil)
		if err != nil || len(logs) != 200 || complete {
			t.Fatalf("logs=%d complete=%v err=%v", len(logs), complete, err)
		}
	})

	t.Run("zero page limit follows next beyond two hundred pages", func(t *testing.T) {
		client, baseURL, closeServer := qojPageServer(t, 201)
		defer closeServer()
		logs, complete, err := fetchQOJSubmitLogs(context.Background(), client, baseURL, 7, true, 0, nil)
		if err != nil || len(logs) != 201 || !complete {
			t.Fatalf("logs=%d complete=%v err=%v", len(logs), complete, err)
		}
	})
}

func qojTestRow(id, status, submittedAt string) string {
	return fmt.Sprintf(`<tr><td>#%s</td><td><a href="/problem/1">#1. Test</a></td><td>-</td><td>%s</td><td>-</td><td>-</td><td>Go</td><td>-</td><td>%s</td></tr>`, id, status, submittedAt)
}

func qojPageServer(t *testing.T, pages int) (*http.Client, string, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		_, _ = fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		next := ""
		if page < pages {
			next = fmt.Sprintf(`<a href="?page=%d">next</a>`, page+1)
		}
		_, _ = fmt.Fprintf(w, `<tbody>%s</tbody>%s`, qojTestRow(fmt.Sprint(page), "100", "2026-08-24 12:00:00"), next)
	}))
	return server.Client(), strings.TrimSuffix(server.URL, "/") + "/submissions?submitter=test&page=", server.Close
}
