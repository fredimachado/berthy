package manifest

import (
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// overlayMerge applies an overlay to a copy of the base tree. A map is merged key by key. A null removes its key, and any other
// value replaces the base's, a list included. What the overlay does that a person should know is kept as warnings: a null that
// removes nothing, and a list that replaces the base list and leaves out some of its entries. The project is the base's alone:
// an environment's project is fixed, and a reader that wants it reads the base file only (ProjectName).
type overlayMerge struct {
	baseName    string // the base file's name, for the messages
	overlayName string // the overlay's file name, where its problems are placed
	warnings    Problems
	problems    Problems
}

// merge applies overlay to base, which are the mappings at path in the merged tree and in the overlay.
func (o *overlayMerge) merge(base, overlay *yaml.Node, path string) {
	for i := 0; i+1 < len(overlay.Content); i += 2 {
		k, v := overlay.Content[i], overlay.Content[i+1]
		at := join(path, k.Value)
		j := keyIndex(base, k.Value)
		switch {
		case at == "project":
			o.problems = append(o.problems, Problem{
				Message: "project is set in " + o.baseName + " only: an environment cannot have another project",
				File:    o.overlayName, Line: k.Line, Column: k.Column,
			})
		case isNull(v) && j >= 0:
			base.Content = slices.Delete(base.Content, j, j+2)
		case isNull(v):
			o.nothingToRemove(at)
		case j < 0:
			base.Content = append(base.Content, k, o.inserted(v, at))
		case base.Content[j+1].Kind == yaml.MappingNode && v.Kind == yaml.MappingNode:
			o.merge(base.Content[j+1], v, at)
		default:
			if base.Content[j+1].Kind == yaml.SequenceNode && v.Kind == yaml.SequenceNode && !argvKeys[k.Value] {
				o.shrink(base.Content[j+1], v, at)
			}
			base.Content[j+1] = o.inserted(v, at)
		}
	}
}

// inserted is the copy of an overlay value that goes into the merged tree. Nothing under a value that is new to the merged tree
// is there to remove, so a null in it is dropped with a warning. The overlay's own tree keeps its nulls, where a problem is placed.
func (o *overlayMerge) inserted(v *yaml.Node, path string) *yaml.Node {
	c := cloneNode(v)
	o.dropNulls(c, path)
	return c
}

// dropNulls removes the null values of the mappings in n, which is at path, and of the mappings under it.
func (o *overlayMerge) dropNulls(n *yaml.Node, path string) {
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); {
		k, v := n.Content[i], n.Content[i+1]
		at := join(path, k.Value)
		if isNull(v) {
			o.nothingToRemove(at)
			n.Content = slices.Delete(n.Content, i, i+2)
			continue
		}
		o.dropNulls(v, at)
		i += 2
	}
}

// argvKeys are the keys of the lists that are command lines: a role's command and entrypoint, a job's command, a health check's
// test, and a migration's version, apply and revert. Position is what they mean, so an overlay that rewrites one replaces all of
// it on purpose, and the note about dropped entries does not apply. A hook's run is not here: it sits in a list item, which the
// merge never compares, so the hook list that holds it is the list the note covers.
var argvKeys = map[string]bool{
	"command": true, "entrypoint": true, "test": true, "version": true, "apply": true, "revert": true,
}

// shrink warns about each item of the base list that the overlay list, which replaces it, leaves out.
func (o *overlayMerge) shrink(base, overlay *yaml.Node, path string) {
	kept := make(map[string]bool, len(overlay.Content))
	for _, item := range overlay.Content {
		kept[canonical(item)] = true
	}
	var dropped []string
	for _, item := range base.Content {
		if !kept[canonical(item)] {
			dropped = append(dropped, label(item))
		}
	}
	if len(dropped) > 0 {
		o.warnings.add(path, "replaces the base list and drops %s", strings.Join(dropped, ", "))
	}
}

func (o *overlayMerge) nothingToRemove(path string) {
	o.warnings.add(path, "null removes nothing; %s does not set it", o.baseName)
}

// isNull is a value that an overlay writes as null: parseYAML accepts no other null in an overlay.
func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

// keyIndex is the index of the key in the content of a mapping, or -1 when the mapping has no such key.
func keyIndex(n *yaml.Node, key string) int {
	for j := 0; j+1 < len(n.Content); j += 2 {
		if n.Content[j].Value == key {
			return j
		}
	}
	return -1
}

// canonical is an item of a list as one string, the same for equal items: a scalar's value, or a mapping's keys and values
// sorted by key.
func canonical(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		pairs := make([]string, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, strconv.Quote(n.Content[i].Value)+":"+canonical(n.Content[i+1]))
		}
		slices.Sort(pairs)
		return "{" + strings.Join(pairs, ",") + "}"
	case yaml.SequenceNode:
		items := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			items = append(items, canonical(c))
		}
		return "[" + strings.Join(items, ",") + "]"
	default:
		return strconv.Quote(n.Value)
	}
}

// label names a dropped item in a warning: a scalar's value, the variable of a one-key mapping whose value is a name (an
// env.secret entry written VARIABLE: SECRET), or else the item's canonical form.
func label(n *yaml.Node) string {
	switch {
	case n.Kind == yaml.ScalarNode:
		return n.Value
	case n.Kind == yaml.MappingNode && len(n.Content) == 2 && n.Content[1].Kind == yaml.ScalarNode:
		return n.Content[0].Value
	}
	return canonical(n)
}
