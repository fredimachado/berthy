package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
)

func serveImageLookup(w http.ResponseWriter, r *http.Request, d *deploy.Controller, action string) {
	body, err := readBody(w, r)
	if err != nil || !unambiguousJSON(body, reflect.TypeOf(api.ImageLookup{})) {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	var q api.ImageLookup
	if decodeBody(w, r, &q) != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if action == "resolve" {
		result, err := d.ResolveTag(r.Context(), q)
		if err != nil {
			writeError(w, 400, "lookup_failed", "image lookup failed")
			return
		}
		json.NewEncoder(w).Encode(result)
		return
	}
	result, err := d.ImageTags(r.Context(), q)
	if err != nil {
		writeError(w, 400, "lookup_failed", "image lookup failed")
		return
	}
	json.NewEncoder(w).Encode(result)
}

// unambiguousJSON applies Go's case-insensitive struct matching without folding user map keys.
func unambiguousJSON(body []byte, typ reflect.Type) bool {
	if !json.Valid(body) {
		return false
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		dec := json.NewDecoder(bytes.NewReader(body))
		tok, err := dec.Token()
		if err != nil || tok != json.Delim('{') {
			return true
		}
		seen := map[string]bool{}
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return false
			}
			key, ok := token.(string)
			if !ok {
				return false
			}
			var value json.RawMessage
			if dec.Decode(&value) != nil {
				return false
			}
			canonical := key
			var child reflect.Type
			if typ.Kind() == reflect.Map {
				child = typ.Elem()
			} else {
				for i := 0; i < typ.NumField(); i++ {
					f := typ.Field(i)
					name := strings.Split(f.Tag.Get("json"), ",")[0]
					if name == "" {
						name = f.Name
					}
					if name != "-" && strings.EqualFold(name, key) {
						canonical = name
						child = f.Type
						break
					}
				}
			}
			if seen[canonical] {
				return false
			}
			seen[canonical] = true
			if child != nil && !unambiguousJSON(value, child) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(body, &values) == nil {
			for _, v := range values {
				if !unambiguousJSON(v, typ.Elem()) {
					return false
				}
			}
		}
	}
	return true
}
