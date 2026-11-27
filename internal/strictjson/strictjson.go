// Package strictjson decodes JSON that can mean only one thing.
package strictjson

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var (
	ErrInvalid     = errors.New("not one valid JSON value")
	ErrNotObject   = errors.New("a JSON object is required")
	ErrRepeatedKey = errors.New("a key appears twice in one object")
	ErrUnknownKey  = errors.New("a key is not the name of a field")
	ErrEmbedded    = errors.New("an embedded struct field is not supported")
)

var (
	jsonUnmarshaler = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textUnmarshaler = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// Unmarshal decodes data into v, a non-nil pointer, as encoding/json does, after checking that data is one valid JSON value,
// a struct receives an object at the top level, no object has a repeated key, and struct keys match exported field names
// exactly. Map keys are preserved. For custom unmarshalers and interface values only repeated keys are checked. Errors wrap
// one of the exported errors and name key paths, never values.
func Unmarshal(data []byte, v any) error {
	if !json.Valid(data) {
		return fmt.Errorf("$: %w", ErrInvalid)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("$: %w", ErrInvalid)
	}
	t := targetType(rv.Type().Elem())
	if t != nil && t.Kind() == reflect.Struct && bytes.TrimLeft(data, " \t\r\n")[0] != '{' {
		return fmt.Errorf("$: %w", ErrNotObject)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	w := walker{dec: dec}
	if err := w.walk(t); err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		path := "$"
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			path = fieldPath(rv.Type().Elem(), mismatch.Field)
		}
		return fmt.Errorf("%s: %w", path, ErrInvalid)
	}
	return nil
}

// fieldPath takes only names from the target type, never text supplied by an unmarshaler.
func fieldPath(t reflect.Type, field string) string {
	var path []string
	for _, name := range strings.Split(field, ".") {
		t = targetType(t)
		for t != nil && (t.Kind() == reflect.Map || t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			t = targetType(t.Elem())
		}
		if t == nil || t.Kind() != reflect.Struct {
			return "$"
		}
		fields, err := structFields(t)
		if err != nil {
			return "$"
		}
		child, ok := fields[name]
		if !ok {
			return "$"
		}
		path = append(path, name)
		t = child
	}
	return formatPath(path)
}

func formatPath(path []string) string {
	var b strings.Builder
	b.WriteByte('$')
	for _, key := range path {
		fmt.Fprintf(&b, "[%q]", key)
	}
	return b.String()
}

// targetType removes pointers, but preserves custom and interface values as opaque.
func targetType(t reflect.Type) reflect.Type {
	for t != nil {
		if t.Implements(jsonUnmarshaler) || t.Implements(textUnmarshaler) ||
			(t.Kind() != reflect.Pointer && (reflect.PointerTo(t).Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler))) || t.Kind() == reflect.Interface {
			return nil
		}
		if t.Kind() != reflect.Pointer {
			return t
		}
		t = t.Elem()
	}
	return nil
}

type walker struct {
	dec  *json.Decoder
	path []string
}

func (w *walker) walk(typ reflect.Type) error {
	tok, err := w.dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", formatPath(w.path), ErrInvalid)
	}
	typ = targetType(typ)
	switch tok {
	case json.Delim('{'):
		var fields map[string]reflect.Type
		if typ != nil && typ.Kind() == reflect.Struct {
			fields, err = structFields(typ)
			if err != nil {
				return fmt.Errorf("%s: %w", formatPath(w.path), err)
			}
		}
		seen := make(map[string]bool)
		for w.dec.More() {
			keyToken, err := w.dec.Token()
			if err != nil {
				return fmt.Errorf("%s: %w", formatPath(w.path), ErrInvalid)
			}
			key := keyToken.(string)
			w.path = append(w.path, key)
			if seen[key] {
				err := fmt.Errorf("%s: %w", formatPath(w.path), ErrRepeatedKey)
				w.path = w.path[:len(w.path)-1]
				return err
			}
			seen[key] = true
			var child reflect.Type
			if fields != nil {
				var ok bool
				child, ok = fields[key]
				if !ok {
					err := fmt.Errorf("%s: %w", formatPath(w.path), ErrUnknownKey)
					w.path = w.path[:len(w.path)-1]
					return err
				}
			} else if typ != nil && typ.Kind() == reflect.Map {
				child = typ.Elem()
			}
			err = w.walk(child)
			w.path = w.path[:len(w.path)-1]
			if err != nil {
				return err
			}
		}
		_, err = w.dec.Token()
	case json.Delim('['):
		var child reflect.Type
		if typ != nil && (typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array) {
			child = typ.Elem()
		}
		for w.dec.More() {
			if err := w.walk(child); err != nil {
				return err
			}
		}
		_, err = w.dec.Token()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", formatPath(w.path), ErrInvalid)
	}
	return nil
}

func structFields(t reflect.Type) (map[string]reflect.Type, error) {
	fields := make(map[string]reflect.Type)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			return nil, ErrEmbedded
		}
		if f.Anonymous || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields, nil
}
