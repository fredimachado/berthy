package manifest

import (
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// source is a manifest file Load read: its name, and its tree as it was read, before the overlay was merged into the base.
type source struct {
	name string
	root *yaml.Node
}

// locator says where in the files a path of the merged manifest is written. The overlay wins where it sets a key, as it does
// in the merge.
type locator struct {
	base    source
	overlay *source // nil when the environment has no overlay
}

// hit is the best place found for a path: the file and the node, the key of a mapping or the item of a list, and how much of
// the path it consumed. A deeper hit is a better one.
type hit struct {
	consumed int
	file     string
	at       *yaml.Node
}

// place sets File, Line and Column of every problem that is not placed yet. A path that is not written, such as a required
// key that was left out, is placed at the nearest key that is; a path of which nothing is written is placed at the base file
// without a line.
func (l locator) place(ps Problems) {
	for i := range ps {
		if ps[i].File != "" {
			continue
		}
		var overlay *yaml.Node
		if l.overlay != nil {
			overlay = l.overlay.root
		}
		h := l.descend(l.base.root, overlay, ps[i].Path)
		ps[i].File = l.base.name
		if h.at != nil {
			ps[i].File, ps[i].Line, ps[i].Column = h.file, h.at.Line, h.at.Column
		}
	}
}

// descend follows rest from the pair of nodes that the base and the overlay have at the same place. A key may contain dots
// (a registry is named by its host), so every way of cutting the next key off rest is tried and the one that goes deepest
// wins.
func (l locator) descend(base, overlay *yaml.Node, rest string) hit {
	var best hit
	if rest == "" {
		return best
	}
	for i := 0; i <= len(rest); i++ {
		if i < len(rest) && rest[i] != '.' {
			continue
		}
		key, tail := rest[:i], ""
		if i < len(rest) {
			tail = rest[i+1:]
		}
		bk, bv := child(base, key)
		ovk, ov := child(overlay, key)
		var h hit
		var here hit
		switch {
		case bv == nil && ov == nil:
			continue
		case bv != nil && ov != nil && bv.Kind == yaml.MappingNode && ov.Kind == yaml.MappingNode:
			h = l.descend(bv, ov, tail)
			here = hit{file: l.overlay.name, at: ovk}
		case ov != nil:
			h = l.descend(nil, ov, tail)
			here = hit{file: l.overlay.name, at: ovk}
		default:
			h = l.descend(bv, nil, tail)
			here = hit{file: l.base.name, at: bk}
		}
		if h.at == nil {
			h.file, h.at = here.file, here.at
		}
		h.consumed += i + 1
		if h.consumed > best.consumed {
			best = h
		}
	}
	return best
}

// child returns the key node and the value node of key in a mapping, or the item node twice for an index in a list.
func child(n *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if n == nil {
		return nil, nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i], n.Content[i+1]
			}
		}
	case yaml.SequenceNode:
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < len(n.Content) && !strings.HasPrefix(key, "+") {
			return n.Content[i], n.Content[i]
		}
	}
	return nil, nil
}

// cloneNode copies a tree, so merge can change the copy and the positions of the original stay as they were read.
func cloneNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, x := range n.Content {
		c.Content[i] = cloneNode(x)
	}
	return &c
}
