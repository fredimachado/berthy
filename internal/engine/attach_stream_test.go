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

func TestAttachStreamAttachesBeforeTheStartAndStreamsBothWays(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/attach": switchTo(t, func(conn net.Conn, br io.Reader) {
			b, _ := io.ReadAll(br)
			io.WriteString(conn, frame(1, "out:"+string(b))+frame(2, "err\n"))
		}),
	})
	attached := false
	var stdout bytes.Buffer
	tail, inErr, err := d.engine.AttachStream(attachCtx(t), "c1", strings.NewReader("abc"), &stdout, 100, func() error {
		d.mu.Lock()
		attached = len(d.calls) == 1
		d.mu.Unlock()
		return nil
	})
	if err != nil || inErr != nil || !attached || stdout.String() != "out:abc" || tail != "err\n" {
		t.Fatalf("attach stream: attached before start %v, stdout %q, tail %q, %v %v", attached, stdout.String(), tail, inErr, err)
	}
}

func TestAttachStreamWithoutInputEndsTheInputAtOnce(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/attach": switchTo(t, func(conn net.Conn, br io.Reader) {
			b, _ := io.ReadAll(br) // returns when the client ends its input
			io.WriteString(conn, frame(1, "read "+string(rune('0'+len(b)))+" bytes"))
		}),
	})
	var stdout bytes.Buffer
	_, _, err := d.engine.AttachStream(attachCtx(t), "c1", nil, &stdout, 100, func() error { return nil })
	if err != nil || stdout.String() != "read 0 bytes" {
		t.Fatalf("attach stream: %q %v", stdout.String(), err)
	}
}

func TestAttachStreamEndsWhenTheContainerCannotStart(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/attach": switchTo(t, echo),
	})
	refused := errors.New("no start")
	if _, _, err := d.engine.AttachStream(attachCtx(t), "c1", nil, io.Discard, 100, func() error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("error %v", err)
	}
}
