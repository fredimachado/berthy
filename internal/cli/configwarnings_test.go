package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// warnManifest is valid, and its one warning is on line 8 of berthy.yml: start-first over a read-write volume.
const warnManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    deploy:
      order: start-first
    roles:
      web:
        port: 8080
        volumes:
          - uploads:/data
`

func TestConfigValidatePrintsWarningsAndStillPasses(t *testing.T) {
	dir := validateDir(t, warnManifest, "")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &errs)
	want := "warning: berthy.yml:8:7: apps.api.deploy.order: start-first lets two versions of role web write volume uploads at once\n"
	if code != 0 || out.String() != "manifest valid\n" || errs.String() != want {
		t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant stderr %q", code, out.String(), errs.String(), want)
	}
}

func TestConfigValidateNoWarningsPrintsNothingExtra(t *testing.T) {
	dir := validateDir(t, strings.Replace(warnManifest, "order: start-first", "order: stop-first", 1), "")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &errs)
	if code != 0 || out.String() != "manifest valid\n" || errs.Len() != 0 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out.String(), errs.String())
	}
}

func TestConfigValidateJSONCarriesWarnings(t *testing.T) {
	dir := validateDir(t, warnManifest, "")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir, "--json"}, &out, &errs)
	var got struct {
		Warnings []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
			File    string `json:"file"`
			Line    int    `json:"line"`
			Column  int    `json:"column"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); code != 0 || err != nil || errs.Len() != 0 {
		t.Fatalf("exit %d, %v\nstdout %q\nstderr %q", code, err, out.String(), errs.String())
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Path != "apps.api.deploy.order" || got.Warnings[0].File != "berthy.yml" || got.Warnings[0].Line != 8 || got.Warnings[0].Column != 7 {
		t.Fatalf("warnings %+v", got.Warnings)
	}
}

func TestConfigValidateJSONHasAnEmptyListWithNoWarnings(t *testing.T) {
	dir := validateDir(t, strings.Replace(warnManifest, "order: start-first", "order: stop-first", 1), "")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir, "--json"}, &out, &errs)
	if code != 0 || !strings.Contains(out.String(), `"warnings": []`) {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out.String(), errs.String())
	}
}
