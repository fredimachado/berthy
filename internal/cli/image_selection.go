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
	"slices"
	"strings"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"

	"github.com/charmbracelet/x/term"
	"github.com/distribution/reference"
)

// loadDeployManifest lets checkout capture tests switch revisions after the file read.
var loadDeployManifest = manifest.Load

// bareSHA256 matches the bare digest grammar accepted by the manifest, not a registry host prefix.
func bareSHA256(value string) bool {
	return len(value) == len("sha256:")+64 && strings.HasPrefix(value, "sha256:") && strings.Trim(value[len("sha256:"):], "0123456789abcdef") == ""
}

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
	fields, err := exactFields(data, "app", "image", "tag", "roles", "blockers", "compose", "diff", "ingress", "changes", "remove", "destroy", "restore", "exec", "command", "schema_revert")
	if err != nil {
		return err
	}
	var app, gotImage, gotTag string
	var roles []string
	if raw := fields["tag"]; raw != nil {
		var present *string
		if json.Unmarshal(raw, &present) != nil || present == nil {
			return errors.New("invalid agent deploy plan")
		}
		gotTag = *present
	} else if tag != "" {
		return errors.New("invalid agent deploy plan")
	}
	if json.Unmarshal(fields["app"], &app) != nil || json.Unmarshal(fields["image"], &gotImage) != nil || json.Unmarshal(fields["roles"], &roles) != nil || roles == nil || app != req.App || gotTag != tag || !pinnedImage(m, req.App, gotImage) || (image != "" && gotImage != image) {
		return errors.New("invalid agent deploy plan")
	}
	expected, exists := m.Apps[req.App]
	if !exists || expected.Kind != "image" || len(roles) != len(expected.Roles) {
		return errors.New("invalid agent deploy plan")
	}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if role == "" || seen[role] {
			return errors.New("invalid agent deploy plan")
		}
		if _, exists := expected.Roles[role]; !exists {
			return errors.New("invalid agent deploy plan")
		}
		seen[role] = true
	}
	for _, name := range []string{"compose", "remove", "destroy", "restore", "exec", "command", "schema_revert"} {
		if raw := fields[name]; raw != nil && string(raw) != "null" {
			return errors.New("invalid agent deploy plan")
		}
	}
	if raw := fields["diff"]; raw != nil && (req.Adopt || !validImageDiff(raw)) {
		return errors.New("invalid agent deploy plan")
	}
	if raw := fields["changes"]; raw != nil && (!req.Adopt || !validStringList(raw)) {
		return errors.New("invalid agent deploy plan")
	}
	if raw := fields["ingress"]; raw != nil && !validDeployIngress(raw) {
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

// validStringList requires a non-null array of non-null strings.
func validStringList(data []byte) bool {
	var values []json.RawMessage
	if json.Unmarshal(data, &values) != nil || values == nil {
		return false
	}
	for _, raw := range values {
		var value *string
		if json.Unmarshal(raw, &value) != nil || value == nil {
			return false
		}
	}
	return true
}

// validDeployIngress validates the route plan produced by dryRunPublish, not ingress apply.
func validDeployIngress(data []byte) bool {
	fields, err := exactOnlyFields(data, "tunnel", "tunnel_id", "dns", "changes", "conflicts", "notes")
	if err != nil {
		return false
	}
	var tunnel *string
	var dns *bool
	if json.Unmarshal(fields["tunnel"], &tunnel) != nil || tunnel == nil || json.Unmarshal(fields["dns"], &dns) != nil || dns == nil {
		return false
	}
	if raw := fields["tunnel_id"]; raw != nil {
		var id *string
		if json.Unmarshal(raw, &id) != nil || id == nil {
			return false
		}
	}
	if raw := fields["notes"]; raw != nil && !validStringList(raw) {
		return false
	}
	if raw := fields["conflicts"]; raw != nil {
		if !validStringList(raw) {
			return false
		}
		var conflicts []string
		if json.Unmarshal(raw, &conflicts) != nil || len(conflicts) != 0 {
			return false
		}
	}
	var changes []json.RawMessage
	if json.Unmarshal(fields["changes"], &changes) != nil || changes == nil {
		return false
	}
	for _, raw := range changes {
		change, err := exactOnlyFields(raw, "hostname", "app", "role", "route", "dns", "target", "previous")
		if err != nil {
			return false
		}
		values := make(map[string]string, len(change))
		for key, raw := range change {
			var value *string
			if json.Unmarshal(raw, &value) != nil || value == nil {
				return false
			}
			values[key] = *value
		}
		if values["hostname"] == "" || !slices.Contains([]string{"add", "change", "remove", "release", "adopt", "keep"}, values["route"]) {
			return false
		}
		if value := values["dns"]; value != "" && !slices.Contains([]string{"add", "change", "remove", "release", "adopt", "keep"}, value) {
			return false
		}
	}
	return true
}

// exactOnlyFields also refuses fields the renderer would otherwise silently discard.
func exactOnlyFields(data []byte, names ...string) (map[string]json.RawMessage, error) {
	fields, err := exactFields(data, names...)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(data, &all) != nil || len(all) != len(fields) {
		return nil, errors.New("invalid agent response")
	}
	return fields, nil
}

// validImageDiff accepts the three forms emitted by current and legacy planners.
func validImageDiff(data []byte) bool {
	fields, err := exactOnlyFields(data, "new", "same", "changes")
	if err != nil {
		return false
	}
	var isNew, isSame bool
	for name, dst := range map[string]*bool{"new": &isNew, "same": &isSame} {
		if raw := fields[name]; raw != nil && (string(raw) == "null" || json.Unmarshal(raw, dst) != nil) {
			return false
		}
	}
	var changes []json.RawMessage
	if raw := fields["changes"]; raw != nil && (json.Unmarshal(raw, &changes) != nil || changes == nil) {
		return false
	}
	if isNew && (isSame || len(changes) != 0) || isSame && len(changes) != 0 || !isNew && !isSame && len(changes) == 0 {
		return false
	}
	for _, change := range changes {
		item, err := exactOnlyFields(change, "area", "change")
		var area, description string
		if err != nil || json.Unmarshal(item["area"], &area) != nil || json.Unmarshal(item["change"], &description) != nil || area == "" || description == "" {
			return false
		}
	}
	return true
}

// safeDeployPlan redacts decoded strings and keys before rendering, without changing the checked selection.
func safeDeployPlan(data []byte, token string, asJSON bool, extra ...string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var plan map[string]any
	if dec.Decode(&plan) != nil || plan == nil || dec.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("invalid agent deploy plan")
	}
	tokens := append([]string{token}, extra...)
	for _, key := range []string{"app", "image", "tag"} {
		if value, ok := plan[key].(string); ok && redactSecretText(value, tokens...) != value {
			return nil, errors.New("cannot safely display deploy selection")
		}
	}
	safe, err := redactDeployValue(plan, !asJSON, tokens...)
	if err != nil {
		return nil, err
	}
	return safe.(map[string]any), nil
}

