// Command gendocs rewrites the manifest reference of the user guide from the definition in internal/manifest/keys.go. Run it
// with go generate ./internal/manifest, which is how TestUserGuideReferenceIsCurrent tells you to.
package main

import (
	"flag"
	"fmt"
	"os"

	"berthy/internal/manifest"
)

func main() {
	guide := flag.String("guide", "../../docs/user-guide.md", "the user guide, relative to the directory it runs in (the manifest package's)")
	flag.Parse()
	if err := run(*guide); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func run(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := manifest.UpdateGuide(string(raw))
	if err != nil {
		return err
	}
	if updated == string(raw) {
		return nil
	}
	return os.WriteFile(path, []byte(updated), info.Mode().Perm())
}
