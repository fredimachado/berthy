package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxComposeFile is the largest Compose file Berthy stores or sends.
const MaxComposeFile = 512 << 10

// ErrComposeFile reports a Compose file that is empty, too large, not UTF-8, or contains NUL.
var ErrComposeFile = errors.New("compose file must be non-empty UTF-8 text of at most 512 KiB without NUL")

// CheckComposeFile reports whether b can be stored and sent as a Compose file.
func CheckComposeFile(b []byte) error {
	if len(b) == 0 || len(b) > MaxComposeFile || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return ErrComposeFile
	}
	return nil
}

// ComposeProject is the Compose project name an app runs under: its project_name, or <project>-<app>.
func (m *Manifest) ComposeProject(app string) string {
	if a, ok := m.Apps[app]; ok && a.ProjectName != "" {
		return a.ProjectName
	}
	return m.Project + "-" + app
}

// CheckComposeDeploy reports whether app exists and is a Compose app.
func (m *Manifest) CheckComposeDeploy(app string) error {
	a, ok := m.Apps[app]
	if !ok || a.Kind != "compose" {
		return fmt.Errorf("app %q is not a compose app", app)
	}
	return nil
}
