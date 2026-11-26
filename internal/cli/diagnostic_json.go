package cli

import (
	"bytes"
	"encoding/json"
	"strings"
)

// redactDiagnosticJSON preserves the original JSON and any trailing bytes, changing only selected string values.
func redactDiagnosticJSON(data []byte, registryOnly bool, values ...string) []byte {
	return redactDiagnosticJSONWith(data, registryOnly, func(s string) string { return redactSecretText(s, values...) })
}

func redactDiagnosticJSONWith(data []byte, registryOnly bool, redact func(string) string) []byte {
	type edit struct {
		start, end int
		value      []byte
	}
	var edits []edit
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value func(string) error
	value = func(path string) error {
		start := int(dec.InputOffset())
		for start < len(data) && (data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t' || data[start] == ':' || data[start] == ',') {
			start++
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch v := tok.(type) {
		case string:
			if !registryOnly || path == "diagnostic" {
				safe := redact(v)
				if safe != v {
					encoded, _ := json.Marshal(safe)
					edits = append(edits, edit{start, int(dec.InputOffset()), []byte(escapeJSONC1(string(encoded)))})
				}
			}
		case json.Delim:
			switch v {
			case '{':
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok {
						return &json.UnmarshalTypeError{}
					}
					child := "other"
					if path == "diagnostic" {
						child = "diagnostic"
					} else if path == "root" {
						for _, field := range []string{"registry", "services_error", "services"} {
							if strings.EqualFold(name, field) {
								child = "diagnostic"
								break
							}
						}
					}
					if err := value(child); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			case '[':
				for dec.More() {
					child := "other"
					if path == "diagnostic" {
						child = "diagnostic"
					}
					if err := value(child); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			}
		}
		return nil
	}
	if value("root") != nil || len(edits) == 0 {
		return data
	}
	var out bytes.Buffer
	last := 0
	for _, e := range edits {
		out.Write(data[last:e.start])
		out.Write(e.value)
		last = e.end
	}
	out.Write(data[last:])
	return out.Bytes()
}
