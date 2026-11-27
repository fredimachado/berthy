package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/manifest"
)

// declaredCall is how asking the agent to generate the declared secrets ended.
type declaredCall int

const (
	declaredAnswered declaredCall = iota // res holds the agent's answer
	declaredHeld                         // the agent held the request for approval; that has been printed
	declaredFailed                       // the agent could not be asked, or refused; why has been printed
)

// runSecretGenerateDeclared answers secret generate with no name: it generates each declared secret the agent will accept.
// dir is the effective manifest directory (the same one the named form uses after alias and --manifest-dir selection).
func runSecretGenerateDeclared(ctx context.Context, f commandFlags, target *agentclient.Target, dir string, out, errout io.Writer) int {
	m, err := manifest.Load(dir, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if !manifestMatchesTarget(errout, dir, m.Project, target.Project) {
		return 2
	}
	client := agentclient.NewHTTPClient()
	res, call := generateDeclared(ctx, client, target, m, dir, f.dry, out, errout, f.json, false)
	switch call {
	case declaredHeld:
		return exitHeld
	case declaredFailed:
		return 1
	}
	if len(m.Secrets) == 0 && !f.json {
		fmt.Fprintln(out, "the manifest declares no generated secret")
		return 0
	}
	if f.json {
		b, err := json.Marshal(res)
		if err != nil {
			return 1
		}
		if code := printResponse(out, errout, b, true); code != 0 {
			return code
		}
	} else {
		printDeclared(out, res, f.dry)
	}
	if !f.dry {
		for _, s := range res.Secrets {
			if s.Outcome == api.DeclaredRefuse {
				return 1
			}
		}
	}
	return 0
}

// generateDeclared asks the agent to generate the declared secrets of m, or to plan it. It reads from dir the Compose file
// of each Compose app that reads a declared secret and sends them along. A manifest that declares nothing is answered
// here, with an empty list and no request.
func generateDeclared(ctx context.Context, client *http.Client, target *agentclient.Target, m *manifest.Manifest, dir string, dry bool, out, errout io.Writer, asJSON, guide bool) (res api.SecretsDeclared, call declaredCall) {
	empty := api.SecretsDeclared{Action: "secret generate declared", Secrets: []api.DeclaredSecretOutcome{}}
	if len(m.Secrets) == 0 {
		return empty, declaredAnswered
	}
	if e := agentclient.CheckAgentVersion(ctx, client, target, *m); e != nil {
		printRefusal(errout, terminalSafe(e.Error()), !asJSON)
		return res, declaredFailed
	}
	files := declaredComposeFiles(m, dir)
	body := api.SecretsGenerateDeclared{Manifest: *m, ComposeFiles: files, DryRun: dry}
	payload, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintln(errout, "invalid request")
		return res, declaredFailed
	}
	if len(payload) > api.MaxRequestBody {
		fmt.Fprintln(errout, "the Compose files of the apps that read declared secrets do not fit in one request")
		return res, declaredFailed
	}
	status, data, older, ok := askAgent(ctx, client, agentURL(target.Base, target.Prefix, "secrets/generate-declared"), target.Token, "POST", payload, errout)
	if !ok {
		return res, declaredFailed
	}
	if status == http.StatusAccepted {
		if guide {
			if reportGuideDeclaredHeld(data, out, target.Token) {
				return res, declaredHeld
			}
			fmt.Fprintln(errout, "invalid agent response")
			return res, declaredFailed
		}
		if code, held := reportHeldMarkers(data, out, errout, asJSON, nil, target.Token); held {
			if code != exitHeld {
				return res, declaredFailed
			}
			return res, declaredHeld
		}
		fmt.Fprintln(errout, "invalid agent response")
		return res, declaredFailed
	}
	if status == http.StatusMethodNotAllowed {
		fmt.Fprintln(errout, "the agent does not know this command: upgrade it with berthy setup")
		return res, declaredFailed
	}
	if status != http.StatusOK {
		printAgentRefusal(errout, safeSecretText(agentclient.Message(status, data), target.Token), !asJSON, older)
		return res, declaredFailed
	}
	if json.Unmarshal(data, &res) != nil || !validDeclaredAnswer(m, res) {
		fmt.Fprintln(errout, "invalid agent response")
		return res, declaredFailed
	}
	return res, declaredAnswered
}

