package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// stateManifest returns a valid JSON manifest with one image app, one S3 destination and the given state_backup block.
func stateManifest(block string) string {
	return `{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},
	"destinations":{"offsite":{"type":"s3","endpoint":"https://s3.example.test","bucket":"bkt","access_key_secret":"S3_ACCESS_KEY","secret_key_secret":"S3_SECRET_KEY"}},
	"state_backup":` + block + `}`
}

func TestStateBackupBlockValidation(t *testing.T) {
	valid := map[string]string{
		"destination only":      `{"destination":"offsite"}`,
		"every field":           `{"destination":"offsite","prefix":"berthy/lab","schedule":"@daily","keep":3}`,
		"keep at the largest":   `{"destination":"offsite","keep":1000}`,
		"prefix with slashes":   `{"destination":"offsite","prefix":"/state/lab/"}`,
		"schedule with a macro": `{"destination":"offsite","schedule":"@hourly"}`,
	}
	for name, block := range valid {
		if _, err := DecodeJSON([]byte(stateManifest(block))); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	invalid := map[string]string{
		"no destination":          `{}`,
		"unknown destination":     `{"destination":"nowhere"}`,
		"schedule does not parse": `{"destination":"offsite","schedule":"every day"}`,
		"keep zero":               `{"destination":"offsite","keep":0}`,
		"keep above 1000":         `{"destination":"offsite","keep":1001}`,
		"prefix with dot segment": `{"destination":"offsite","prefix":"a/../b"}`,
		"prefix with a space":     `{"destination":"offsite","prefix":"a b"}`,
		"unknown field":           `{"destination":"offsite","retain":3}`,
	}
	for name, block := range invalid {
		if _, err := DecodeJSON([]byte(stateManifest(block))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestStateBackupDefaultsAreScheduleAndKeep(t *testing.T) {
	m, err := DecodeJSON([]byte(stateManifest(`{"destination":"offsite"}`)))
	if err != nil {
		t.Fatal(err)
	}
	sb := m.StateBackup
	if sb == nil || sb.Schedule != "17 3 * * *" || sb.Keep == nil || *sb.Keep != 14 || sb.Prefix != "" {
		t.Fatalf("defaults: %+v", sb)
	}
	m, err = DecodeJSON([]byte(`{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`))
	if err != nil || m.StateBackup != nil {
		t.Fatalf("a manifest without the block: %+v %v", m, err)
	}
}

func TestStateBackupBlockLoadsFromYAML(t *testing.T) {
	dir := t.TempDir()
	yml := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
destinations:
  offsite:
    type: s3
    endpoint: https://s3.example.test
    bucket: bkt
    access_key_secret: S3_ACCESS_KEY
    secret_key_secret: S3_SECRET_KEY
state_backup:
  destination: offsite
  keep: 7
`
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(yml), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if m.StateBackup == nil || m.StateBackup.Destination != "offsite" || *m.StateBackup.Keep != 7 || m.StateBackup.Schedule != "17 3 * * *" {
		t.Fatalf("loaded: %+v", m.StateBackup)
	}
}
