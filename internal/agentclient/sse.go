package agentclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// The ways ReadEvents ends other than by the stream's own events.
var (
	// ErrStopEvents is what a handler returns to end the stream normally: ReadEvents then returns nil.
	ErrStopEvents = errors.New("stop reading events")
	// ErrInvalidEvent is an event whose data is not one JSON object, or that is over 16 MiB.
	ErrInvalidEvent = errors.New("invalid stream event")
	// ErrStreamEnded is a stream that ended without the handler stopping it.
	ErrStreamEnded = errors.New("agent stream ended unexpectedly")
	// ErrStreamRead is a stream that could not be read to its end.
	ErrStreamRead = errors.New("agent stream could not be read")
)

// maxEventBytes is the most data one event may carry.
const maxEventBytes = 16 << 20

// ReadEvents reads a server-sent event stream and calls fn with the name and the JSON data of each event that has data.
// Comment lines and events without data are skipped. It returns nil when fn returns ErrStopEvents, fn's own error when it
// returns another, ErrInvalidEvent for data that is not one JSON object, ctx's error when ctx ended the read, ErrStreamRead
// when the body failed and ErrStreamEnded when it ended.
func ReadEvents(ctx context.Context, r io.Reader, fn func(event, data string) error) error {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), maxEventBytes)
	event := ""
	var data []string
	size := 0
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			if len(data) == 0 {
				event = ""
				continue
			}
			payload := strings.Join(data, "\n")
			data = nil
			size = 0
			if !json.Valid([]byte(payload)) || len(payload) == 0 || payload[0] != '{' {
				return ErrInvalidEvent
			}
			name := event
			event = ""
			if err := fn(name, payload); err != nil {
				if errors.Is(err, ErrStopEvents) {
					return nil
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			size += len(value) + 1
			if size > maxEventBytes {
				return ErrInvalidEvent
			}
			data = append(data, value)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scan.Err() != nil {
		return ErrStreamRead
	}
	return ErrStreamEnded
}
