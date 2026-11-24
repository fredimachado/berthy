package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/keywrap"
)

type copyRunner struct {
	agentFake
	answer func(context.Context, []byte) (Result, error)
	called bool
}

func (r *copyRunner) Run(ctx context.Context, script string, in io.Reader) (Result, error) {
	r.called = true
	b, _ := io.ReadAll(in)
	return r.answer(ctx, b)
}

func TestKeyCopyWithoutHomeIsNonfatal(t *testing.T) {
	status, detail, err := keyCopyStep(context.Background(), &Session{})
	if err != nil || status != StatusSkipped || !strings.Contains(detail, "this machine has no home folder") {
		t.Fatalf("got %q %q %v", status, detail, err)
	}
}

func TestKeyCopySavesAndRetries(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	r := &copyRunner{answer: func(ctx context.Context, in []byte) (Result, error) {
		out, err := keywrap.Wrap(in, key)
		return Result{Stdout: out}, err
	}}
	s := &Session{R: r, Opts: Options{Project: "shop", Env: "p5", KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { return wrap, nil }}}
	var first os.FileInfo
	for _, want := range []string{StatusChanged, StatusOK} {
		status, _, err := keyCopyStep(context.Background(), s)
		if err != nil || status != want {
			t.Fatalf("status %q, error %v, want %q", status, err, want)
		}
		got, err := os.ReadFile(s.KeyCopyPath)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatal("saved key differs")
		}
		info, err := os.Stat(s.KeyCopyPath)
		if err != nil {
			t.Fatal(err)
		}
		if first != nil && (!os.SameFile(first, info) || !first.ModTime().Equal(info.ModTime())) {
			t.Fatal("retry changed the key copy")
		}
		first = info
	}
}

func TestKeyCopySkipsUnsafeCases(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		local, dry bool
		answer     []byte
		code       int
		runErr     error
		unsettled  bool
	}{
		{name: "local", local: true}, {name: "dry", dry: true}, {name: "unsettled", answer: []byte(keywrap.UnsettledLine), code: 3, unsettled: true},
		{name: "bad answer", answer: []byte("secret"), code: 1}, {name: "missing helper", code: 2},
		{name: "exit three without marker", code: 3}, {name: "error", runErr: errors.New("secret")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &copyRunner{answer: func(context.Context, []byte) (Result, error) {
				return Result{Stdout: tc.answer, Stderr: []byte("secret"), Code: tc.code}, tc.runErr
			}}
			s := &Session{R: r, DryRun: tc.dry, Opts: Options{Local: tc.local, Project: "shop", Env: "p5", KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { return wrap, nil }}}
			status, detail, err := keyCopyStep(context.Background(), s)
			if status != StatusSkipped || err != nil || s.KeyCopyPath != "" || s.KeyUnsettled != tc.unsettled || strings.Contains(detail, "secret") {
				t.Fatal("unsafe copy outcome")
			}
			if (tc.local || tc.dry) && r.called {
				t.Fatal("contacted server")
			}
		})
	}
}

func TestKeyCopyTimeoutUsesExchangeDeadline(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		generated := false
		r := &copyRunner{answer: func(ctx context.Context, _ []byte) (Result, error) {
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Sub(time.Now()) != 30*time.Second || !generated {
				t.Error("wrong exchange deadline")
			}
			<-ctx.Done()
			return Result{}, ctx.Err()
		}}
		s := &Session{R: r, Opts: Options{KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { generated = true; return wrap, nil }}}
		status, detail, err := keyCopyStep(context.Background(), s)
		if status != StatusSkipped || err != nil || !strings.Contains(detail, "the server did not answer in time") {
			t.Fatalf("got %q %q %v", status, detail, err)
		}
	})
}

func TestKeyCopyVerboseDoesNotExposeRunnerError(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	r := &copyRunner{answer: func(context.Context, []byte) (Result, error) { return Result{}, errors.New("private-fixture-key") }}
	l := NewCommandLog(&log)
	s := &Session{R: Recording(r, l), Log: l, Opts: Options{KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { return wrap, nil }}}
	l.begin("cli_key")
	_, _, _ = keyCopyStep(context.Background(), s)
	if strings.Contains(log.String(), "private-fixture-key") {
		t.Fatal("verbose log exposed runner error")
	}
}

func TestKeyWrapScriptOnlyReads(t *testing.T) {
	script := keyWrapScript("/a key/it's")
	for _, want := range []string{"# key copy", "--reuid=berthy", "--regid=berthy", "--clear-groups", "agent key-wrap", shellQuote("/a key/it's")} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	if strings.Contains(script, ">") || strings.Contains(script, "sudo") {
		t.Fatal("script changes files or invokes sudo")
	}
}

func TestKeyCopyInterruptedIsNonfatal(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &copyRunner{answer: func(context.Context, []byte) (Result, error) { cancel(); return Result{}, context.Canceled }}
	s := &Session{R: r, Opts: Options{KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { return wrap, nil }}}
	status, detail, err := keyCopyStep(ctx, s)
	if status != StatusSkipped || err != nil || !strings.Contains(detail, "interrupted") || s.KeyCopyPath != "" {
		t.Fatalf("got %q %q %v", status, detail, err)
	}
}

func TestKeyCopyUnsettledMarkerRequiresExitThree(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{0, 1, 2, 4} {
		r := &copyRunner{answer: func(context.Context, []byte) (Result, error) {
			return Result{Code: code, Stdout: []byte(keywrap.UnsettledLine)}, nil
		}}
		s := &Session{R: r, Opts: Options{KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { return wrap, nil }}}
		status, detail, err := keyCopyStep(context.Background(), s)
		if err != nil || status != StatusSkipped || s.KeyUnsettled || !strings.Contains(detail, "did not hand over") {
			t.Errorf("exit %d incorrectly accepted", code)
		}
	}
}

func TestKeyCopyAlreadyCancelledDoesNotContactServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &copyRunner{answer: func(context.Context, []byte) (Result, error) { return Result{}, nil }}
	s := &Session{R: r, Opts: Options{KeyHome: t.TempDir()}}
	status, detail, err := keyCopyStep(ctx, s)
	if status != StatusSkipped || err != nil || r.called || !strings.Contains(detail, "interrupted") {
		t.Fatal("cancelled run contacted server")
	}
}
