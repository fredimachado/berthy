package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

type operationEvent struct {
	ID   uint64
	Kind string
	Data json.RawMessage
}

// readOperationEvents commits only frames terminated by a blank line.
func readOperationEvents(r io.Reader, after uint64, accept func(operationEvent)) (last uint64, err error) {
	const maxLine = 1 << 20
	invalid := errors.New("invalid operation event frame")
	oversized := errors.New("operation event line too large")
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, maxLine+1)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			if i+1 > maxLine {
				return 0, nil, oversized
			}
			return i + 1, bytes.TrimSuffix(data[:i], []byte("\r")), nil
		}
		if atEOF && len(data) != 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	last = after
	var event operationEvent
	var idSet, kindSet, dataSet, pending bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if idSet || kindSet || dataSet {
				if !kindSet || !dataSet || (idSet && event.Kind == "terminal") || (!idSet && event.Kind != "terminal") || !utf8.Valid(event.Data) || !json.Valid(event.Data) {
					return last, invalid
				}
				if !idSet || event.ID > last {
					event.Data = append(json.RawMessage(nil), event.Data...)
					accept(event)
					if idSet {
						last = event.ID
					}
				}
			}
			event = operationEvent{}
			idSet, kindSet, dataSet, pending = false, false, false, false
			continue
		}
		pending = true
		if line[0] == ':' {
			continue
		}
		key, value, ok := strings.Cut(string(line), ":")
		if !ok {
			return last, invalid
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			if idSet || value == "" {
				return last, invalid
			}
			for _, ch := range value {
				if ch < '0' || ch > '9' {
					return last, invalid
				}
			}
			event.ID, err = strconv.ParseUint(value, 10, 64)
			if err != nil || event.ID == 0 {
				return last, invalid
			}
			idSet = true
		case "event":
			if kindSet || value == "" {
				return last, invalid
			}
			for _, ch := range value {
				if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
					return last, invalid
				}
			}
			event.Kind, kindSet = value, true
		case "data":
			if dataSet {
				return last, invalid
			}
			event.Data, dataSet = json.RawMessage(value), true
		default:
			return last, invalid
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return last, oversized
		}
		return last, err
	}
	if pending {
		return last, io.ErrUnexpectedEOF
	}
	return last, nil
}
