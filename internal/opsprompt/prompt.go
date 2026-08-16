package opsprompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

var ErrNonInteractive = errors.New("非交互式终端：请提供参数或环境变量")

type Prompter struct {
	In  *bufio.Reader
	Out io.Writer
	TTY bool
}

func New() *Prompter {
	return &Prompter{
		In:  bufio.NewReader(os.Stdin),
		Out: os.Stdout,
		TTY: term.IsTerminal(int(os.Stdin.Fd())),
	}
}

func (p *Prompter) String(prompt, def string) (string, error) {
	if !p.TTY {
		return "", ErrNonInteractive
	}
	fmt.Fprintf(p.Out, "%s [%s]: ", prompt, def)
	line, err := p.In.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (p *Prompter) Password(prompt string) (string, error) {
	if !p.TTY {
		return "", ErrNonInteractive
	}
	fmt.Fprint(p.Out, prompt+": ")
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(p.Out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (p *Prompter) Confirm(prompt string, def bool) (bool, error) {
	if !p.TTY {
		return false, ErrNonInteractive
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(p.Out, "%s (%s): ", prompt, hint)
	line, err := p.In.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return def, nil
}

func (p *Prompter) Choice(prompt string, def int, options ...string) (int, error) {
	if !p.TTY {
		return -1, ErrNonInteractive
	}
	for i, opt := range options {
		fmt.Fprintf(p.Out, "  %d) %s\n", i+1, opt)
	}
	fmt.Fprintf(p.Out, "%s [%d]: ", prompt, def+1)
	line, err := p.In.ReadString('\n')
	if err != nil && err != io.EOF {
		return -1, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(options) {
		return def, nil
	}
	return n - 1, nil
}
