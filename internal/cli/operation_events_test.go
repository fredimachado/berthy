package cli

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestReadOperationEventsCompleteFrames(t *testing.T) {
	first := "id: 1\nevent: accepted\ndata: {}\n\n"
	warning := "id: 2\nevent: verify\ndata: {\"status\":\"warn\"}\n"
	for _, ending := range []string{"", "\r"} {
		var got []operationEvent
		last, err := readOperationEvents(strings.NewReader(first+warning+ending), 0, func(e operationEvent) { got = append(got, e) })
		if !errors.Is(err, io.ErrUnexpectedEOF) || last != 1 || len(got) != 1 || got[0].Kind != "accepted" {
			t.Fatalf("cut %q: last=%d events=%v err=%v", ending, last, got, err)
		}
	}
	stream := ": heartbeat\r\n\r\n" + strings.ReplaceAll(first, "\n", "\r\n") + warning + "\n" +
		"id: 2\nevent: verify\ndata: {\"status\":\"warn\"}\n\n" +
		"id: 3\nevent: succeeded\ndata: {}\n\n" +
		"event: terminal\ndata: {\"status\":\"succeeded\"}\n\n"
	var got []operationEvent
	last, err := readOperationEvents(strings.NewReader(stream), 1, func(e operationEvent) { got = append(got, e) })
	want := []operationEvent{{ID: 2, Kind: "verify", Data: []byte(`{"status":"warn"}`)}, {ID: 3, Kind: "succeeded", Data: []byte(`{}`)}, {Kind: "terminal", Data: []byte(`{"status":"succeeded"}`)}}
	if err != nil || last != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("last=%d events=%+v err=%v", last, got, err)
	}
}

func TestReadOperationEventsRejectsInvalidFrames(t *testing.T) {
	for _, frame := range []string{
		"id: 0\nevent: verify\ndata: {}\n\n", "id: -1\nevent: verify\ndata: {}\n\n",
		"id: 1\nid: 2\nevent: verify\ndata: {}\n\n", "id: 1\nevent: verify\nevent: verify\ndata: {}\n\n",
		"id: 1\nevent: verify\ndata: {}\ndata: {}\n\n", "id: 1\nevent: bad-name\ndata: {}\n\n",
		"id: 1\nevent: verify\ndata: {\n\n", "id: 1\nevent: verify\n\n",
		"event: verify\ndata: {}\n\n", "id: 1\nevent: verify\ndata: {}\nextra: x\n\n",
		"id: 1\nevent: verify\ndata: \"" + strings.Repeat("x", 1<<20) + "\"\n\n",
		":" + strings.Repeat("x", 1<<20) + "\n\n",
	} {
		t.Run(frameName(frame), func(t *testing.T) {
			count := 0
			last, err := readOperationEvents(strings.NewReader(frame), 7, func(operationEvent) { count++ })
			if err == nil || last != 7 || count != 0 {
				t.Fatalf("last=%d count=%d err=%v", last, count, err)
			}
			if err.Error() != "invalid operation event frame" && err.Error() != "operation event line too large" {
				t.Fatalf("diagnostic exposes input: %v", err)
			}
		})
	}
}

func frameName(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func TestReadOperationEventsEOFAndCopiedData(t *testing.T) {
	for _, input := range []string{"", "\n", ": heartbeat\n\n"} {
		last, err := readOperationEvents(strings.NewReader(input), 9, func(operationEvent) { t.Fatal("unexpected event") })
		if err != nil || last != 9 {
			t.Fatalf("%q: %d, %v", input, last, err)
		}
	}
	var got []operationEvent
	last, err := readOperationEvents(strings.NewReader("id: 1\nevent: verify\ndata: {}\n\nid: 2\nevent: verify\ndata: {\"value\":2}\n\n"), 0, func(e operationEvent) { got = append(got, e) })
	if err != nil || last != 2 || string(got[0].Data) != "{}" {
		t.Fatalf("last=%d events=%v err=%v", last, got, err)
	}
}

func TestReadOperationEventsRejectsNumberedTerminalAndInvalidUTF8(t *testing.T) {
	for name, frame := range map[string]string{
		"numbered terminal":           "id: 8\nevent: terminal\ndata: {}\n\n",
		"duplicate numbered terminal": "id: 7\nevent: terminal\ndata: {}\n\n",
		"invalid UTF-8":               "id: 8\nevent: verify\ndata: {\"value\":\"\xff\"}\n\n",
		"invalid UTF-8 terminal":      "event: terminal\ndata: {\"value\":\"\xff\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			count := 0
			last, err := readOperationEvents(strings.NewReader(frame), 7, func(operationEvent) { count++ })
			if err == nil || err.Error() != "invalid operation event frame" || last != 7 || count != 0 {
				t.Fatalf("last=%d count=%d err=%v", last, count, err)
			}
		})
	}
}
