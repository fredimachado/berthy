package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"

	"github.com/charmbracelet/x/term"
	"github.com/distribution/reference"
)

// exactFields refuses ambiguous spellings and repeated fields before using a decision from the agent.
func exactFields(data []byte, names ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid agent response")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid agent response")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err = dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid agent response")
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return nil, errors.New("invalid agent response")
		}
		for _, name := range names {
			if strings.EqualFold(key, name) {
				if key != name || fields[name] != nil {
					return nil, errors.New("invalid agent response")
				}
				fields[name] = raw
			}
		}
	}
	tok, err = dec.Token()
	if err != nil || tok != json.Delim('}') || dec.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("invalid agent response")
	}
	return fields, nil
}

// pinnedImage requires a tagless, qualified SHA-256 reference in the selected repository.
func pinnedImage(m *manifest.Manifest, app, image string) bool {
	ref, err := m.ImageReference(app, image)
	if err != nil {
		return false
	}
	canonical, ok := ref.(reference.Canonical)
	if !ok || canonical.Digest().Algorithm() != "sha256" || ref.String() != image {
		return false
	}
	_, tagged := ref.(reference.NamedTagged)
	return !tagged
}

func imagePlan(data []byte, m *manifest.Manifest, req deploy.Request, image, tag string) error {
	fields, err := exactFields(data, "app", "image", "tag", "roles", "blockers", "compose")
	if err != nil {
		return err
	}
	var app, gotImage, gotTag string
	var roles []string
	if json.Unmarshal(fields["app"], &app) != nil || json.Unmarshal(fields["image"], &gotImage) != nil || json.Unmarshal(fields["tag"], &gotTag) != nil || json.Unmarshal(fields["roles"], &roles) != nil || roles == nil || app != req.App || gotTag != tag || !pinnedImage(m, req.App, gotImage) || (image != "" && gotImage != image) {
		return errors.New("invalid agent deploy plan")
	}
	if raw := fields["compose"]; raw != nil && string(raw) != "null" {
		return errors.New("invalid agent deploy plan")
	}
	if raw := fields["blockers"]; raw != nil {
		var blockers []string
		if json.Unmarshal(raw, &blockers) != nil || blockers == nil {
			return errors.New("invalid agent deploy plan")
		}
		if len(blockers) != 0 {
			return errors.New("deploy plan has blockers")
		}
	}
	return nil
}

func showImagePlan(out, errout io.Writer, data []byte, asJSON bool, heading string) error {
	var buf bytes.Buffer
	if !asJSON && heading != "" {
		fmt.Fprintln(&buf, terminalSafe(heading))
	}
	var code int
	if asJSON {
		code = printResponse(&buf, errout, data, true)
	} else {
		code = printDryRun(&buf, errout, data)
	}
	if code != 0 {
		return errors.New("invalid agent deploy plan")
	}
	n, err := out.Write(buf.Bytes())
	if err == nil && n != buf.Len() {
		err = io.ErrShortWrite
	}
	return err
}

func interactiveDeploy(f commandFlags, out io.Writer) bool {
	screen, ok := out.(*os.File)
	return !f.json && ok && screen != nil && os.Stdin != nil && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(screen.Fd())
}