func redactDeployValue(value any, text bool, tokens ...string) (any, error) {
	sanitize := func(s string) string {
		s = redactSecretText(s, tokens...)
		if text {
			s = terminalSafe(s)
		}
		return s
	}
	switch v := value.(type) {
	case string:
		return sanitize(v), nil
	case []any:
		for i, item := range v {
			safe, err := redactDeployValue(item, text, tokens...)
			if err != nil {
				return nil, err
			}
			v[i] = safe
		}
		return v, nil
	case map[string]any:
		safe := make(map[string]any, len(v))
		for key, item := range v {
			name := sanitize(key)
			for _, decision := range []string{"app", "image", "tag", "roles", "blockers", "compose"} {
				if strings.EqualFold(key, decision) && name != key {
					return nil, errors.New("cannot safely display deploy plan")
				}
			}
			if _, exists := safe[name]; exists {
				return nil, errors.New("cannot safely display deploy plan")
			}
			var err error
			safe[name], err = redactDeployValue(item, text, tokens...)
			if err != nil {
				return nil, err
			}
		}
		return safe, nil
	default:
		return v, nil
	}
}

// safePromotionResponse keeps protocol identities intact while redacting decoded metadata.
func safePromotionResponse(data []byte, tokens ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("invalid agent response")
	}
	var containsSecret func(any) bool
	containsSecret = func(v any) bool {
		switch x := v.(type) {
		case string:
			return redactSecretText(x, tokens...) != x
		case []any:
			for _, item := range x {
				if containsSecret(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range x {
				if containsSecret(key) || containsSecret(item) {
					return true
				}
			}
		}
		return false
	}
	var valid func(any) bool
	valid = func(v any) bool {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				if !valid(item) {
					return false
				}
			}
		case map[string]any:
			for key, item := range x {
				switch strings.ToLower(key) {
				case "operation_id", "events_url", "approval_id", "app", "image", "tag", "roles", "blockers", "compose":
					if containsSecret(item) {
						return false
					}
				}
				if !valid(item) {
					return false
				}
			}
		}
		return true
	}
	if !valid(value) {
		return nil, errors.New("cannot safely display deploy response")
	}
	safe, err := redactDeployValue(value, false, tokens...)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return nil, errors.New("invalid agent response")
	}
	return encoded, nil
}

