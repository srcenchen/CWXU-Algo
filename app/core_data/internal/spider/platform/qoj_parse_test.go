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
		{name: "integer full score without marker", raw: "100", status: "WA"},
		{name: "integer full score marker", raw: "100 ✓", status: "AC"},
		{name: "decimal full score without marker", raw: "100.0", status: "WA"},
		{name: "partial score", raw: "99", status: "WA"},
		{name: "zero score", raw: "0", status: "WA"},
		{name: "accepted text", raw: "Accepted", status: "AC"},
		{name: "accepted marker", raw: "AC ✓", status: "AC"},
		{name: "compile error", raw: "CE", status: "CE"},
		{name: "compilation error", raw: "Compilation Error", status: "CE"},
		{name: "wrong answer", raw: "Wrong Answer", status: "WA"},
		{name: "time limit exceeded", raw: "Time Limit Exceeded", status: "TLE"},
		{name: "memory limit exceeded", raw: "Memory Limit Exceeded", status: "MLE"},
		{name: "runtime error", raw: "Runtime Error", status: "RE"},
		{name: "output limit exceeded", raw: "Output Limit Exceeded", status: "OLE"},
		{name: "presentation error", raw: "Presentation Error", status: "PE"},
		{name: "idleness limit exceeded", raw: "Idleness Limit Exceeded", status: "ILE"},
		{name: "security violated", raw: "Security Violated", status: "SV"},
		{name: "judgement failed", raw: "Judgement Failed", status: "JF"},
		{name: "judging", raw: "Judging", status: "JUDGING"},
		{name: "waiting", raw: "Waiting", status: "WAITING"},
		{name: "waiting rejudge", raw: "Waiting Rejudge", status: "WAITING"},
		{name: "judged waiting", raw: "Judged, Waiting", status: "WAITING"},
		{name: "judged judging", raw: "Judged, Judging", status: "JUDGING"},
		{name: "ellipsis pending", raw: "…", status: "JUDGING"},
		{name: "three dots pending", raw: "...", status: "JUDGING"},
		{name: "future qoj result", raw: "Validator Failed", status: "VALIDATOR FAILED"},
		{name: "unknown future qoj result", raw: "Partially Correct", status: "PARTIALLY CORRECT"},
		{name: "nan score", raw: "NaN", wantErr: true},
		{name: "nan score marker", raw: "NaN ✓", wantErr: true},
		{name: "infinite score", raw: "+Inf", wantErr: true},
		{name: "negative score", raw: "-1", wantErr: true},
		{name: "score above full without marker", raw: "110", status: "WA"},
		{name: "score above full check mark", raw: "110 ✓", status: "AC"},
		{name: "score above full heavy check mark", raw: "110 ✔", status: "AC"},
		{name: "score above full emoji check mark", raw: "110 ✅", status: "AC"},
		{name: "implausibly high score", raw: "1000001", wantErr: true},
		{name: "authentication error is not a verdict", raw: "Access denied", wantErr: true},
		{name: "service error is not a verdict", raw: "Central failure", wantErr: true},
		{name: "overlong result is not a verdict", raw: strings.Repeat("A", 65), wantErr: true},
		{name: "empty", raw: "  ", wantErr: true},
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

func TestParseQOJSubmissionPageKeepsUnmarkedScoreAboveFull(t *testing.T) {
	html := `<tbody>` + qojTestRow("1", "110", "2026-08-24 12:00:00") + `</tbody>`
	logs, _, err := parseQOJSubmissionPage(html, 7, 2, map[string]struct{}{})
	if err != nil {
		t.Fatalf("parseQOJSubmissionPage: %v", err)
	}
	if len(logs) != 1 || logs[0].Status != "WA" {
		t.Fatalf("logs=%+v, want one WA submission", logs)
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
		{name: "empty table body", html: `<tbody></tbody>`},
		{name: "unknown single-cell row", html: `<tbody><tr><td colspan="233">Service unavailable</td></tr></tbody>`},
		{name: "data colspan is not the official empty state", html: `<tbody><tr><td data-colspan="233">None</td></tr></tbody>`},
		{name: "empty submit id", html: `<tbody>` + qojTestRow("", "100", "2026-08-24 12:00:00") + `</tbody>`},
		{name: "empty status", html: `<tbody>` + qojTestRow("1", " ", "2026-08-24 12:00:00") + `</tbody>`},
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

func TestParseQOJSubmissionPageAcceptsOfficialEmptyState(t *testing.T) {
	for _, tt := range []struct {
		name string
		cell string
	}{
		{name: "double quoted", cell: `<td colspan="233">None</td>`},
		{name: "single quoted", cell: `<td colspan='233'>无</td>`},
		{name: "unquoted", cell: `<td colspan=233>None</td>`},
	} {
		html := `<tbody><tr>` + tt.cell + `</tr></tbody>`
		logs, hasNext, err := parseQOJSubmissionPage(html, 7, 2, map[string]struct{}{})
		if err != nil || len(logs) != 0 || hasNext {
			t.Fatalf("name=%q logs=%d hasNext=%v err=%v", tt.name, len(logs), hasNext, err)
		}
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
