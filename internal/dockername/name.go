// Package dockername validates identifiers used in Docker API paths.
package dockername

import "regexp"

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Valid reports whether name is a Docker identifier accepted by the engine.
func Valid(name string) bool { return identifier.MatchString(name) }