func showImagePlan(out io.Writer, data []byte, token string, asJSON bool, heading string, extra ...string) error {
	plan, err := safeDeployPlan(data, token, asJSON, extra...)
	if err != nil {
		return err
	}
	if redactSecretText(heading, append([]string{token}, extra...)...) != heading {
		return errors.New("cannot safely display deploy selection")
	}
	var buf bytes.Buffer
	if asJSON {
		encoded, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return errors.New("invalid agent deploy plan")
		}
		fmt.Fprintln(&buf, escapeJSONC1(string(encoded)))
	} else {
		if heading != "" {
			fmt.Fprintln(&buf, terminalSafe(heading))
		}
		lines := takeDiffLines(plan)
		if lines == nil {
			printText(&buf, plan, "")
		} else {
			app, _ := plan["app"].(string)
			delete(plan, "app")
			fmt.Fprintln(&buf, app)
			printLines(&buf, "  ", lines)
			if len(plan) > 0 {
				printText(&buf, plan, "")
			}
		}
	}
	n, err := out.Write(buf.Bytes())
	if err == nil && n != buf.Len() {
		err = io.ErrShortWrite
	}
	return err
}

// escapeJSONC1 preserves JSON values without writing C1 controls to a terminal.
func escapeJSONC1(encoded string) string {
	var out strings.Builder
	for _, r := range encoded {
		if r >= 0x80 && r <= 0x9f {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func interactiveDeploy(f commandFlags, out io.Writer) bool {
	screen, ok := out.(*os.File)
	return !f.json && ok && screen != nil && os.Stdin != nil && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(screen.Fd())
}

func runSingleTagDeploy(ctx context.Context, client *http.Client, target *agentclient.Target, f commandFlags, m *manifest.Manifest, req deploy.Request, selected reference.Named, before gitcheck.Identity, out, errout io.Writer) int {
	tagged, ok := selected.(reference.NamedTagged)
	if !ok {
		fmt.Fprintln(errout, "image tag required")
		return 1
	}
	progress := newProgressLine(errout, !f.json && stderrTerminal(errout))
	progress.Start("Checking checkout")
	co, err := agentclient.PrepareDeployFilesChecked(ctx, f.dir, f.env, m, []*deploy.Request{&req}, before)
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
			if err := showImagePlan(out, data, target.Token, f.json, ""); err != nil {
				fmt.Fprintln(errout, safeSecretText(err.Error(), target.Token))
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
	if err := showImagePlan(out, data, target.Token, f.json, "Deploy "+target.Project+"/"+target.Env+" app "+req.App); err != nil {
		fmt.Fprintln(errout, safeSecretText(err.Error(), target.Token))
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
