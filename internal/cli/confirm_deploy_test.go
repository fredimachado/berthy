package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

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
