package agent

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
)

func TestSetDryRunFindsTheTagNameBeforeTheComma(t *testing.T) {
	a, b := &deploy.Request{}, &deploy.ApplyRequest{}
	c, d, e := &api.SecretImport{}, &api.SecretGenerate{}, &api.SecretsGenerateDeclared{}
	f := &struct {
		DryRun bool `json:"dry_run"`
	}{}
	for _, v := range []any{a, b, c, d, e, f} {
		if !setDryRun(v) {
			t.Errorf("not found in %T", v)
		}
	}
	if !a.DryRun || !b.DryRun || !c.DryRun || !d.DryRun || !e.DryRun || !f.DryRun {
		t.Fatal("field not set")
	}
	for _, v := range []any{&struct {
		DryRun bool `json:"dry_runs"`
	}{}, &struct {
		DryRun bool `json:"x,dry_run"`
	}{}, reflect.New(reflect.StructOf([]reflect.StructField{{Name: "dryRun", PkgPath: "agent", Type: reflect.TypeOf(false), Tag: `json:"dry_run"`}})).Interface()} {
		if setDryRun(v) {
			t.Errorf("matched %T", v)
		}
	}
}

func TestPlanningFailsClosedWithoutDryRun(t *testing.T) {
	r := asPlan(httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
	if err := decodeBody(httptest.NewRecorder(), r, &struct{}{}); !errors.Is(err, errNoDryRun) {
		t.Fatalf("planning without dry_run: %v", err)
	}
}
