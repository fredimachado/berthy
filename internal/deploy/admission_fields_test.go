package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

func admissionSample() Request {
	applied, replicas := int64(12), 3
	return Request{
		App: "web", Version: "digest", VersionTag: "tag", ExpectedApplied: &applied,
		Commit: "commit", Modified: true, RequestID: "request", Manifest: manifest.Manifest{Project: "shop"},
		DryRun: true, Adopt: true, Kind: "deploy", TargetRelease: 5, Role: "web", Replicas: &replicas,
		RevertSchema: true, Command: []string{"echo"}, Service: "service", Timeout: "30m",
		ReplaceExisting: true, ComposeFile: "compose", AllowRecreate: true, Volumes: true,
		Confirm: "test", Tunnel: true, RestoreKey: "key", RestoreInto: "volume", KeepVersion: true, KeepDigest: "digest",
	}
}

// Visibility must be decided for each exported request field.
var publicAdmissionFields = map[string]bool{
	"App": true, "Version": true, "VersionTag": true, "ExpectedApplied": true, "Commit": true,
	"Modified": true, "Adopt": true, "TargetRelease": true, "RevertSchema": true,
	"ReplaceExisting": true, "AllowRecreate": true, "Volumes": true, "Tunnel": true,
	"RestoreKey": true, "RestoreInto": true, "KeepVersion": true, "KeepDigest": true,
	"RequestID": false, "Manifest": false, "DryRun": false, "Kind": false, "Role": false,
	"Replicas": false, "Command": false, "Service": false, "Timeout": false, "ComposeFile": false, "Confirm": false,
}

func admissionFields(t *testing.T) []reflect.StructField {
	t.Helper()
	sample := reflect.ValueOf(admissionSample())
	var fields []reflect.StructField
	for i := 0; i < sample.NumField(); i++ {
		field := sample.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		if name := strings.Split(field.Tag.Get("json"), ",")[0]; name == "" || name == "-" {
			t.Fatalf("%s has no JSON name", field.Name)
		}
		if sample.Field(i).IsZero() {
			t.Fatalf("%s has no nonzero sample", field.Name)
		}
		if _, ok := publicAdmissionFields[field.Name]; !ok {
			t.Fatalf("%s has no public visibility decision", field.Name)
		}
		fields = append(fields, field)
	}
	return fields
}

func seedAdmission(t *testing.T, s *state.Store, req Request) state.Operation {
	t.Helper()
	copyReq := req
	if copyReq.Kind == "ingress" {
		copyReq.App = manifest.ConnectorAppName
	}
	copyReq.RequestID = ""
	raw, err := json.Marshal(copyReq)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: req.RequestID, ActorID: "admin", Kind: "deploy", RequestHash: sha256.Sum256(raw), PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), op.ID, state.Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestAdmissionHashCoversRequestFields(t *testing.T) {
	fields := admissionFields(t)
	req := admissionSample()
	req.DryRun = false
	s := openStore(t)
	op := seedAdmission(t, s, req)
	c := Controller{Store: s}
	actor := Actor{ID: "admin", Admin: true}
	got, created, err := c.Admit(context.Background(), actor, req)
	if err != nil || created || got.ID != op.ID {
		t.Fatalf("unchanged replay: %s, created %v, error %v", got.ID, created, err)
	}
	for _, field := range fields {
		if field.Name == "RequestID" || field.Name == "DryRun" {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			changed := req
			v := reflect.ValueOf(&changed).Elem().FieldByIndex(field.Index)
			v.SetZero()
			_, _, err := c.Admit(context.Background(), actor, changed)
			if !errors.Is(err, state.ErrConflict) {
				t.Fatalf("changed %s: got %v, want conflict", field.Name, err)
			}
		})
	}
	dry := req
	dry.DryRun = true
	if _, _, err := c.Admit(context.Background(), actor, dry); err == nil || err.Error() != "use Plan for dry run" {
		t.Fatalf("dry run: %v", err)
	}
	other := req
	other.RequestID = "another"
	otherOp := seedAdmission(t, s, other)
	if got, created, err := c.Admit(context.Background(), actor, other); err != nil || created || got.ID != otherOp.ID {
		t.Fatalf("request ID lookup: %s, %v, %v", got.ID, created, err)
	}
	ingress := req
	ingress.Kind = "ingress"
	ingress.RequestID = "ingress"
	ingressOp := seedAdmission(t, s, ingress)
	if got, created, err := c.Admit(context.Background(), actor, ingress); err != nil || created || got.ID != ingressOp.ID {
		t.Fatalf("ingress replay: %s, %v, %v", got.ID, created, err)
	}
}

func TestPublicAdmissionCoversRequestFields(t *testing.T) {
	fields := admissionFields(t)
	zero := map[string]any{"app": "", "version": "", "commit": "", "adopt": false}
	check := func(t *testing.T, req Request, want map[string]any) {
		t.Helper()
		raw, err := req.publicRequest()
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("public request: got %#v, want %#v", got, want)
		}
	}
	check(t, Request{}, zero)
	sample := admissionSample()
	for _, field := range fields {
		t.Run(field.Name, func(t *testing.T) {
			var req Request
			v := reflect.ValueOf(&req).Elem().FieldByIndex(field.Index)
			v.Set(reflect.ValueOf(sample).FieldByIndex(field.Index))
			want := make(map[string]any, len(zero)+1)
			for k, value := range zero {
				want[k] = value
			}
			if publicAdmissionFields[field.Name] {
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				raw, err := json.Marshal(v.Interface())
				if err != nil {
					t.Fatal(err)
				}
				var value any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				want[name] = value
			}
			check(t, req, want)
			v.SetZero()
			check(t, req, zero)
		})
	}
	raw, err := sample.publicRequest()
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		_, present := all[name]
		if present != publicAdmissionFields[field.Name] {
			t.Errorf("%s presence %v", field.Name, present)
		}
	}
	s := openStore(t)
	if _, _, err := s.Admit(context.Background(), state.Admission{RequestID: "public", ActorID: "admin", Kind: "deploy", PublicRequest: raw}); err != nil {
		t.Fatal(err)
	}
}
