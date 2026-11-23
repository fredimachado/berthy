package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/manifest"
)

type guideResult int

const (
	guideDone guideResult = iota
	guideHeld
	guideFailed
)

func runSecretSetMissing(f commandFlags, target *agentclient.Target, dir string, out, errout io.Writer) int {
	read, ok := hiddenInput()
	if !ok {
		fmt.Fprintln(errout, "secret set without a NAME asks at a terminal: pipe a value to berthy secret set NAME, or use berthy secret import")
		return 2
	}
	m, err := manifest.Load(dir, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if !manifestMatchesTarget(errout, dir, m.Project, target.Project) {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	unmet, result := guideSecretsWithFlags(ctx, agentclient.NewHTTPClient(), target, m, dir, f.env, f, read, out, errout)
	if result == guideHeld {
		return exitHeld
	}
	if result == guideFailed || len(unmet) != 0 {
		return 1
	}
	return 0
}

func guideSecrets(ctx context.Context, client *http.Client, target *agentclient.Target, m *manifest.Manifest, dir, env string, read func() ([]byte, error), out, errout io.Writer) ([]unmetNeed, guideResult) {
	return guideSecretsWithFlags(ctx, client, target, m, dir, env, commandFlags{}, read, out, errout)
}

func guideSecretsWithFlags(ctx context.Context, client *http.Client, target *agentclient.Target, m *manifest.Manifest, dir, env string, f commandFlags, read func() ([]byte, error), out, errout io.Writer) ([]unmetNeed, guideResult) {
	declared, call := generateDeclared(ctx, client, target, m, dir, false, out, errout, false, true)
	if call == declaredHeld {
		return nil, guideHeld
	}
	if call == declaredFailed {
		return nil, guideFailed
	}
	printDeclared(out, declared, false)
	refused := map[string]api.DeclaredSecretOutcome{}
	for _, s := range declared.Secrets {
		if s.Outcome == api.DeclaredRefuse {
			refused[s.Name] = s
		}
	}
	endpoint := agentURL(target.Base, target.Prefix, "secrets")
	held, ok := storedSecrets(ctx, client, endpoint, target.Token, errout)
	if !ok {
		return nil, guideFailed
	}
	needs := unmetSecrets(m, held)
	if len(needs) == 0 {
		fmt.Fprintln(out, "every secret the manifest needs is stored")
		return needs, guideDone
	}
	var remaining []unmetNeed
	for _, n := range needs {
		if ctx.Err() != nil {
			return remaining, guideFailed
		}
		name := terminalSafe(n.Name)
		fmt.Fprintf(errout, "%s is needed by %s\n", name, terminalSafe(strings.Join(n.NeededBy, ", ")))
		switch n.Kind {
		case unmetShared:
			fmt.Fprintf(errout, "%s is the password_secret of %s: give each registry its own secret name\n", name, safeRegistryList(n.Registries))
			remaining = append(remaining, n)
			continue
		case unmetMixed:
			fmt.Fprintf(errout, "%s is needed by %s: give them separate secret names\n", name, mixedReaders(n))
			remaining = append(remaining, n)
			continue
		case unmetNotPlain:
			fmt.Fprintf(errout, "%s is stored as the password of %s: remove it with berthy secret rm %s, then set it\n", name, terminalSafe(n.Stored), name)
			remaining = append(remaining, n)
			continue
		case unmetRegistry:
			if n.Stored == "" {
				fmt.Fprintf(errout, "%s is stored without the registry %s: enter it again and it is stored for %s\n", name, terminalSafe(n.Registry), terminalSafe(n.Registry))
			} else {
				fmt.Fprintf(errout, "%s is stored for the registry %s: enter the password for %s and it replaces it\n", name, terminalSafe(n.Stored), terminalSafe(n.Registry))
			}
		}
		if s, ok := refused[n.Name]; ok {
			if s.Volume == "" {
				fmt.Fprintf(errout, "app %s was deployed here: enter the value it runs with\n", terminalSafe(s.App))
			} else {
				fmt.Fprintf(errout, "the volume %s of app %s exists: enter the value it was created with\n", terminalSafe(s.Volume), terminalSafe(s.App))
			}
		}
		g := guideFor(m, env, n.Name, n.Registry)
		for i := range g.intro {
			g.intro[i] = terminalSafe(g.intro[i])
		}
		if check := g.check; check != nil {
			g.check = func(_ context.Context, value string) checkResult {
				checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				res := check(checkCtx, value)
				for i := range res.notes {
					res.notes[i] = terminalSafe(res.notes[i])
				}
				res.refusal = terminalSafe(res.refusal)
				return res
			}
		}
		for {
			if ctx.Err() != nil {
				return remaining, guideFailed
			}
			value, result := askSecret(g, fmt.Sprintf("Value of %s (hidden, Enter skips): ", name), read, errout)
			if ctx.Err() != nil {
				return remaining, guideFailed
			}
			if result == askFailed {
				return remaining, guideFailed
			}
			if result == askRefused {
				continue
			}
			if result == askEmpty {
				fmt.Fprintf(out, "skipped %s\n", name)
				remaining = append(remaining, n)
				break
			}
			payload, _ := json.Marshal(map[string]string{"name": n.Name, "value": value, "registry": n.Registry})
			status, data, ok := askAgent(ctx, client, endpoint, target.Token, http.MethodPost, payload, errout)
			if !ok {
				return remaining, guideFailed
			}
			if status == http.StatusAccepted {
				if reportGuideHeld(data, value, out) {
					return remaining, guideHeld
				}
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			if status != http.StatusOK {
				fmt.Fprintln(errout, safeSecretText(agentclient.Message(status, data), value))
				return remaining, guideFailed
			}
			var reply secretSetReply
			var fields map[string]json.RawMessage
			if json.Unmarshal(data, &reply) != nil || json.Unmarshal(data, &fields) != nil || !reply.Ok || reply.Registry != n.Registry || string(fields["registry"]) == "null" {
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			if n.Registry != "" {
				reply.Registry = safeSecretText(reply.Registry, value)
				reply.ServicesError = safeSecretText(reply.ServicesError, value)
				for i := range reply.Services {
					reply.Services[i].Name = safeSecretText(reply.Services[i].Name, value)
					reply.Services[i].Error = safeSecretText(reply.Services[i].Error, value)
				}
				if printRegistrySet(out, reply) {
					retry := "berthy secret set " + safeSecretText(n.Name, value) + " --registry " + strconv.Quote(safeSecretText(n.Registry, value)) + " -e " + safeSecretText(env, value) + " --project " + safeSecretText(target.Project, value)
					if f.config != "" {
						retry += " --config " + strconv.Quote(safeSecretText(f.config, value))
					}
					if f.dirSet {
						retry += " --manifest-dir " + strconv.Quote(safeSecretText(dir, value))
					}
					fmt.Fprintln(out, "Run "+retry+" to retry the services that were not updated.")
				}
			} else {
				fmt.Fprintf(out, "stored %s\n", name)
			}
			break
		}
	}
	return remaining, guideDone
}

// safeSecretText removes a submitted value before escaping untrusted display text.
func safeSecretText(text, value string) string {
	if value != "" {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return terminalSafe(text)
}

func safeJSONValue(v any, value string) any {
	switch x := v.(type) {
	case string:
		return safeSecretText(x, value)
	case []any:
		for i := range x {
			x[i] = safeJSONValue(x[i], value)
		}
		return x
	case map[string]any:
		safe := make(map[string]any, len(x))
		for k, v := range x {
			safe[safeSecretText(k, value)] = safeJSONValue(v, value)
		}
		return safe
	default:
		return x
	}
}

// safeHeldJSON scopes terminal escaping to the guide's declared-generation output.
func safeHeldJSON(data []byte) []byte {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return data
	}
	b, err := json.Marshal(safeJSONValue(v, ""))
	if err != nil {
		return data
	}
	return b
}

// reportGuideHeld validates and prints only the fields of a secret set plan.
func reportGuideHeld(data []byte, value string, out io.Writer) bool {
	var h heldApproval
	if json.Unmarshal(data, &h) != nil || h.ID == "" || h.Expires.IsZero() {
		return false
	}
	var plan map[string]json.RawMessage
	if json.Unmarshal(h.Plan, &plan) != nil || plan == nil {
		return false
	}
	safe := map[string]any{}
	for _, key := range []string{"action", "name", "registry", "services_error"} {
		if raw, ok := plan[key]; ok {
			var v string
			if json.Unmarshal(raw, &v) != nil || string(raw) == "null" {
				return false
			}
			safe[key] = safeSecretText(v, value)
		}
	}
	if raw, ok := plan["exists"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) != nil || string(raw) == "null" {
			return false
		}
		safe["exists"] = v
	}
	if raw, ok := plan["services"]; ok {
		var v []string
		if json.Unmarshal(raw, &v) != nil || v == nil {
			return false
		}
		for i := range v {
			v[i] = safeSecretText(v[i], value)
		}
		safe["services"] = v
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return false
	}
	fmt.Fprintf(out, "held for approval: %s (expires %s)\n", safeSecretText(h.ID, value), safeSecretText(h.Expires.UTC().Format(time.RFC3339), value))
	printPlan(out, encoded)
	return true
}
