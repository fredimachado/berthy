package api

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// The length and the formats of a generated secret live in manifest; the API keeps the names callers already use.
const (
	GenerateDefaultBytes  = manifest.GenerateDefaultBytes
	GenerateMinBytes      = manifest.GenerateMinBytes
	GenerateMaxBytes      = manifest.GenerateMaxBytes
	GenerateDefaultFormat = manifest.GenerateDefaultFormat
)

// GenerateFormats are the formats a generated secret may take.
var GenerateFormats = manifest.GenerateFormats

// SecretGenerate is the body of POST secrets/generate. Bytes and Format may be left out, and then take their defaults. The agent
// makes the value and the request never carries one.
type SecretGenerate struct {
	Manifest     *manifest.Manifest `json:"manifest,omitempty"`
	ComposeFiles map[string]string  `json:"compose_files,omitempty"`
	Name         string             `json:"name"`
	Bytes        *int               `json:"bytes,omitempty"`
	Format       string             `json:"format,omitempty"`
	Replace      bool               `json:"replace,omitempty"`
	DryRun       bool               `json:"dry_run,omitempty"`
}

// SecretGenerated is what a generate stored: the name, the length, the format and whether it replaced a stored secret. It
// never holds the value.
type SecretGenerated struct {
	Name     string `json:"name"`
	Bytes    int    `json:"bytes"`
	Format   string `json:"format"`
	Replaced bool   `json:"replaced"`
}

// SecretGeneratePlan is the dry run of a generate and the plan a held one shows. It names the secret and whether it exists, and
// never holds a value.
type SecretGeneratePlan struct {
	App    string `json:"app,omitempty"`
	Volume string `json:"volume,omitempty"`
	Action string `json:"action"`
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Bytes  int    `json:"bytes"`
	Format string `json:"format"`
}

// CheckSecretGenerate checks a generate request where it enters the agent, and fills in the defaults. The name must be a secret
// name other than the tunnel token, bytes must be from GenerateMinBytes to GenerateMaxBytes, and the format must be one of
// GenerateFormats. It returns the length and the format to generate.
func CheckSecretGenerate(req SecretGenerate) (bytes int, format string, err error) {
	if err := CheckSecretName(req.Name); err != nil {
		return 0, "", err
	}
	bytes = GenerateDefaultBytes
	if req.Bytes != nil {
		bytes = *req.Bytes
	}
	if bytes < GenerateMinBytes || bytes > GenerateMaxBytes {
		return 0, "", fmt.Errorf("bytes must be from %d to %d", GenerateMinBytes, GenerateMaxBytes)
	}
	format = req.Format
	if format == "" {
		format = GenerateDefaultFormat
	}
	if !slices.Contains(GenerateFormats, format) {
		return 0, "", errors.New("format must be hex, base64 or alnum")
	}
	return bytes, format, nil
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

// SecretsGenerateDeclared is the body of POST secrets/generate-declared.
type SecretsGenerateDeclared struct {
	Manifest     manifest.Manifest `json:"manifest"`
	ComposeFiles map[string]string `json:"compose_files,omitempty"`
	DryRun       bool              `json:"dry_run,omitempty"`
}
type DeclaredOutcome string

const (
	DeclaredGenerate DeclaredOutcome = "generate"
	DeclaredKeep     DeclaredOutcome = "keep"
	DeclaredRefuse   DeclaredOutcome = "refuse"
)

type DeclaredSecretOutcome struct {
	Name    string          `json:"name"`
	Outcome DeclaredOutcome `json:"outcome"`
	Bytes   int             `json:"bytes"`
	Format  string          `json:"format"`
	App     string          `json:"app,omitempty"`
	Volume  string          `json:"volume,omitempty"`
}
type SecretsDeclared struct {
	Action  string                  `json:"action"`
	Secrets []DeclaredSecretOutcome `json:"secrets"`
}
