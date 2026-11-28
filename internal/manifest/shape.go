package manifest

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// jsonUnmarshaler is the interface of a type that reads its own JSON shape.
var jsonUnmarshaler = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// shapeWalk checks a parsed manifest against the Go types it is decoded into, and finds every mistake of shape in one pass:
// a key that is not a field, and a value of a kind the field cannot take. The decoder stops at the first value of the wrong
// kind and does not say where it is among maps and lists, so the walk names the path itself, and words the mistake as the
// decoder does.
type shapeWalk struct {
	// unknown are the keys that are not fields. The decoder ignores them, so the manifest can still be checked.
	unknown Problems
	// wrong are the values of a kind the field cannot take, after which the decoded manifest cannot be trusted to be checked.
	wrong Problems
}

// found is every problem of the walk: the unknown fields first, as they always were reported, then the wrong values.
func (w *shapeWalk) found() Problems {
	return append(slices.Clone(w.unknown), w.wrong...)
}

// walk checks node n against type t. path is where n is in the manifest. field and owner are what the decoder would report
// for it: the dotted names of the struct fields on the way down, which leave out map keys and list indexes, and the struct
// that holds the field.
func (w *shapeWalk) walk(n *yaml.Node, t reflect.Type, path, field, owner string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// A type with its own UnmarshalJSON checks its own shape. Its keys are not field names, as a variable's name is not.
	if reflect.PointerTo(t).Implements(jsonUnmarshaler) {
		var v any
		if n.Decode(&v) == nil {
			b, _ := json.Marshal(v)
			if err := reflect.New(t).Interface().(json.Unmarshaler).UnmarshalJSON(b); err != nil {
				w.shape(path, err.Error())
			}
		}
		return
	}
	if value := mismatch(n, t); value != "" {
		w.shape(path, (&json.UnmarshalTypeError{Value: value, Type: t, Struct: owner, Field: field}).Error())
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		// A supplied empty command option is indistinguishable from an omitted field after decoding Health.
		if t == reflect.TypeOf(Health{}) && keyIndex(n, "uptime") >= 0 {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i].Value, n.Content[i+1]
				switch key {
				case "interval", "timeout", "start_period", "start_interval":
					if value.Tag == "!!str" && value.Value == "" {
						w.shape(join(path, key), key+" is only allowed with health.test")
					}
				}
			}
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			name := n.Content[i].Value
			ft, ok := fields[name]
			if !ok {
				w.unknown.add(join(path, name), "unknown field %q", name)
				continue
			}
			w.walk(n.Content[i+1], ft, join(path, name), join(field, name), t.Name())
		}
	case reflect.Map:
		for i := 0; i+1 < len(n.Content); i += 2 {
			w.walk(n.Content[i+1], t.Elem(), join(path, n.Content[i].Value), field, owner)
		}
	case reflect.Slice:
		for i, v := range n.Content {
			w.walk(v, t.Elem(), join(path, strconv.Itoa(i)), field, owner)
		}
	}
}

func (w *shapeWalk) shape(path, message string) {
	w.wrong.add(path, "%s", message)
}

// mismatch returns the decoder's name for the value of n, such as "string" or "number 1.5", when a value of that kind cannot
// be decoded into t; otherwise "".
func mismatch(n *yaml.Node, t reflect.Type) string {
	var kind string // what n is, as the decoder says it
	switch n.Kind {
	case yaml.MappingNode:
		kind = "object"
	case yaml.SequenceNode:
		kind = "array"
	default:
		switch n.Tag {
		case "!!str":
			kind = "string"
		case "!!bool":
			kind = "bool"
		case "!!int", "!!float":
			kind = "number"
		default:
			return ""
		}
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if kind != "object" {
			return kind
		}
	case reflect.Slice:
		if kind != "array" {
			return kind
		}
	case reflect.String:
		if kind != "string" {
			return kind
		}
	case reflect.Bool:
		if kind != "bool" {
			return kind
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if kind != "number" {
			return kind
		}
		if n.Tag != "!!int" {
			return "number " + n.Value
		}
	case reflect.Float32, reflect.Float64:
		if kind != "number" {
			return kind
		}
	}
	return ""
}
