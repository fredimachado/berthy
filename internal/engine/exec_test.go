package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"
)

// execFrames returns a Docker multiplexed output stream of frames stdout frames of size bytes each.
func execFrames(frames, size int) string {
	var b bytes.Buffer
	for i := 0; i < frames; i++ {
		var hdr [8]byte
		hdr[0] = 1
		binary.BigEndian.PutUint32(hdr[4:], uint32(size))
		b.Write(hdr[:])
		b.WriteString(strings.Repeat("x", size))
	}
	return b.String()
}

// execEngine fakes the three Docker calls of an exec: create, start (which streams output) and inspect (the exit code).
func execEngine(t *testing.T, stream string, exit string) *Engine {
	return fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/c1/exec":
			return response(201, `{"Id":"e1"}`), nil
		case "/v1.44/exec/e1/start":
			return response(200, stream), nil
		case "/v1.44/exec/e1/json":
			return response(200, `{"Running":false,"ExitCode":`+exit+`}`), nil
		}
		t.Errorf("request %s", r.URL.Path)
		return response(404, `{}`), nil
	})
}

func execCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestExecKeepsOutputAndExitCode(t *testing.T) {
	res, err := execEngine(t, execFrames(1, 10), "3").Exec(execCtx(t), "c1", []string{"true"})
	if err != nil || res.ExitCode != 3 || len(res.Output) != 10 {
		t.Fatalf("exec: %+v %v", res, err)
	}
}

func TestExecDiscardReturnsTheExitCodeOfAChattyCommand(t *testing.T) {
	// Three mebibytes of output: Exec refuses it, ExecDiscard reads it and throws it away.
	chatty := execFrames(3, 1<<20)
	if _, err := execEngine(t, chatty, "0").Exec(execCtx(t), "c1", []string{"true"}); err == nil {
		t.Fatal("Exec accepted output past its limit")
	}
	code, err := execEngine(t, chatty, "0").ExecDiscard(execCtx(t), "c1", []string{"true"})
	if err != nil || code != 0 {
		t.Fatalf("exec discard: %d %v", code, err)
	}
	code, err = execEngine(t, chatty, "7").ExecDiscard(execCtx(t), "c1", []string{"true"})
	if err != nil || code != 7 {
		t.Fatalf("exit code: %d %v", code, err)
	}
}

func TestExecDiscardRefusesAMalformedStream(t *testing.T) {
	if _, err := execEngine(t, "not a docker stream", "0").ExecDiscard(execCtx(t), "c1", []string{"true"}); err == nil {
		t.Fatal("malformed stream accepted")
	}
	if _, err := execEngine(t, "", "0").ExecDiscard(execCtx(t), "c1", nil); err == nil {
		t.Fatal("empty command accepted")
	}
}
