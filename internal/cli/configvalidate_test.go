package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validateBase = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        port: 70000
destinations:
  s3:
    type: s3
    bucket: my-bucket
    endpoint: https://s3.example.com
    access_key_secret: AK
    secret_key_secret: SK
backups:
  data:
    app: api
    volume: data
    destination: s3
`

func validateDir(t *testing.T, base, overlay string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if overlay != "" {
		if err := os.WriteFile(filepath.Join(dir, "berthy.prod.yml"), []byte(overlay), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestConfigValidateReportsEveryProblemWithItsPlace(t *testing.T) {
	dir := validateDir(t, validateBase, "backups:\n  data:\n    destination: nope\n")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "prod", "--manifest-dir", dir}, &out, &errs)
	want := "berthy.yml:9:9: apps.api.roles.web.port: app api role web: invalid settings\n" +
		"berthy.prod.yml:3:5: backups.data.destination: backup data: unknown destination\n" +
		"2 problems\n"
	if code != 1 || out.Len() != 0 || errs.String() != want {
		t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant stderr %q", code, out.String(), errs.String(), want)
	}
}

func TestConfigValidateCountsOneProblem(t *testing.T) {
	dir := validateDir(t, validateBase, "")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &errs)
	if code != 1 || !strings.HasSuffix(errs.String(), "\n1 problem\n") {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}

func TestConfigShowStillPrintsOneLine(t *testing.T) {
	dir := validateDir(t, validateBase, "backups:\n  data:\n    destination: nope\n")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "show", "-e", "prod", "--manifest-dir", dir}, &out, &errs)
	if code != 1 || errs.String() != "app api role web: invalid settings\n" {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}
