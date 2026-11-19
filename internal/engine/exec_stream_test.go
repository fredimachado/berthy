package engine

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestExecStreamSendsStdoutToTheWriterAndKeepsTheTailOfStderr(t *testing.T) {
	var stream bytes.Buffer
	stream.WriteString(frame(1, "hello "))
	stream.WriteString(frame(2, "warning one\n"))
	stream.WriteString(frame(1, "world"))
	stream.WriteString(frame(2, "the end\n"))
	var stdout bytes.Buffer
	exit, tail, err := execEngine(t, stream.String(), "3").ExecStream(execCtx(t), "c1", []string{"dump"}, nil, &stdout, 10)
	if err != nil || exit != 3 || stdout.String() != "hello world" || tail != "e\nthe end\n" {
		t.Fatalf("exec stream: exit %d, stdout %q, tail %q, %v", exit, stdout.String(), tail, err)
	}
}

func TestExecStreamStopsWhenTheWriterFails(t *testing.T) {
	broken := errors.New("upload closed")
	_, _, err := execEngine(t, frame(1, "abc"), "0").ExecStream(execCtx(t), "c1", []string{"dump"}, nil, brokenWriter{broken}, 10)
	if !errors.Is(err, broken) {
		t.Fatalf("error %v, want the writer's", err)
	}
}

type brokenWriter struct{ err error }

func (w brokenWriter) Write([]byte) (int, error) { return 0, w.err }

func TestExecStreamSendsStdinAndClosesItThenReadsTheExitCode(t *testing.T) {
	gotCh := make(chan string, 1)
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start": switchTo(t, func(conn net.Conn, br io.Reader) {
			b, _ := io.ReadAll(br) // returns only when the client's write side ends
			gotCh <- string(b)
			io.WriteString(conn, frame(2, "restored "+string(b)+"\n"))
		}),
		"GET /v1.44/exec/e1/json": jsonReply(200, `{"Running":false,"ExitCode":4}`),
	})
	var stdout bytes.Buffer
	exit, tail, err := d.engine.ExecStream(attachCtx(t), "c1", []string{"pg_restore"}, strings.NewReader("the dump"), &stdout, 1<<10)
	got := <-gotCh
	if err != nil || exit != 4 || got != "the dump" || tail != "restored the dump\n" || stdout.Len() != 0 {
		t.Fatalf("exec stream: exit %d, stdin %q, tail %q, stdout %q, %v", exit, got, tail, stdout.String(), err)
	}
	if !strings.Contains(d.call(0).Body, `"AttachStdin":true`) {
		t.Errorf("exec was created without stdin: %s", d.call(0).Body)
	}
}

func TestExecStreamReportsAnInputThatCannotBeRead(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start": switchTo(t, func(conn net.Conn, br io.Reader) {
			io.Copy(io.Discard, br)
		}),
		"GET /v1.44/exec/e1/json": jsonReply(200, `{"Running":false,"ExitCode":0}`),
	})
	lost := errors.New("storage lost")
	_, _, err := d.engine.ExecStream(attachCtx(t), "c1", []string{"pg_restore"}, io.MultiReader(strings.NewReader("part"), failingReader{lost}), io.Discard, 1<<10)
	if !errors.Is(err, lost) {
		t.Fatalf("error %v, want the input's", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