// declaredComposeFiles reads each Compose app's file once when that app reads any declared secret. A missing or unreadable
// file is omitted, as the named form does, so the agent can still decide from its own state.
func declaredComposeFiles(m *manifest.Manifest, dir string) map[string]string {
	files := map[string]string{}
	for _, app := range sortedAppNames(m) {
		a := m.Apps[app]
		if a.Kind != "compose" {
			continue
		}
		reads := false
		for _, name := range a.Env.SecretNames() {
			if _, ok := m.Secrets[name]; ok {
				reads = true
				break
			}
		}
		if !reads {
			continue
		}
		file, err := agentclient.ReadComposeFile(dir, a.File)
		if err != nil {
			continue
		}
		files[app] = file
	}
	return files
}

// validDeclaredAnswer reports whether res is a usable secrets/generate-declared answer: known action, known outcomes, and
// protocol formats and lengths. A bad row is refused before any of it is printed.
func validDeclaredAnswer(m *manifest.Manifest, res api.SecretsDeclared) bool {
	if res.Action != "secret generate declared" || res.Secrets == nil || len(res.Secrets) != len(m.Secrets) {
		return false
	}
	seen := make(map[string]bool, len(res.Secrets))
	for _, s := range res.Secrets {
		if _, ok := m.Secrets[s.Name]; !ok || seen[s.Name] {
			return false
		}
		seen[s.Name] = true
		if s.Name == "" || s.Bytes < api.GenerateMinBytes || s.Bytes > api.GenerateMaxBytes {
			return false
		}
		if !slices.Contains(api.GenerateFormats, s.Format) {
			return false
		}
		switch s.Outcome {
		case api.DeclaredGenerate, api.DeclaredKeep:
		case api.DeclaredRefuse:
			if s.App == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// printDeclared prints one line per declared secret.
func printDeclared(out io.Writer, res api.SecretsDeclared, dry bool) {
	for _, s := range res.Secrets {
		name, size := terminalSafe(s.Name), generatedSize(s.Bytes, s.Format)
		switch s.Outcome {
		case api.DeclaredGenerate:
			if dry {
				fmt.Fprintf(out, "would generate %s (%s)\n", name, size)
			} else {
				fmt.Fprintf(out, "generated %s (%s)\n", name, size)
			}
		case api.DeclaredKeep:
			if dry {
				fmt.Fprintf(out, "would keep %s: it is already stored\n", name)
			} else {
				fmt.Fprintf(out, "kept %s: it is already stored\n", name)
			}
		case api.DeclaredRefuse:
			switch {
			case s.Volume != "" && dry:
				fmt.Fprintf(out, "would not generate %s: the volume %s of app %s exists and may hold the old value\n", name, terminalSafe(s.Volume), terminalSafe(s.App))
			case s.Volume != "":
				fmt.Fprintf(out, "not generated %s: the volume %s of app %s exists and may hold the old value; set the current one with berthy secret set %s\n", name, terminalSafe(s.Volume), terminalSafe(s.App), name)
			case dry:
				fmt.Fprintf(out, "would not generate %s: app %s was deployed here and may hold the old value\n", name, terminalSafe(s.App))
			default:
				fmt.Fprintf(out, "not generated %s: app %s was deployed here and may hold the old value; set the current one with berthy secret set %s\n", name, terminalSafe(s.App), name)
			}
		}
	}
}

// sortedAppNames returns the app names of m in sorted order.
func sortedAppNames(m *manifest.Manifest) []string {
	names := make([]string, 0, len(m.Apps))
	for name := range m.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
