// Package scope names a hosted environment: one project's environment on one agent.
package scope

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	LabelProject     = "berthy.project"
	LabelEnvironment = "berthy.environment"
)

// ID is the identity of a hosted environment.
type ID struct{ Project, Environment string }

var (
	projectName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	environmentName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// ValidProject reports whether s is a project name: a letter or digit, then letters, digits, '_' and '-', at most 63
// characters.
func ValidProject(s string) bool { return projectName.MatchString(s) }

// ValidEnvironment reports whether s is an environment name: a letter or digit, then letters, digits and '-', at most 63
// characters.
func ValidEnvironment(s string) bool { return environmentName.MatchString(s) }

// MaxIDLength is the longest <project>-<environment>. A role's Swarm service is named <project>-<environment>-<app>-<role> and
// Docker allows 63 characters for it, so a longer pair could never deploy anything. It also keeps every name Berthy gives a
// container of its own, which carries the slug, within Docker's 128.
const MaxIDLength = 59

// CheckLength refuses a hosted environment whose <project>-<environment> is longer than MaxIDLength.
func (id ID) CheckLength() error {
	if len(id.Project)+1+len(id.Environment) > MaxIDLength {
		return fmt.Errorf("<project>-<environment> is longer than %d characters; shorten the project or environment", MaxIDLength)
	}
	return nil
}

// String is project/environment.
func (id ID) String() string { return id.Project + "/" + id.Environment }

// Parse is the inverse of String. Both parts must be valid.
func Parse(s string) (ID, error) {
	project, environment, found := strings.Cut(s, "/")
	if !found || strings.Contains(environment, "/") || !ValidProject(project) || !ValidEnvironment(environment) {
		return ID{}, fmt.Errorf("invalid hosted environment %q", s)
	}
	return ID{Project: project, Environment: environment}, nil
}

// Slug is project.environment, for the names of Berthy's own containers. Neither part may contain a dot.
func (id ID) Slug() string { return id.Project + "." + id.Environment }

const tokenHexDigits = "0123456789abcdef"
const tokenHexLength = 64

func (id ID) tokenPrefix() string { return "bty." + id.Slug() + "." }

// TokenExpression matches an issued bearer token for this environment without anchoring it to the input.
func (id ID) TokenExpression() string {
	return regexp.QuoteMeta(id.tokenPrefix()) + "[" + tokenHexDigits + "]{" + strconv.Itoa(tokenHexLength) + "}"
}

// TokenSuffixOverlap returns the longest proper suffix of a possible token at the start of data. A truncated stream may
// start inside either the scope prefix or the hex payload.
func (id ID) TokenSuffixOverlap(data []byte) int {
	prefix := id.tokenPrefix()
	size := len(prefix) + tokenHexLength
	for cut := max(1, size-len(data)); cut < size; cut++ {
		fixed := prefix[min(cut, len(prefix)):]
		if !bytes.HasPrefix(data, []byte(fixed)) {
			continue
		}
		n := size - cut
		if bytes.IndexFunc(data[len(fixed):n], func(r rune) bool { return !strings.ContainsRune(tokenHexDigits, r) }) < 0 {
			return n
		}
	}
	return 0
}

// APIPath is the hosted environment's API prefix, with the names as they are.
func (id ID) APIPath() string {
	return "/v1/projects/" + id.Project + "/environments/" + id.Environment + "/"
}

// Labels returns both ownership labels. Both keys are always present.
func (id ID) Labels() map[string]string {
	return map[string]string{LabelProject: id.Project, LabelEnvironment: id.Environment}
}

// Owns reports whether labels name this hosted environment. An absent label is read as "".
func (id ID) Owns(labels map[string]string) bool {
	return labels[LabelProject] == id.Project && labels[LabelEnvironment] == id.Environment
}
