package platform

import "testing"

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
