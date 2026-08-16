package opsprompt

import (
	"bufio"
	"strings"
	"testing"
)

func TestNonInteractiveReturnsError(t *testing.T) {
	p := New()
	p.TTY = false
	if _, err := p.String("a", "b"); err != ErrNonInteractive {
		t.Fatalf("expected ErrNonInteractive, got %v", err)
	}
}

func TestStringUsesDefaultOnEnter(t *testing.T) {
	p := New()
	p.TTY = true
	p.In = bufio.NewReader(strings.NewReader("\n"))
	got, err := p.String("name", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "default" {
		t.Fatalf("got %q, want default", got)
	}
}

func TestChoiceReturnsIndex(t *testing.T) {
	p := New()
	p.TTY = true
	p.In = bufio.NewReader(strings.NewReader("2\n"))
	idx, err := p.Choice("pick", 0, "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("got %d, want 1", idx)
	}
}

func TestConfirmFalseOnNo(t *testing.T) {
	p := New()
	p.TTY = true
	p.In = bufio.NewReader(strings.NewReader("n\n"))
	if ok, err := p.Confirm("proceed?", false); err != nil || ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}
