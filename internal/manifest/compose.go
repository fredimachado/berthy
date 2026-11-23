package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// MaxComposeFile is the largest Compose file Berthy stores or sends.
const MaxComposeFile = 512 << 10

// ErrComposeFile reports a Compose file that is empty, too large, not UTF-8, or contains NUL.
var ErrComposeFile = errors.New("compose file must be non-empty UTF-8 text of at most 512 KiB without NUL")

// CheckComposeFile reports whether b can be stored and sent as a Compose file.
func CheckComposeFile(b []byte) error {
	if len(b) == 0 || len(b) > MaxComposeFile || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return ErrComposeFile
	}
	return nil
}

// CheckComposeDeploy reports whether app exists and is a Compose app.
func (m *Manifest) CheckComposeDeploy(app string) error {
	a, ok := m.Apps[app]
	if !ok || a.Kind != "compose" {
		return fmt.Errorf("app %q is not a compose app", app)
	}
	return nil
}

// ComposeFixedVolumes finds volume names fixed by the file without interpolating values.
func ComposeFixedVolumes(file []byte) ([]string, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(file))
	if err := dec.Decode(&doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("invalid compose file")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("multiple compose documents")
	}
	root := doc.Content[0]
	var volumes *yaml.Node
	seen := map[string]bool{}
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] || key.Value == "include" {
			return nil, errors.New("unresolved compose file")
		}
		seen[key.Value] = true
		if key.Value == "volumes" {
			volumes = value
		}
	}
	if volumes == nil || volumes.Tag == "!!null" {
		return nil, nil
	}
	if volumes.Kind != yaml.MappingNode {
		return nil, errors.New("invalid compose volumes")
	}
	names := map[string]bool{}
	seen = map[string]bool{}
	for i := 0; i < len(volumes.Content); i += 2 {
		key, entry := volumes.Content[i], volumes.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return nil, errors.New("invalid compose volume")
		}
		seen[key.Value] = true
		if entry.Tag == "!!null" {
			continue
		}
		if entry.Kind != yaml.MappingNode {
			return nil, errors.New("invalid compose volume")
		}
		name, external := "", false
		fields := map[string]bool{}
		for j := 0; j < len(entry.Content); j += 2 {
			k, v := entry.Content[j], entry.Content[j+1]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || fields[k.Value] {
				return nil, errors.New("invalid compose volume")
			}
			fields[k.Value] = true
			switch k.Value {
			case "name":
				if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Value == "" || strings.Contains(v.Value, "$") {
					return nil, errors.New("unresolved compose volume name")
				}
				name = v.Value
			case "external":
				if v.Tag != "!!bool" || v.Decode(&external) != nil {
					return nil, errors.New("invalid compose volume external")
				}
			}
		}
		if external && name == "" {
			name = key.Value
		}
		if strings.Contains(name, "$") {
			return nil, errors.New("unresolved compose volume name")
		}
		if name != "" {
			names[name] = true
		}
	}
	var out []string
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
