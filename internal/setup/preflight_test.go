package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRejectsUnusableInputsBeforeProvisioning(t *testing.T) {
	exe := elfFile(t, emX86)
	for _, tc := range []struct {
		name, detail string
		dry          bool
		opts         func(*Options)
		noTools      bool
	}{
		{"dev", "development build", false, func(o *Options) { o.Build = Build{OS: "darwin", Arch: "arm64"} }, false},
		{"no downloader", "neither curl nor wget", false, func(o *Options) { o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true} }, true},
		{"missing key", "cannot read --key-file", false, func(o *Options) { o.KeyFile = filepath.Join(t.TempDir(), "missing") }, false},
		{"short key", "--key-file must hold exactly 32 bytes", false, func(o *Options) { o.KeyFile = writeKey(t, []byte("short")) }, false},
		{"dry no downloader", "would install v1.2.3", true, func(o *Options) { o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true} }, true},
		{"dry short key", "--key-file must hold exactly 32 bytes", true, func(o *Options) { o.KeyFile = writeKey(t, []byte("short")) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := freshAgentServer(t)
			if tc.noTools {
				f.probe = without(f.probe, "curl=", "wget=")
			}
			s := agentSession(t, f, exe, tc.dry, tc.opts)
			got, ok := runSteps(s)
			if tc.dry {
				if stepOf(t, got, "check").Status != StatusOK {
					t.Fatalf("%+v", got)
				}
				if tc.noTools {
					r := stepOf(t, got, "binary")
					if r.Status != StatusWouldChange || r.Detail != tc.detail {
						t.Fatalf("%+v", got)
					}
					stepOf(t, got, "key")
					return
				}
				r := lastResult(got)
				if ok || r.Step != "key" || r.Detail != tc.detail {
					t.Fatalf("%+v", got)
				}
				return
			}
			if ok || statuses(got) != "connect=ok check=failed" || !strings.Contains(lastResult(got).Detail, tc.detail) {
				t.Errorf("got %+v", got)
			}
			for _, c := range f.calls {
				if agentChange(c.script) {
					t.Errorf("provisioned: %s", c.script)
				}
			}
		})
	}
}

func TestSetupUsesTheKeyReadAtCheck(t *testing.T) {
	exe := elfFile(t, emX86)
	f := freshAgentServer(t)
	key := keyBytes()
	file := writeKey(t, key)
	s := agentSession(t, f, exe, false, func(o *Options) { o.KeyFile = file })
	var got []StepResult
	ok := Run(context.Background(), s, Steps(), func(r StepResult) {
		got = append(got, r)
		if r.Step == "check" && r.Status == StatusOK {
			if err := os.Remove(file); err != nil {
				t.Error(err)
			}
		}
	})
	c, found := f.find("cat > \"$tmp\"")
	if !ok || !found || !bytes.Equal(c.stdin, key) {
		t.Fatalf("got %+v, uploaded=%v", got, found)
	}
}

type releaseFailure struct {
	*agentFake
	asset string
}

func (f *releaseFailure) Run(ctx context.Context, script string, in io.Reader) (Result, error) {
	if strings.Contains(script, f.asset) {
		f.calls = append(f.calls, call{script: script})
		return Result{Code: 1}, nil
	}
	return f.agentFake.Run(ctx, script, in)
}
func TestSetupChecksBothReleaseAssetsBeforeProvisioning(t *testing.T) {
	exe := elfFile(t, emX86)
	for _, tool := range []string{"curl", "wget"} {
		for _, asset := range []string{"berthy-linux-amd64", "checksums.txt"} {
			t.Run(tool+"/"+asset, func(t *testing.T) {
				f := freshAgentServer(t)
				f.probe = without(f.probe, "curl=", "wget=")
				f.probe = with(f.probe, tool+"=/usr/bin/"+tool)
				runner := &releaseFailure{agentFake: f, asset: asset}
				s := agentSession(t, runner, exe, false, func(o *Options) { o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true} })
				got, ok := runSteps(s)
				if ok || statuses(got) != "connect=ok check=failed" || !strings.Contains(lastResult(got).Detail, asset) {
					t.Errorf("got %+v", got)
				}
				for _, c := range f.calls {
					if agentChange(c.script) {
						t.Errorf("provisioned: %s", c.script)
					}
				}
			})
		}
	}
	t.Run("installed without downloader", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = without(f.probe, "curl=", "wget=")
		f.version = "berthy v1.2.3 linux/amd64\n"
		s := agentSession(t, f, exe, false, func(o *Options) { o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true} })
		got, ok := runSteps(s)
		if !ok || stepOf(t, got, "binary").Status != StatusOK || f.ran("checksums.txt") {
			t.Fatalf("%+v", got)
		}
	})
}

func TestSetupClearsThePreparedKeyOnExit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("later failure=", fail), func(t *testing.T) {
			exe := elfFile(t, emX86)
			f := freshAgentServer(t)
			key := keyBytes()
			file := writeKey(t, key)
			s := agentSession(t, f, exe, false, func(o *Options) { o.KeyFile = file })
			var captured []byte
			steps := Steps()
			if fail {
				steps = []Step{steps[0], steps[1], {Name: "fail", Run: func(context.Context, *Session) (string, string, error) { return "", "", errors.New("forced failure") }}}
			}
			ok := Run(context.Background(), s, steps, func(r StepResult) {
				if r.Step == "check" {
					captured = s.suppliedKey
				}
			})
			if ok == fail || len(captured) != 32 || s.suppliedKey != nil || !bytes.Equal(captured, make([]byte, 32)) {
				t.Fatalf("ok=%v captured=%v retained=%v", ok, captured, s.suppliedKey)
			}
		})
	}
}

func TestSetupDryRunOmitsUnreachableListenProbe(t *testing.T) {
	exe := elfFile(t, emX86)
	for _, tc := range []struct {
		name, step, detail string
		noTools            bool
		opts               func(*Options)
	}{
		{"missing key", "key", "cannot read --key-file", false, func(o *Options) { o.KeyFile = filepath.Join(t.TempDir(), "missing") }},
		{"short key", "key", "--key-file must hold exactly 32 bytes", false, func(o *Options) { o.KeyFile = writeKey(t, []byte("short")) }},
		{"unusable binary", "binary", "development build", false, func(o *Options) { o.Build = Build{OS: "darwin", Arch: "arm64"} }},
		{"no downloader", "", "", true, func(o *Options) { o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := freshAgentServer(t)
			if tc.noTools {
				f.probe = without(f.probe, "curl=", "wget=")
			}
			s := agentSession(t, f, exe, true, tc.opts)
			var log bytes.Buffer
			s.Log = NewCommandLog(&log)
			got, ok := runSteps(s)
			if tc.step != "" {
				r := lastResult(got)
				if ok || r.Step != tc.step || !strings.Contains(r.Detail, tc.detail) {
					t.Errorf("results: %+v", got)
				}
			} else {
				r := stepOf(t, got, "binary")
				if r.Status != StatusWouldChange || r.Detail != "would install v1.2.3" {
					t.Errorf("binary: %+v", r)
				}
				stepOf(t, got, "key")
			}
			if strings.Contains(log.String(), "# setup listen port") {
				t.Errorf("unreachable listen probe appears in verbose preview")
			}
		})
	}
}
