package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
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
	reader := bufio.NewReaderSize(r, maxLine+1)
	last = after
	var event operationEvent
	var idSet, kindSet, dataSet, pending bool
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) || len(line) > maxLine {
			return last, oversized
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if pending || len(line) != 0 {
					return last, io.ErrUnexpectedEOF
				}
				return last, nil
			}
			return last, readErr
		}
		line = line[:len(line)-1]
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			if idSet || kindSet || dataSet {
				if !kindSet || !dataSet || (!idSet && event.Kind != "terminal") || !json.Valid(event.Data) {
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
}
