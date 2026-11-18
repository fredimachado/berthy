package manifest

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Problem is one thing wrong with a manifest.
type Problem struct {
	Path    string `json:"path,omitempty"`   // where it is, as a dotted path into the manifest: "apps.api.roles.web.port"; "" when it is about the whole
	Message string `json:"message"`          // what is wrong, worded as Validate words it today
	File    string `json:"file,omitempty"`   // "berthy.yml" or "berthy.prod.yml" once Load has placed it; "" before
	Line    int    `json:"line,omitempty"`   // 1-based; 0 when unknown
	Column  int    `json:"column,omitempty"` // 1-based; 0 when unknown
}

// String is the problem as one line for a person: where it is in the files, where it is in the manifest, and what is wrong.
// Each part that is not known is left out. The path is left out too when the message already says where the problem is, by
// starting with the path or with a part of it ("alerts.telegram: chat_id must be ...").
func (p Problem) String() string {
	var b strings.Builder
	if p.File != "" {
		b.WriteString(p.File)
		if p.Line > 0 {
			fmt.Fprintf(&b, ":%d", p.Line)
			if p.Column > 0 {
				fmt.Fprintf(&b, ":%d", p.Column)
			}
		}
		b.WriteString(": ")
	}
	if p.Path != "" && !p.messageNamesPath() {
		b.WriteString(p.Path)
		b.WriteString(": ")
	}
	b.WriteString(p.Message)
	return b.String()
}

func (p Problem) messageNamesPath() bool {
	head, _, ok := strings.Cut(p.Message, ": ")
	return ok && (p.Path == head || strings.HasPrefix(p.Path, head+"."))
}

// Problems is every problem found. Its Error is the first problem's message, as Validate's error always was, so callers that
// show one line keep showing the same line.
type Problems []Problem

func (p Problems) Error() string {
	if len(p) == 0 {
		return "no problems"
	}
	return p[0].Message
}

// add records a problem at path.
func (p *Problems) add(path, format string, args ...any) {
	*p = append(*p, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

// addAt records a problem at path, with the line and column of the node it was found at.
func (p *Problems) addAt(n *yaml.Node, path, format string, args ...any) {
	p.add(path, format, args...)
	last := &(*p)[len(*p)-1]
	last.Line, last.Column = n.Line, n.Column
}

// join extends a dotted path by one key or index.
func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
