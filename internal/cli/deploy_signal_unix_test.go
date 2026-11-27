//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"berthy/internal/gitcheck"
)

func TestMutableTagCaptureReceivesSIGTERM(t *testing.T) {
	const childMode = "BERTHY_TEST_MUTABLE_TAG_SIGTERM"
	if os.Getenv(childMode) != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestMutableTagCaptureReceivesSIGTERM$")
		child.Env = append(os.Environ(), childMode+"=1", "TMPDIR="+t.TempDir())
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("capture child: %v\n%s", err, output)
		}
		return
	}

	signal.Reset(syscall.SIGTERM)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	fixtureErr := errors.New("fixture stopped before deploy admission")
	captureDeployCheckout = func(ctx context.Context, _ string) (gitcheck.Identity, error) {
		entered <- ctx
		<-release
		return gitcheck.Identity{}, fixtureErr
	}
	dir := t.TempDir()
	args := []string{"api", "--version", "stable", "-e", "dev", "--project", "demo", "--config", filepath.Join(dir, "unused.json")}
	var errs bytes.Buffer
	var code int
	done := make(chan struct{})
	go func() {
		defer close(done)
		code = runDeployment("deploy", args, io.Discard, &errs, dir, nil)
	}()
	defer func() { unblock(); <-done }()

	var ctx context.Context
	select {
	case ctx = <-entered:
	case <-done:
		t.Fatalf("deploy exited %d before checkout capture: %s", code, errs.String())
	}
	if ctx.Done() == nil {
		t.Fatal("initial checkout capture received a context with a nil Done channel")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if got := context.Cause(ctx); got != (commandSignal{syscall.SIGTERM}) {
		t.Fatalf("initial checkout capture cause %v, want SIGTERM", got)
	}
	unblock()
	<-done
	if code != 1 || !strings.Contains(errs.String(), fixtureErr.Error()) {
		t.Fatalf("fixture did not stop deploy at checkout capture: exit %d, stderr %q", code, errs.String())
	}
}
