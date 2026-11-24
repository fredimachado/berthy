package sshhost

import "regexp"

var pattern = regexp.MustCompile(`^([a-z_][a-z0-9_.-]*@)?[A-Za-z0-9][A-Za-z0-9.-]*$`)

// Valid reports whether s is an SSH destination of the form USER@HOST or HOST.
// A destination never starts with a dash, so SSH cannot read it as an option.
func Valid(s string) bool { return len(s) <= 320 && pattern.MatchString(s) }
