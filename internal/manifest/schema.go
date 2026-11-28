package manifest

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// JSONSchema is the manifest as a JSON Schema, draft-07, for editors that check a YAML file against one. It is built by
// reflection over the Manifest type, so its shape is the one the agent decodes, and from Keys, for what each key means.
// Every object that has known keys refuses any other, as the agent does. An overlay is a partial manifest, so with overlay
// no key is required. The output is indented, and stable.
func JSONSchema(overlay bool) ([]byte, error) {
	b := schemaBuilder{overlay: overlay}
	root, err := b.node(reflect.TypeOf(Manifest{}), "")
	if err != nil {
		return nil, err
	}
	root["$schema"] = "http://json-schema.org/draft-07/schema#"
	root["title"] = "Berthy manifest (berthy.yml)"
	root["description"] = "The desired configuration of one project: berthy.yml, merged with the overlay of the environment."
	if overlay {
		root["title"] = "Berthy manifest overlay (berthy.<env>.yml)"
		root["description"] = "The part of the manifest that an environment changes. It is merged over berthy.yml, so no key is required."
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type schemaBuilder struct{ overlay bool }

var secretVarType = reflect.TypeOf(SecretVar{})

// node is the schema of a value of type t at the manifest path path.
func (b schemaBuilder) node(t reflect.Type, path string) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == secretVarType {
		return secretEntry(), nil
	}
	switch t.Kind() {
	case reflect.Struct:
		return b.object(t, path)
	case reflect.Map:
		ph, ok := placeholder(path)
		if !ok {
			return nil, fmt.Errorf("Keys has no key under %s.<...>, which names the keys of that map", path)
		}
		elem, err := b.node(t.Elem(), path+"."+ph)
		if err != nil {
			return nil, err
		}
		decorate(elem, path+"."+ph)
		if b.overlay {
			elem = nullable(elem)
		}
		node := map[string]any{"type": "object", "additionalProperties": elem}
		if k, ok := keyAt(path); ok && k.NamePattern != "" {
			node["propertyNames"] = map[string]any{"pattern": k.NamePattern}
		}
		return node, nil
	case reflect.Slice:
		elem, err := b.node(t.Elem(), path+"[]")
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": elem}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Interface:
		// The only value of any type is a backup's retention.keep: a whole number of at least 1, or the word "all".
		if path == "backups.<name>.retention.keep" {
			return map[string]any{"oneOf": []any{
				map[string]any{"type": "integer", "minimum": 1},
				map[string]any{"const": "all"},
			}}, nil
		}
	}
	return nil, fmt.Errorf("the schema has no shape for %s, of type %s", path, t)
}

// secretEntry is an entry of env.secret: a secret's name, or an object of exactly one key, a variable, whose value is the
// name of the secret it takes.
func secretEntry() map[string]any {
	name := map[string]any{"type": "string", "pattern": envName.String()}
	return map[string]any{"oneOf": []any{
		name,
		map[string]any{
			"type":                 "object",
			"minProperties":        1,
			"maxProperties":        1,
			"propertyNames":        map[string]any{"pattern": envName.String()},
			"additionalProperties": name,
		},
	}}
}

// nullable is the schema of a value in an overlay: its own schema, or null, which removes the key the value is for.
func nullable(node map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{node, map[string]any{"type": "null"}}}
}

// object is the schema of a struct: the fields that have a json name, each described by its Key.
func (b schemaBuilder) object(t reflect.Type, path string) (map[string]any, error) {
	props := map[string]any{}
	var required []string
	byKind := map[string][]string{}
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		p := name
		if path != "" {
			p = path + "." + name
		}
		child, err := b.node(t.Field(i).Type, p)
		if err != nil {
			return nil, err
		}
		decorate(child, p)
		if b.overlay {
			child = nullable(child)
		}
		props[name] = child
		if k, ok := keyAt(p); ok && k.Required {
			if k.Applies == "" {
				required = append(required, name)
			} else {
				byKind[k.Applies] = append(byKind[k.Applies], name)
			}
		}
	}
	node := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if b.overlay {
		return node, nil
	}
	if t == reflect.TypeOf(Health{}) {
		props["test"].(map[string]any)["minItems"] = 1
		props["uptime"].(map[string]any)["minLength"] = 1
		node["oneOf"] = []any{
			map[string]any{"required": []string{"test"}, "not": map[string]any{"required": []string{"uptime"}}},
			map[string]any{"required": []string{"uptime"}, "maxProperties": 1},
		}
	}
	if len(required) > 0 {
		node["required"] = required
	}
	if len(byKind) > 0 {
		if _, ok := props["kind"]; !ok {
			return nil, fmt.Errorf("%s has keys that apply to a kind of app, and no kind", path)
		}
		var all []any
		for _, kind := range slices.Sorted(maps.Keys(byKind)) {
			all = append(all, map[string]any{
				"if":   map[string]any{"properties": map[string]any{"kind": map[string]any{"const": kind}}, "required": []string{"kind"}},
				"then": map[string]any{"required": byKind[kind]},
			})
		}
		node["allOf"] = all
	}
	return node, nil
}

// decorate adds what Keys says about the key at path to its schema: description, allowed values and default.
func decorate(node map[string]any, path string) {
	k, ok := keyAt(path)
	if !ok {
		return
	}
	text := plain(k.Description)
	switch k.Applies {
	case "image":
		text += " For image apps."
	case "compose":
		text += " For Compose apps."
	}
	node["description"] = text
	if len(k.Enum) > 0 {
		node["enum"] = k.Enum
	}
	if d, ok := k.literalDefault(); ok {
		node["default"] = d
	}
}

// literalDefault is the default as a JSON value, when it is one: a boolean, an integer or a duration that the key's type
// allows, or one of the allowed values. A default that describes (the image's, no limit, <project>-<app>) is not one.
func (k Key) literalDefault() (any, bool) {
	if k.Default == "" {
		return nil, false
	}
	switch k.Type {
	case "boolean":
		v, err := strconv.ParseBool(k.Default)
		return v, err == nil
	case "integer":
		v, err := strconv.Atoi(k.Default)
		return v, err == nil
	case "duration":
		_, err := time.ParseDuration(k.Default)
		return k.Default, err == nil
	}
	return k.Default, slices.Contains(k.Enum, k.Default)
}
