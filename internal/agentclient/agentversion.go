package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"berthy/internal/manifest"
	"berthy/internal/version"
)

// Protection describes the agent's effective policy or the configured policy of a legacy agent.
type Protection uint8

const (
	ProtectionUnknown Protection = iota
	ProtectionUnprotected
	ProtectionProtected
	ProtectionLegacyUnprotected
	ProtectionLegacyProtected
)

// AgentProtection reads a scoped, unambiguous protection capability from the agent.
func AgentProtection(ctx context.Context, c *http.Client, t *Target) (Protection, error) {
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	data, status, err := Do(ctx, &client, t.Base, t.Prefix, t.Token, "GET", "agent", "", nil)
	if err != nil {
		return ProtectionUnknown, errors.New("agent protection lookup failed")
	}
	if status != http.StatusOK {
		return ProtectionUnknown, fmt.Errorf("agent returned HTTP %d", status)
	}
	if !utf8.Valid(data) {
		return ProtectionUnknown, ErrInvalidResponse
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ProtectionUnknown, ErrInvalidResponse
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return ProtectionUnknown, ErrInvalidResponse
		}
		key, ok := tok.(string)
		if !ok {
			return ProtectionUnknown, ErrInvalidResponse
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return ProtectionUnknown, ErrInvalidResponse
		}
		for _, name := range []string{"project", "environment", "protected", "effective_protected"} {
			if strings.EqualFold(key, name) {
				if key != name || fields[name] != nil {
					return ProtectionUnknown, ErrInvalidResponse
				}
				fields[name] = raw
			}
		}
	}
	if tok, err = dec.Token(); err != nil || tok != json.Delim('}') || dec.Decode(new(json.RawMessage)) != io.EOF {
		return ProtectionUnknown, ErrInvalidResponse
	}
	var project, environment string
	if string(fields["project"]) == "null" || string(fields["environment"]) == "null" || json.Unmarshal(fields["project"], &project) != nil || json.Unmarshal(fields["environment"], &environment) != nil || project != t.Project || environment != t.Env {
		return ProtectionUnknown, ErrInvalidResponse
	}
	configured := fields["protected"]
	if string(configured) != "true" && string(configured) != "false" {
		return ProtectionUnknown, ErrInvalidResponse
	}
	effective, present := fields["effective_protected"]
	if !present {
		if string(configured) == "true" {
			return ProtectionLegacyProtected, nil
		}
		return ProtectionLegacyUnprotected, nil
	}
	if string(effective) == "true" {
		return ProtectionProtected, nil
	}
	if string(effective) == "false" && string(configured) == "false" {
		return ProtectionUnprotected, nil
	}
	return ProtectionUnknown, ErrInvalidResponse
}

// AgentVersion asks the agent of target for its version. It returns "" with no error when the agent answers without one.
func AgentVersion(ctx context.Context, c *http.Client, t *Target) (string, error) {
	data, status, err := Do(ctx, c, t.Base, t.Prefix, t.Token, "GET", "agent", "", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("agent returned HTTP %d", status)
	}
	var answer struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &answer) != nil {
		return "", ErrInvalidResponse
	}
	return answer.Version, nil
}

// CheckAgentVersion refuses, before anything is sent, a manifest whose min_version is above the agent's release: "this manifest
// needs Berthy 0.9.0 or newer; the agent of prod runs v0.8.2: upgrade it with berthy setup". It asks the agent only when the
// manifest has a min_version and this CLI is a release build. A failed lookup or a development agent passes, so the agent's own
// check decides.
func CheckAgentVersion(ctx context.Context, c *http.Client, t *Target, m manifest.Manifest) error {
	if m.MinVersion == "" || !version.Release() {
		return nil
	}
	agent, err := AgentVersion(ctx, c, t)
	if err != nil {
		return nil
	}
	var old *manifest.TooOld
	if errors.As(m.CheckVersion(agent), &old) {
		return fmt.Errorf("this manifest needs Berthy %s or newer; the agent of %s runs %s: upgrade it with berthy setup", old.Need, t.Env, old.Have)
	}
	return nil
}

// OlderAgentHint is the sentence to print after an agent refuses a request as invalid, when the agent runs an older release
// than this CLI, and "" otherwise. Both must be release builds, and a failed lookup gives "".
func OlderAgentHint(ctx context.Context, c *http.Client, t *Target) string {
	if !version.Release() {
		return ""
	}
	agent, err := AgentVersion(ctx, c, t)
	if err != nil {
		return ""
	}
	if cmp, ok := version.Compare(agent, version.Version); !ok || cmp >= 0 {
		return ""
	}
	return OlderAgentMessage
}

// AgentProtected reports whether the agent of target is configured for a protected environment. A failed lookup reports false:
// the agent refuses a change on a protected environment by its own rule whatever the CLI knows.
func AgentProtected(ctx context.Context, c *http.Client, t *Target) bool {
	data, status, err := Do(ctx, c, t.Base, t.Prefix, t.Token, "GET", "agent", "", nil)
	if err != nil || status != http.StatusOK {
		return false
	}
	var answer struct {
		Protected bool `json:"protected"`
	}
	return json.Unmarshal(data, &answer) == nil && answer.Protected
}

// InvalidRequest reports whether an agent answer refuses a request as invalid: the agent could not decode the request's body,
// which it does for a field it does not know.
func InvalidRequest(status int, body []byte) bool {
	var reply struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	return status == http.StatusBadRequest && json.Unmarshal(body, &reply) == nil && reply.Code == "bad_request" && reply.Message == "invalid request"
}
