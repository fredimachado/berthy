package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
)

// allVersions is the version REF given for each image app that has one.
type allVersions map[string]string

// parseAllVersions turns the --version values of a deploy without an app into one version per image app of m, or an error for
// the operator. A plain REF is for a manifest with exactly one image app; otherwise each is APP=REF, at most once per app.
func parseAllVersions(m *manifest.Manifest, values []string) (allVersions, error) {
	var plain, named []string
	for _, v := range values {
		if strings.Contains(v, "=") {
			named = append(named, v)
		} else {
			plain = append(plain, v)
		}
	}
	versions := allVersions{}
	switch {
	case len(plain) > 0 && len(named) > 0:
		return nil, errors.New("--version REF cannot be combined with --version APP=REF")
	case len(plain) > 1:
		return nil, errors.New("--version given more than once: use --version APP=REF")
	case len(plain) == 1:
		var images []string
		for name, app := range m.Apps {
			if app.Kind == "image" {
				images = append(images, name)
			}
		}
		if len(images) != 1 {
			return nil, errors.New("--version REF needs a manifest with exactly one image app: use --version APP=REF")
		}
		versions[images[0]] = plain[0]
	}
	for _, v := range named {
		app, ref, _ := strings.Cut(v, "=")
		switch {
		case m.Apps[app].Kind == "":
			return nil, fmt.Errorf("--version %s: unknown app %q", v, app)
		case m.Apps[app].Kind != "image":
			return nil, fmt.Errorf("--version %s: %s is a compose app and takes no version", v, app)
		case ref == "":
			return nil, fmt.Errorf("--version %s: a version is needed after =", v)
		}
		if _, again := versions[app]; again {
			return nil, fmt.Errorf("--version given twice for app %s", app)
		}
		versions[app] = ref
	}
	return versions, nil
}

// deployStep is one app of a deploy without an app: its request, ready to send, and what its header says about the version.
type deployStep struct {
	app    string
	req    deploy.Request
	header string
}

