package agent

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"berthy/internal/strictjson"
)

// planCallKey marks a request the approval gate serves for its plan. Only asPlan sets it.
type planCallKey struct{}

// asPlan marks a request so its route answers with a dry run, whatever the body says.
func asPlan(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), planCallKey{}, true))
}

// planning reports whether the gate asked for the plan of r.
func planning(r *http.Request) bool {
	v, _ := r.Context().Value(planCallKey{}).(bool)
	return v
}

// errNoDryRun refuses a planning call whose body type has no dry_run field.
var errNoDryRun = errors.New("the route has no dry run")

// decodeBody reads a JSON body into a struct pointer. Routes read bodies only through this or decodeOptionalBody.
// A planning call sets its dry_run field or fails closed.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	return readRequest(w, r, v, false)
}

// decodeOptionalBody also accepts an empty body or JSON whitespace as an empty object.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, v any) error {
	return readRequest(w, r, v, true)
}

func readRequest(w http.ResponseWriter, r *http.Request, v any, optional bool) error {
	b, err := readBody(w, r)
	if err != nil {
		return err
	}
	if optional && len(bytes.Trim(b, " \t\r\n")) == 0 {
		b = []byte("{}")
	}
	if err := strictjson.Unmarshal(b, v); err != nil {
		return err
	}
	if planning(r) && !setDryRun(v) {
		return errNoDryRun
	}
	return nil
}

// setDryRun sets the exported bool field whose JSON tag name is dry_run, if one exists.
func setDryRun(v any) bool {
	s := reflect.ValueOf(v).Elem()
	if s.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < s.NumField(); i++ {
		f := s.Type().Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.IsExported() && name == "dry_run" && f.Type.Kind() == reflect.Bool {
			s.Field(i).SetBool(true)
			return true
		}
	}
	return false
}
