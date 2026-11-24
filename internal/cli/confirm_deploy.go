package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
)

func confirmDeploy(ctx context.Context, in *os.File, out io.Writer) (bool, error) {
	screen, ok := out.(*os.File)
	if in == nil || !ok || screen == nil || !term.IsTerminal(in.Fd()) || !term.IsTerminal(screen.Fd()) {
		return false, errors.New("deploy confirmation requires terminal input and preview output; run interactively or deploy an explicit digest")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	const prompt = "Deploy this image? [y/N] "
	n, err := io.WriteString(out, prompt)
	if err == nil && n != len(prompt) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return false, fmt.Errorf("write deploy confirmation: %w", err)
	}
	answer, err := readDeployConfirmation(ctx, in, out)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, err
	}
	answer = strings.TrimSpace(answer)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}

// confirmationLine treats escape sequences and oversized input as invalid even if edited away.
type confirmationLine struct {
	text    []byte
	invalid bool
}

func (l *confirmationLine) add(c byte, out io.Writer) (bool, error) {
	switch c {
	case '\r', '\n':
		_, err := io.WriteString(out, "\r\n")
		return true, err
	case 3, 4:
		return true, io.EOF
	case 8, 127:
		if len(l.text) > 0 {
			l.text = l.text[:len(l.text)-1]
			_, err := io.WriteString(out, "\b \b")
			return false, err
		}
	case '\t':
		if len(l.text) >= 256 {
			l.invalid = true
		} else {
			l.text = append(l.text, c)
		}
	case 27:
		l.invalid = true
	default:
		if c < 32 || c >= 127 {
			l.invalid = true
			break
		}
		if len(l.text) >= 256 {
			l.invalid = true
			break
		}
		l.text = append(l.text, c)
		_, err := out.Write([]byte{c})
		return false, err
	}
	return false, nil
}
func (l *confirmationLine) answer() string {
	if l.invalid {
		return ""
	}
	return string(l.text)
}
