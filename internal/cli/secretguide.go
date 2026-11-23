package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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
	unmet, result := guideSecrets(ctx, agentclient.NewHTTPClient(), target, m, dir, f.env, read, out, errout)
	if result == guideHeld {
		return exitHeld
	}
	if result == guideFailed || len(unmet) != 0 {
		return 1
	}
	return 0
}

func guideSecrets(ctx context.Context, client *http.Client, target *agentclient.Target, m *manifest.Manifest, dir, env string, read func() ([]byte, error), out, errout io.Writer) ([]unmetNeed, guideResult) {
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
				if reportGuideHeld(data, value, n.Name, n.Registry, out) {
					return remaining, guideHeld
				}
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			if status != http.StatusOK {
				fmt.Fprintln(errout, safeSecretText(agentclient.Message(status, data), value))
				return remaining, guideFailed
			}
			var ack struct {
				Ok            bool            `json:"ok"`
				Registry      json.RawMessage `json:"registry"`
				Services      []secretService `json:"services"`
				ServicesError string          `json:"services_error"`
			}
			var registry string
			if json.Unmarshal(data, &ack) != nil || !ack.Ok || (len(ack.Registry) != 0 && (string(ack.Registry) == "null" || json.Unmarshal(ack.Registry, &registry) != nil)) || registry != n.Registry {
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			reply := secretSetReply{Ok: ack.Ok, Registry: registry, Services: ack.Services, ServicesError: ack.ServicesError}
			if n.Registry != "" {
				reply.Registry = safeSecretText(reply.Registry, value)
				reply.ServicesError = safeSecretText(reply.ServicesError, value)
				for i := range reply.Services {
					reply.Services[i].Name = safeSecretText(reply.Services[i].Name, value)
					reply.Services[i].Error = safeSecretText(reply.Services[i].Error, value)
				}
				if printRegistrySet(out, reply) {
					fmt.Fprintln(out, "Run berthy secret set NAME --registry HOST with the same targeting flags to retry the services that were not updated.")
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

// decodeGuideHeld validates the original envelope before any display transformation.
func decodeGuideHeld(data []byte) (heldApproval, bool) {
	var h heldApproval
	if json.Unmarshal(data, &h) != nil || h.ID == "" || h.Expires.IsZero() {
		return h, false
	}
	plan := strings.TrimSpace(string(h.Plan))
	return h, len(plan) > 0 && plan[0] == '{'
}

func printGuideHeldHeading(out io.Writer, h heldApproval, value string) {
	fmt.Fprintf(out, "held for approval: %s (expires %s)\n", safeSecretText(h.ID, value), safeSecretText(h.Expires.UTC().Format(time.RFC3339), value))
}

func printGuideHeld(out io.Writer, h heldApproval, plan []byte, value string) {
	printGuideHeldHeading(out, h, value)
	printPlan(out, plan)
}

func reportGuideDeclaredHeld(data []byte, out io.Writer) bool {
	h, ok := decodeGuideHeld(data)
	if !ok {
		return false
	}
	var plan any
	dec := json.NewDecoder(strings.NewReader(string(h.Plan)))
	dec.UseNumber()
	if dec.Decode(&plan) != nil {
		return false
	}
	encoded, err := json.Marshal(safeJSONValue(plan, ""))
	if err != nil {
		return false
	}
	printGuideHeld(out, h, encoded, "")
	return true
}

// reportGuideHeld validates the store plan against the request before displaying it.
func reportGuideHeld(data []byte, value, name, registry string, out io.Writer) bool {
	h, ok := decodeGuideHeld(data)
	if !ok {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(h.Plan, &fields) != nil || len(fields) == 0 {
		return false
	}
	const marker = "[redacted]"
	redacted := false
	for key, raw := range fields {
		if strings.Contains(key, marker) {
			redacted = true
			break
		}
		if strings.EqualFold(key, "action") || strings.EqualFold(key, "name") || strings.EqualFold(key, "registry") {
			var field string
			if json.Unmarshal(raw, &field) == nil && strings.Contains(field, marker) {
				redacted = true
				break
			}
		}
	}
	if redacted {
		printGuideHeldHeading(out, h, value)
		fmt.Fprintln(out, "plan details were redacted")
		return true
	}
	var plan struct {
		Action        *string         `json:"action"`
		Name          *string         `json:"name"`
		Registry      *string         `json:"registry"`
		Exists        *bool           `json:"exists"`
		Services      json.RawMessage `json:"services"`
		ServicesError json.RawMessage `json:"services_error"`
	}
	if json.Unmarshal(h.Plan, &plan) != nil || plan.Action == nil || *plan.Action != "secret set" || plan.Name == nil || *plan.Name != name || plan.Registry == nil || *plan.Registry != registry || plan.Exists == nil {
		return false
	}
	safe := map[string]any{
		"action":   safeSecretText(*plan.Action, value),
		"name":     safeSecretText(*plan.Name, value),
		"registry": safeSecretText(*plan.Registry, value),
		"exists":   *plan.Exists,
	}
	if len(plan.ServicesError) != 0 {
		var message string
		if string(plan.ServicesError) == "null" || json.Unmarshal(plan.ServicesError, &message) != nil {
			return false
		}
		safe["services_error"] = safeSecretText(message, value)
	}
	if len(plan.Services) != 0 {
		var services []string
		if json.Unmarshal(plan.Services, &services) != nil || services == nil {
			return false
		}
		for i := range services {
			services[i] = safeSecretText(services[i], value)
		}
		safe["services"] = services
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return false
	}
	printGuideHeld(out, h, encoded, value)
	return true
}
