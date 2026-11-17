package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"berthy/internal/manifest"
	"berthy/internal/version"
)

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
	return "the agent runs an older release than this CLI; upgrade it with berthy setup"
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
