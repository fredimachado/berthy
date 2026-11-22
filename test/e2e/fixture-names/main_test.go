package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNamesUseManifestRulesAndRejectAnotherProject(t *testing.T) {
	dir := t.TempDir()
	data := `project: shop
network: net
apps:
  web:
    kind: image
    image: alpine
    roles:
      main:
        volumes: ["data:/data", "cache:/cache:ro"]
  db:
    kind: compose
    file: compose.yml
  legacy:
    kind: compose
    file: old.yml
    project_name: legacy-db
`
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := names(dir, "shop", "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"compose\tshop-prod-db", "compose\tlegacy-db", "service\tshop-prod-web-main", "volume\tshop-prod_data", "volume\tshop-prod_cache"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got, err = names(dir, "shop", "prod", "web")
	if err != nil || !reflect.DeepEqual(got, []string{"service\tshop-prod-web-main", "volume\tshop-prod_data", "volume\tshop-prod_cache"}) {
		t.Fatalf("web names %v: %v", got, err)
	}
	if _, err := names(dir, "other", "prod", ""); err == nil {
		t.Fatal("another project's manifest accepted")
	}
}
