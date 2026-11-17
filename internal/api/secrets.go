package api

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"berthy/internal/manifest"
)

// MaxSecretValue is the largest value a secret may have, in bytes.
const MaxSecretValue = 64 << 10

// MaxSecretImport is the most secrets one import may hold.
const MaxSecretImport = 500

// MaxRequestBody is the largest request body the agent reads. A secrets import has to fit in one request.
const MaxRequestBody = 1 << 20

var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SecretEntry is one secret of an import: its name and the value it takes.
type SecretEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SecretImport is the body of POST secrets/import.
type SecretImport struct {
	Secrets []SecretEntry `json:"secrets"`
	DryRun  bool          `json:"dry_run,omitempty"`
}

// SecretImportPlan is the dry run of an import and the plan a held import shows: the names it would create and the names it
// would replace, sorted. It never holds a value.
type SecretImportPlan struct {
	Action  string   `json:"action"`
	Create  []string `json:"create"`
	Replace []string `json:"replace"`
}

// SecretImportResult is what an import stored: the names it created and the names it replaced, sorted.
type SecretImportResult struct {
	Created  []string `json:"created"`
	Replaced []string `json:"replaced"`
}

// CheckSecretName reports whether name may be a secret: a name a container variable can take, other than the tunnel token,
// which managed ingress owns. The error never contains name, so a line that is not a name is not echoed back.
func CheckSecretName(name string) error {
	switch {
	case name == manifest.TunnelTokenSecret:
		return errors.New(manifest.TunnelTokenSecret + " is reserved for managed ingress")
	case !secretName.MatchString(name):
		return errors.New("the name is not a valid secret name: letters, digits and _, not starting with a digit")
	}
	return nil
}

// CheckSecretImport checks an import where it enters, on the CLI and on the agent alike: one to MaxSecretImport entries, each
// with a valid name that appears once, and a value of at most MaxSecretValue bytes with no NUL. The first failure is returned.
// Its text names the entry by its number, or by its name when the name is valid, and never says what a value is.
func CheckSecretImport(entries []SecretEntry) error {
	if len(entries) == 0 {
		return errors.New("the import has no secrets")
	}
	if len(entries) > MaxSecretImport {
		return fmt.Errorf("the import has %d secrets: at most %d", len(entries), MaxSecretImport)
	}
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if err := CheckSecretName(e.Name); err != nil {
			return fmt.Errorf("entry %d: %w", i+1, err)
		}
		if seen[e.Name] {
			return fmt.Errorf("%s appears more than once", e.Name)
		}
		seen[e.Name] = true
		if len(e.Value) > MaxSecretValue {
			return fmt.Errorf("%s: the value is larger than %d bytes", e.Name, MaxSecretValue)
		}
		if strings.Contains(e.Value, "\x00") {
			return fmt.Errorf("%s: the value contains a NUL byte", e.Name)
		}
	}
	return nil
}
