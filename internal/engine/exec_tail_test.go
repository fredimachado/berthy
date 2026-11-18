package engine

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestExecTailKeepsTheLastBytesOfAChattyCommand(t *testing.T) {
	// Three mebibytes of output in 1 MiB frames, the last of which ends in a marker that must survive.
	var b bytes.Buffer
	for i := 0; i < 3; i++ {
		var hdr [8]byte
		hdr[0] = byte(1 + i%2) // stdout and stderr share one buffer
		binary.BigEndian.PutUint32(hdr[4:], 1<<20)
		b.Write(hdr[:])
		b.WriteString(strings.Repeat("x", 1<<20-4) + "end" + string(rune('0'+i)))
	}
	exit, tail, err := execEngine(t, b.String(), "7").ExecTail(execCtx(t), "c1", []string{"true"}, 64<<10)
	if err != nil || exit != 7 || len(tail) != 64<<10 || !strings.HasSuffix(tail, "end2") {
		t.Fatalf("exec tail: exit %d, %d bytes, %v", exit, len(tail), err)
	}
	// Output that fits is returned whole.
	exit, tail, err = execEngine(t, execFrames(2, 10), "0").ExecTail(execCtx(t), "c1", []string{"true"}, 64<<10)
	if err != nil || exit != 0 || tail != strings.Repeat("x", 20) {
		t.Fatalf("short output: %d %q %v", exit, tail, err)
	}
}

func TestExecTailRefusesAMalformedStreamButKeepsWhatItRead(t *testing.T) {
	stream := execFrames(1, 5) + "garbage"
	_, tail, err := execEngine(t, stream, "0").ExecTail(execCtx(t), "c1", []string{"true"}, 64<<10)
	if err == nil || tail != "xxxxx" {
		t.Fatalf("malformed stream: %q %v", tail, err)
	}
}
