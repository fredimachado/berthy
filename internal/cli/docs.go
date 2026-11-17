package cli

import (
	"fmt"
	"io"
	"strings"

	"berthy/internal/manifest"
)

// runDocs prints the manifest reference, or one section of it. It is local: it reads the reference built into this binary,
// so it needs no agent, no manifest and no CLI configuration.
func runDocs(args []string, out, errout io.Writer) int {
	var section string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errout, "unknown flag %s\n", a)
			return 2
		case section != "":
			fmt.Fprintln(errout, "docs takes at most one section")
			return 2
		default:
			section = a
		}
	}
	text, err := manifest.ReferenceText(section)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	fmt.Fprint(out, text)
	return 0
}

// runConfigSchema prints the JSON Schema of the manifest, or with --overlay of an overlay. It is local, like docs, and takes
// no -e: the schema is the same for every environment.
func runConfigSchema(args []string, out, errout io.Writer) int {
	overlay := false
	for _, a := range args {
		switch {
		case a == "--overlay":
			overlay = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errout, "%s is not a flag of config schema\n", a)
			return 2
		default:
			fmt.Fprintln(errout, "config schema takes no argument")
			return 2
		}
	}
	schema, err := manifest.JSONSchema(overlay)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	_, _ = out.Write(schema)
	return 0
}
