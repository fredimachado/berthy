package manifest

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"berthy/internal/scope"

	"go.yaml.in/yaml/v3"
)

// DefaultDir prefers the working directory's base manifest. Only a missing base
// permits the .berthy fallback. Callers that have an explicit directory use it directly.
func DefaultDir() string {
	if _, err := os.Lstat("berthy.yml"); !errors.Is(err, fs.ErrNotExist) {
		return "."
	}
	if _, err := os.Lstat(filepath.Join(".berthy", "berthy.yml")); err == nil || errors.Is(err, fs.ErrPermission) {
		return ".berthy"
	}
	return "."
}

// ProjectName reads the project of dir's berthy.yml without loading the manifest: the rest of the file may be wrong. ok is
// false when dir has no berthy.yml. A berthy.yml whose project is missing or invalid is an error with ok true. An overlay is
// not read: it cannot set the project (overlayMerge).
func ProjectName(dir string) (name string, ok bool, err error) {
	f, err := os.Open(filepath.Join(dir, "berthy.yml"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return "", true, err
	}
	if len(b) > maxInput {
		return "", true, errors.New("berthy.yml exceeds size limit")
	}
	var head struct {
		Project string `yaml:"project"`
	}
	if err := yaml.Unmarshal(b, &head); err != nil {
		return "", true, errors.New("berthy.yml is not valid YAML")
	}
	if !scope.ValidProject(head.Project) {
		return "", true, errors.New("berthy.yml has no valid project")
	}
	return head.Project, true, nil
}
