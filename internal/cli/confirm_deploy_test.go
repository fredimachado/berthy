package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type confirmationWriter struct{ err error }

func (w confirmationWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestConfirmationWriteFailures(t *testing.T) {
	failure := errors.New("write failed")
	if err := writeDeployPrompt(context.Background(), confirmationWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("short prompt: %v", err)
	}
	if err := writeDeployPrompt(context.Background(), confirmationWriter{failure}); !errors.Is(err, failure) {
		t.Errorf("prompt: %v", err)
	}
	var line confirmationLine
	if _, err := line.add('\t', confirmationWriter{failure}); !errors.Is(err, failure) {
		t.Errorf("tab echo: %v", err)
	}
}

func TestConfirmationTabBackspaceTranscript(t *testing.T) {
	var line confirmationLine
	var out bytes.Buffer
	for _, c := range []byte("y\t\b\r") {
		if _, err := line.add(c, &out); err != nil {
			t.Fatal(err)
		}
	}
	if got := out.String(); got != "y \b \b\r\n" {
		t.Errorf("transcript = %q", got)
	}
	if got := line.answer(); got != "y" {
		t.Errorf("answer = %q", got)
	}
}

func TestConfirmDeployRejectsPipes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if ok, err := confirmDeploy(context.Background(), r, w); ok || err == nil {
		t.Fatalf("pipe: ok=%v err=%v", ok, err)
	}
}

func TestConfirmationLine(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"y\bYes\r", "Yes"}, {"y\x1b[D\r", ""}, {strings.Repeat(" ", 257) + "yes\r", ""},
	} {
		var line confirmationLine
		for _, c := range []byte(tc.input) {
			end, err := line.add(c, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if end {
				break
			}
		}
		if got := line.answer(); got != tc.want {
			t.Errorf("input %q: got %q want %q", tc.input, got, tc.want)
		}
	}
}
