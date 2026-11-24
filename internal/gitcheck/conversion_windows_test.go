//go:build windows

package gitcheck

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
)

func TestConversionJobNested(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "nested-job" {
		cmd := exec.CommandContext(context.Background(), "git", "--version")
		if err := runConversion(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestConversionJobNested$", "--", "nested-job")
	output := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := runConversion(context.Background(), cmd); err != nil {
		t.Fatalf("nested job: %v: %s", err, output.String())
	}
}

func TestConversionJobStartErrors(t *testing.T) {
	ctx := context.Background()
	if err := runConversion(ctx, exec.CommandContext(ctx, "this-git-command-does-not-exist-berthy")); err == nil {
		t.Fatal("missing command succeeded")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := runConversion(ctx, exec.CommandContext(ctx, os.Args[0])); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled start: %v", err)
	}
}

func TestJobDrainTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
		if err := awaitJobDrain(ctx, make(chan error)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("drain: %v", err)
		}
	})
}
