package opsprogress

import (
	"fmt"
	"io"
)

type Progress struct {
	total   int
	current int
	out     io.Writer
}

func New(total int, out io.Writer) *Progress {
	return &Progress{total: total, out: out}
}

func (p *Progress) Step(message string) {
	p.current++
	fmt.Fprintf(p.out, "[%d/%d] %s\n", p.current, p.total, message)
}

func (p *Progress) Sub(message string) {
	fmt.Fprintf(p.out, "  - %s\n", message)
}

func (p *Progress) Message(message string) {
	fmt.Fprintf(p.out, "%s\n", message)
}

func Done(out io.Writer, message string) {
	fmt.Fprintf(out, "%s\n", message)
}