// deployAllEntry is what the --json document of a deploy without an app says about one app: how its deploy ended and what
// `deploy APP --json` printed, or that it was not run.
type deployAllEntry struct {
	App    string          `json:"app"`
	Exit   *int            `json:"exit,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	State  string          `json:"state,omitempty"`
}

// appliedRelease is the answer to the agent's read of the release an app runs now.
type appliedRelease struct {
	Digest string `json:"digest"`
	Tag    string `json:"tag"`
}

// runDeployAll deploys every app of the manifest, one after another in depends_on order, each as `berthy deploy APP` would.
// Everything is prepared and checked first, so a request that cannot be made leaves nothing deployed. It stops at the first
// app that does not succeed. With --dry-run it plans every app and reports the ones that failed.
func runDeployAll(f commandFlags, target *agentclient.Target, out, errout io.Writer) int {
	base, prefix, token := target.Base, target.Prefix, target.Token
	for _, needsApp := range []struct {
		given bool
		flag  string
	}{{f.adopt, "--adopt"}, {f.replace, "--replace-existing"}, {f.allowRecreate, "--allow-recreate"}, {f.id != "", "--request-id"}} {
		if needsApp.given {
			fmt.Fprintf(errout, "%s needs an app: berthy deploy APP %s\n", needsApp.flag, needsApp.flag)
			return 2
		}
	}
	m, err := manifest.Load(f.dir, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	versions, err := parseAllVersions(m, f.versions)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	var co gitcheck.Checkout
	steps := make([]deployStep, 0, len(m.Apps))
	for _, app := range m.DeployOrder() {
		req := deploy.Request{App: app, Commit: f.commit, DryRun: f.dry}
		step := deployStep{app: app}
		if ref, ok := versions[app]; ok {
			req.Version = ref
			step.header = " (" + terminalSafe(ref) + ")"
		} else if m.Apps[app].Kind == "image" {
			req.KeepVersion = true
		}
		if req.RequestID, err = newRequestID(); err != nil {
			return 1
		}
		checkout, e := agentclient.PrepareDeploy(f.dir, f.env, &req)
		if e != nil {
			fmt.Fprintf(errout, "app %s: %v (nothing was deployed)\n", app, e)
			return 1
		}
		// Every app reads the same base manifest and overlay, so the checkout is one: the files any of the apps reads.
		co.InWorkTree, co.Head = checkout.InWorkTree, checkout.Head
		co.Changed = append(co.Changed, checkout.Changed...)
		step.req = req
		steps = append(steps, step)
	}
	slices.Sort(co.Changed)
	co.Changed = slices.Compact(co.Changed)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := agentclient.NewHTTPClient()
	warnCheckout(errout, f.commit, co)
	if e := refuseLocalChanges(ctx, client, target, m.Protected, co); e != nil {
		fmt.Fprintf(errout, "%v (nothing was deployed)\n", e)
		return 1
	}
	if e := agentclient.CheckAgentVersion(ctx, client, target, *m); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if code := checkKeptVersions(ctx, client, base, prefix, token, steps, errout); code != 0 {
		return code
	}

	results := make([]deployAllEntry, len(steps))
	failed, stoppedAt := false, -1
	exit := 0
	for i, step := range steps {
		stdout := out
		var captured bytes.Buffer
		if f.json {
			stdout = &captured
		} else {
			fmt.Fprintf(out, "==> deploy %s%s\n", step.app, step.header)
		}
		code := sendCommand(ctx, client, base, prefix, token, "POST", "deploys", step.req, f, "deploy", stdout, errout)
		result, capturedErr := capturedDocument(captured.Bytes())
		if capturedErr != nil && code == 0 {
			fmt.Fprintf(errout, "app %s: %v\n", step.app, capturedErr)
			code = 1
		}
		results[i] = deployAllEntry{App: step.app, Exit: &code, Result: result}
		if code == 0 {
			continue
		}
		failed = true
		if f.dry {
			continue
		}
		exit, stoppedAt = code, i
		break
	}
	if f.dry && failed {
		exit = 1
	}

	if stoppedAt >= 0 {
		var notRun []string
		for _, step := range steps[stoppedAt+1:] {
			notRun = append(notRun, step.app)
		}
		line := "deploy stopped at " + steps[stoppedAt].app
		if len(notRun) > 0 {
			line += "; not run: " + strings.Join(notRun, ", ")
		}
		fmt.Fprintln(errout, line)
		for i := stoppedAt + 1; i < len(steps); i++ {
			results[i] = deployAllEntry{App: steps[i].app, State: "not_run"}
		}
	}
	if f.json {
		doc, _ := json.MarshalIndent(map[string][]deployAllEntry{"apps": results}, "", "  ")
		fmt.Fprintln(out, string(doc))
	} else if stoppedAt < 0 {
		for _, r := range results {
			state := "succeeded"
			switch {
			case *r.Exit != 0:
				state = "failed"
			case f.dry:
				state = "planned"
			}
			fmt.Fprintf(out, "%s  %s\n", r.App, state)
		}
	}
	return exit
}

// capturedDocument is the one JSON value a single deploy printed to its captured stdout. It is nil when nothing was printed,
// as when the agent could not be reached, and an error when what was printed is not exactly one JSON value.
func capturedDocument(b []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var doc json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.New("its output is not a JSON document")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("its output is more than one JSON document")
	}
	return doc, nil
}

// checkKeptVersions asks the agent, with a read that changes nothing, for the release of every image app that keeps its version.
// An app that was never deployed has none, and the agent would refuse it only when its turn came, after the apps before it
// were deployed, so it is refused here, before anything is sent. The apps' headers say which version each keeps.
func checkKeptVersions(ctx context.Context, client *http.Client, base *url.URL, prefix, token string, steps []deployStep, errout io.Writer) int {
	missing := false
	for i := range steps {
		step := &steps[i]
		if !step.req.KeepVersion {
			continue
		}
		u := *base
		u.Path = prefix + "applied/" + url.PathEscape(step.app)
		resp, err := agentclient.Send(ctx, client, "GET", u.String(), token, nil)
		if err != nil {
			fmt.Fprintln(errout, "agent connection failed (nothing was deployed)")
			return 1
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		resp.Body.Close()
		if err != nil || len(data) > 1<<20 {
			fmt.Fprintln(errout, "invalid agent response (nothing was deployed)")
			return 1
		}
		var answer struct {
			Code string `json:"code"`
			appliedRelease
		}
		switch {
		case resp.StatusCode == http.StatusNotFound && json.Unmarshal(data, &answer) == nil && answer.Code == "not_deployed":
			fmt.Fprintf(errout, "image app %s is not deployed yet: give --version %s=REF (nothing was deployed)\n", step.app, step.app)
			missing = true
		case resp.StatusCode != http.StatusOK || json.Unmarshal(data, &answer) != nil:
			fmt.Fprintf(errout, "%s (nothing was deployed)\n", agentclient.Message(resp.StatusCode, data))
			return 1
		default:
			// The agent refuses the deploy if the app runs another version by the time it deploys it, so what the header says is
			// what is kept.
			step.req.KeepDigest = answer.Digest
			step.header = " (keeps " + terminalSafe(keptVersionName(answer.appliedRelease)) + ")"
		}
	}
	if missing {
		return 2
	}
	return 0
}

// keptVersionName is the name a person knows a release by: its tag, or the start of its digest.
func keptVersionName(r appliedRelease) string {
	if r.Tag != "" {
		return r.Tag
	}
	return deploy.ShortDigest(r.Digest)
}
