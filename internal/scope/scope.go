// Package scope names a hosted environment: one project's environment on one agent.
package scope

import (
	"fmt"
	"regexp"
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
