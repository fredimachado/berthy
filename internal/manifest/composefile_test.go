package manifest

import "testing"

// A Compose app's file means the same wherever the manifest is read: the CLI on Windows and the agent on Linux accept and
// refuse the same files.
func TestComposeFileIsARelativePathInsideTheManifestDirectory(t *testing.T) {
	for file, ok := range map[string]bool{
		"compose.yml":           true,
		"deploy/compose.yml":    true,
		`deploy\compose.yml`:    true,
		"./compose.yml":         true,
		"a..b/compose.yml":      true,
		"..compose.yml":         true,
		"":                      false,
		"/etc/compose.yml":      false,
		`\srv\compose.yml`:      false,
		`\\host\share\c.yml`:    false,
		`\\?\C:\c.yml`:          false,
		`//host/share/c.yml`:    false,
		`C:\compose.yml`:        false,
		"c:/compose.yml":        false,
		"C:compose.yml":         false,
		"../compose.yml":        false,
		`..\compose.yml`:        false,
		"..":                    false,
		"a/..":                  false,
		"a/../../compose.yml":   false,
		`a\..\..\compose.yml`:   false,
		"a/b/../compose.yml":    false,
		"deploy/../compose.yml": false,
	} {
		if got := composeFileOK(file); got != ok {
			t.Errorf("composeFileOK(%q) = %v, want %v", file, got, ok)
		}
	}
}
