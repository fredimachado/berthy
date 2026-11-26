package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
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
	display := declared
	display.Secrets = append([]api.DeclaredSecretOutcome(nil), declared.Secrets...)
	for i := range display.Secrets {
		display.Secrets[i].Name = redactSecretText(display.Secrets[i].Name, target.Token)
		display.Secrets[i].App = redactSecretText(display.Secrets[i].App, target.Token)
		display.Secrets[i].Volume = redactSecretText(display.Secrets[i].Volume, target.Token)
	}
	printDeclared(out, display, false)
	refused := map[string]api.DeclaredSecretOutcome{}
	for _, s := range declared.Secrets {
		if s.Outcome == api.DeclaredRefuse {
			refused[s.Name] = s
		}
	}
	endpoint := agentURL(target.Base, target.Prefix, "secrets")
	held, ok := storedSecrets(ctx, client, endpoint, target.Token, errout, true)
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
			fmt.Fprintf(errout, "%s is stored as the password of %s: remove it with berthy secret rm %s, then set it\n", name, safeSecretText(n.Stored, target.Token), name)
			remaining = append(remaining, n)
			continue
		case unmetRegistry:
			if n.Stored == "" {
				fmt.Fprintf(errout, "%s is stored without the registry %s: enter it again and it is stored for %s\n", name, terminalSafe(n.Registry), terminalSafe(n.Registry))
			} else {
				fmt.Fprintf(errout, "%s is stored for the registry %s: enter the password for %s and it replaces it\n", name, safeSecretText(n.Stored, target.Token), terminalSafe(n.Registry))
			}
		}
		if s, ok := refused[n.Name]; ok {
			if s.Volume == "" {
				fmt.Fprintf(errout, "app %s was deployed here: enter the value it runs with\n", safeSecretText(s.App, target.Token))
			} else {
				fmt.Fprintf(errout, "the volume %s of app %s exists: enter the value it was created with\n", safeSecretText(s.Volume, target.Token), safeSecretText(s.App, target.Token))
			}
		}
		g := guideFor(m, env, n.Name, n.Registry)
		for {
			if ctx.Err() != nil {
				return remaining, guideFailed
			}
			value, result := askSecret(ctx, g, fmt.Sprintf("Value of %s (hidden, Enter skips): ", name), read, errout, true, target.Token)
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
			progress := startWait(errout, true, "storing secret")
			var diagnostic bytes.Buffer
			status, data, ok := askAgent(ctx, client, endpoint, target.Token, http.MethodPost, payload, &diagnostic)
			progress.Close()
			_, _ = io.Copy(errout, &diagnostic)
			if !ok {
				return remaining, guideFailed
			}
			if status == http.StatusAccepted {
				if reportGuideHeld(data, value, n.Name, n.Registry, out, target.Token) {
					return remaining, guideHeld
				}
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			if status != http.StatusOK {
				printRefusal(errout, safeSecretText(agentclient.Message(status, data), value, wireSecret(value), target.Token), true)
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
				if printRegistrySet(out, reply, value, wireSecret(value), target.Token) {
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

// redactSecretText removes longer values first so overlapping secrets cannot reveal one another.
func redactSecretText(text string, values ...string) string {
	values = append([]string(nil), values...)
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var result strings.Builder
	var write func(string, int)
	write = func(part string, index int) {
		if index == len(values) || values[index] == "" {
			result.WriteString(part)
			return
		}
		value := values[index]
		for {
			i := strings.Index(part, value)
			if i < 0 {
				write(part, index+1)
				return
			}
			write(part[:i], index+1)
			result.WriteString("[redacted]")
			part = part[i+len(value):]
		}
	}
	write(text, 0)
	return result.String()
}

func safeSecretText(text string, values ...string) string {
	return terminalSafe(redactSecretText(text, values...))
}

func safeJSONValue(v any, values ...string) any {
	switch x := v.(type) {
	case string:
		return safeSecretText(x, values...)
	case []any:
		for i := range x {
			x[i] = safeJSONValue(x[i], values...)
		}
		return x
	case map[string]any:
		safe := make(map[string]any, len(x))
		for k, v := range x {
			safe[safeSecretText(k, values...)] = safeJSONValue(v, values...)
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

func printGuideHeldHeading(out io.Writer, h heldApproval, values ...string) {
	fmt.Fprintf(out, "%s: %s (expires %s)\n", styleFor(out, true).paint(toneWarning, "held for approval"), safeSecretText(h.ID, values...), safeSecretText(h.Expires.UTC().Format(time.RFC3339), values...))
}

func printGuideHeld(out io.Writer, h heldApproval, plan []byte, values ...string) {
	printGuideHeldHeading(out, h, values...)
	printPlan(out, plan)
}

func reportGuideDeclaredHeld(data []byte, out io.Writer, token string) bool {
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
	encoded, err := json.Marshal(safeJSONValue(plan, token))
	if err != nil {
		return false
	}
	printGuideHeld(out, h, encoded, token)
	return true
}

// reportGuideHeld validates the store plan against the request before displaying it.
func reportGuideHeld(data []byte, value, name, registry string, out io.Writer, token string) bool {
	values := []string{value, wireSecret(value), token}
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
		printGuideHeldHeading(out, h, values...)
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
		"action":   safeSecretText(*plan.Action, values...),
		"name":     safeSecretText(*plan.Name, values...),
		"registry": safeSecretText(*plan.Registry, values...),
		"exists":   *plan.Exists,
	}
	if len(plan.ServicesError) != 0 {
		var message string
		if string(plan.ServicesError) == "null" || json.Unmarshal(plan.ServicesError, &message) != nil {
			return false
		}
		safe["services_error"] = safeSecretText(message, values...)
	}
	if len(plan.Services) != 0 {
		var services []string
		if json.Unmarshal(plan.Services, &services) != nil || services == nil {
			return false
		}
		for i := range services {
			services[i] = safeSecretText(services[i], values...)
		}
		safe["services"] = services
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return false
	}
	printGuideHeld(out, h, encoded, values...)
	return true
}