func runSingleTagDeploy(ctx context.Context, client *http.Client, target *agentclient.Target, f commandFlags, m *manifest.Manifest, req deploy.Request, selected reference.Named, out, errout io.Writer) int {
	tagged, ok := selected.(reference.NamedTagged)
	if !ok {
		fmt.Fprintln(errout, "image tag required")
		return 1
	}
	progress := newProgressLine(errout, !f.json && stderrTerminal(errout))
	progress.Start("Checking checkout")
	co, err := agentclient.PrepareDeployFiles(ctx, f.dir, f.env, m, []*deploy.Request{&req})
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	warnCheckout(errout, f.commit, co)
	if m.Protected && len(co.Changed) != 0 {
		fmt.Fprintf(errout, "the manifest has local changes: commit them first: %s\n", terminalSafe(strings.Join(co.Changed, ", ")))
		return 1
	}
	if m.Protected && !f.dry && !interactiveDeploy(f, out) {
		fmt.Fprintln(errout, "deploy confirmation requires terminal input and preview output; run interactively or deploy an explicit digest")
		return 1
	}
	progress.Start("Checking agent policy")
	policy, err := agentclient.AgentProtection(ctx, client, target)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, target.Redact(err.Error()))
		return 1
	}
	if (policy == agentclient.ProtectionProtected || policy == agentclient.ProtectionLegacyProtected) && len(co.Changed) != 0 {
		fmt.Fprintf(errout, "the manifest has local changes: commit them first: %s\n", terminalSafe(strings.Join(co.Changed, ", ")))
		return 1
	}
	progress.Start("Checking agent version")
	err = agentclient.CheckAgentVersion(ctx, client, target, *m)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, target.Redact(err.Error()))
		return 1
	}
	protected := m.Protected || policy == agentclient.ProtectionProtected || policy == agentclient.ProtectionLegacyProtected
	if policy == agentclient.ProtectionLegacyProtected || (policy == agentclient.ProtectionLegacyUnprotected && protected) {
		fmt.Fprintln(errout, "protected tag deployment requires an agent upgrade or an explicit digest")
		return 1
	}
	if policy == agentclient.ProtectionLegacyUnprotected {
		guard := req
		guard.DryRun = true
		payload, _ := json.Marshal(guard)
		progress.Start("Checking legacy agent")
		data, status, e := agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "", payload)
		progress.Close()
		if e != nil || status != http.StatusOK {
			if e != nil {
				fmt.Fprintln(errout, "legacy agent check failed; upgrade the agent or deploy an explicit digest")
			} else {
				fmt.Fprintf(errout, "%s; upgrade the agent or deploy an explicit digest\n", target.Redact(agentclient.Message(status, data)))
			}
			return 1
		}
		if err := imagePlan(data, m, req, "", tagged.Tag()); err != nil {
			fmt.Fprintf(errout, "%s; upgrade the agent or deploy an explicit digest\n", err)
			return 1
		}
		if f.dry {
			if err := showImagePlan(out, errout, []byte(target.Redact(string(data))), f.json, ""); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
			return 0
		}
		return sendCommand(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", req, f, "deploy", out, errout)
	}
	if !protected {
		if _, err := m.CheckImageDeploy(req.App, req.Version, false); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return sendCommand(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", req, f, "deploy", out, errout)
	}
	if !f.dry {
		if !interactiveDeploy(f, out) {
			fmt.Fprintln(errout, "deploy confirmation requires terminal input and preview output; run interactively or deploy an explicit digest")
			return 1
		}
	}
	lookup, _ := json.Marshal(api.ImageLookup{Manifest: *m, App: req.App, Tag: tagged.Tag()})
	progress.Start("Resolving image tag")
	data, status, err := agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "images/resolve", "", lookup)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, "image tag lookup failed")
		return 1
	}
	if status != http.StatusOK {
		if status == http.StatusForbidden {
			fmt.Fprintln(errout, "image tag lookup requires a human administrator token")
		} else if status == http.StatusNotFound {
			fmt.Fprintln(errout, "image tag lookup is unavailable; upgrade the agent or deploy an explicit digest")
		} else {
			fmt.Fprintln(errout, target.Redact(agentclient.Message(status, data)))
		}
		return 1
	}
	fields, err := exactFields(data, "image", "tag")
	var image, tag string
	if err != nil || json.Unmarshal(fields["image"], &image) != nil || json.Unmarshal(fields["tag"], &tag) != nil || tag != tagged.Tag() || !pinnedImage(m, req.App, image) {
		fmt.Fprintln(errout, "invalid image tag lookup response")
		return 1
	}
	req.Version, req.VersionTag = image, tag
	if _, err := m.CheckImageDeploy(req.App, req.Version, true); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	preview := req
	preview.DryRun = true
	payload, _ := json.Marshal(preview)
	progress.Start("Planning deploy")
	data, status, err = agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "", payload)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, "deploy preview failed")
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintln(errout, target.Redact(agentclient.Message(status, data)))
		return 1
	}
	if err := imagePlan(data, m, req, image, tag); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if err := showImagePlan(out, errout, []byte(target.Redact(string(data))), f.json, "Deploy "+target.Project+"/"+target.Env+" app "+req.App); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if f.dry {
		return 0
	}
	yes, err := confirmDeploy(ctx, os.Stdin, out)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if !yes || ctx.Err() != nil {
		fmt.Fprintln(errout, "deploy cancelled")
		return 1
	}
	req.DryRun = false
	return sendCommand(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", req, f, "deploy", out, errout)
}
