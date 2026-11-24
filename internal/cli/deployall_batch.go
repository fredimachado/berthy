package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
)

// prepareBatchTags completes all policy, registry and plan reads before any admission.
func prepareBatchTags(ctx context.Context, client *http.Client, target *agentclient.Target, f commandFlags, m *manifest.Manifest, co gitcheck.Checkout, steps []deployStep, tags map[string]string, out, errout io.Writer) (bool, int) {
	fail := func(err any) (bool, int) { fmt.Fprintln(errout, err); return true, 1 }
	progress := newProgressLine(errout, !f.json && stderrTerminal(errout))
	if m.Protected && len(co.Changed) > 0 {
		return fail("the manifest has local changes: commit them first: " + terminalSafe(strings.Join(co.Changed, ", ")))
	}
	progress.Start("Checking agent policy")
	policy, err := agentclient.AgentProtection(ctx, client, target)
	progress.Close()
	if err != nil {
		return fail(target.Redact(err.Error()))
	}
	protected := m.Protected || policy == agentclient.ProtectionProtected || policy == agentclient.ProtectionLegacyProtected
	if protected && len(co.Changed) > 0 {
		return fail("the manifest has local changes: commit them first: " + terminalSafe(strings.Join(co.Changed, ", ")))
	}
	if policy == agentclient.ProtectionLegacyProtected || (policy == agentclient.ProtectionLegacyUnprotected && protected) {
		return fail("protected tag deployment requires an agent upgrade or an explicit digest")
	}
	if protected && !f.dry && !interactiveDeploy(f, out) {
		return fail("deploy confirmation requires terminal input and preview output; run interactively or deploy an explicit digest")
	}
	if !protected && policy != agentclient.ProtectionLegacyUnprotected {
		return false, 0
	}
	for i := range steps {
		step := &steps[i]
		tag, explicit := tags[step.app]
		if !explicit {
			continue
		}
		if policy == agentclient.ProtectionLegacyUnprotected {
			guard := step.req
			guard.DryRun = true
			guard.Modified = guard.Modified || len(co.Changed) > 0
			payload, _ := json.Marshal(guard)
			progress.Start("Checking legacy agent")
			data, status, e := agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "", payload)
			progress.Close()
			if e != nil {
				return fail("legacy agent check failed; upgrade the agent or deploy an explicit digest")
			}
			if status != http.StatusOK {
				return fail(target.Redact(agentclient.Message(status, data)) + "; upgrade the agent or deploy an explicit digest")
			}
			if e = imagePlan(data, m, guard, "", tag); e != nil {
				return fail(e.Error() + "; upgrade the agent or deploy an explicit digest")
			}
			step.preview = []byte(target.Redact(string(data)))
			continue
		}
		payload, _ := json.Marshal(api.ImageLookup{Manifest: *m, App: step.app, Tag: tag})
		progress.Start("Resolving image tag")
		data, status, e := agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "images/resolve", "", payload)
		progress.Close()
		if e != nil {
			return fail("image tag lookup failed")
		}
		if status != http.StatusOK {
			return fail(target.Redact(agentclient.Message(status, data)))
		}
		fields, e := exactFields(data, "image", "tag")
		var image, gotTag string
		if e != nil || json.Unmarshal(fields["image"], &image) != nil || json.Unmarshal(fields["tag"], &gotTag) != nil || gotTag != tag || !pinnedImage(m, step.app, image) {
			return fail("invalid image tag lookup response")
		}
		step.req.Version, step.req.VersionTag = image, tag
		if _, e = m.CheckImageDeploy(step.app, image, true); e != nil {
			return fail(e)
		}
	}
	if !protected {
		return false, 0
	}
	for i := range steps {
		step := &steps[i]
		preview := step.req
		preview.DryRun = true
		payload, _ := json.Marshal(preview)
		progress.Start("Planning deploy")
		data, status, e := agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "", payload)
		progress.Close()
		var planErr error
		if e != nil {
			planErr = errors.New("deploy preview failed")
		} else if status != http.StatusOK {
			planErr = errors.New(target.Redact(agentclient.Message(status, data)))
		}
		image, tag := step.req.Version, step.req.VersionTag
		if step.req.KeepVersion {
			image, tag = step.req.KeepDigest, step.tag
		}
		if planErr == nil {
			planErr = batchPlan(data, m, f.env, step.req, image, tag)
		}
		if planErr != nil {
			if !f.dry {
				return fail(planErr)
			}
			step.previewErr = planErr.Error()
			continue
		}
		step.preview = []byte(target.Redact(string(data)))
	}
	if f.dry {
		return false, 0
	}
	for _, step := range steps {
		if err := showImagePlan(out, errout, step.preview, false, "Deploy "+target.Project+"/"+target.Env+" app "+step.app); err != nil {
			return fail(err)
		}
	}
	yes, err := confirmDeploy(ctx, os.Stdin, out)
	if err != nil {
		return fail(err)
	}
	if !yes || ctx.Err() != nil {
		return true, 1
	}
	return false, 0
}

func batchPlan(data []byte, m *manifest.Manifest, env string, req deploy.Request, image, tag string) error {
	invalid := errors.New("invalid agent deploy plan")
	fields, err := exactFields(data, "app", "image", "tag", "roles", "blockers", "compose")
	if err != nil {
		return invalid
	}
	if raw := fields["blockers"]; raw != nil {
		var b []string
		if json.Unmarshal(raw, &b) != nil || b == nil {
			return invalid
		}
		if len(b) > 0 {
			return errors.New("deploy plan has blockers")
		}
	}
	if m.Apps[req.App].Kind != "compose" {
		// imagePlan requires a tag field; an omitted optional tag means empty.
		if fields["tag"] == nil && tag == "" {
			var obj map[string]json.RawMessage
			if json.Unmarshal(data, &obj) != nil {
				return invalid
			}
			obj["tag"] = json.RawMessage(`""`)
			data, _ = json.Marshal(obj)
		}
		return imagePlan(data, m, req, image, tag)
	}
	var app, gotImage, gotTag string
	var roles []string
	if json.Unmarshal(fields["app"], &app) != nil || app != req.App || json.Unmarshal(fields["image"], &gotImage) != nil || gotImage != "" || json.Unmarshal(fields["roles"], &roles) != nil || roles == nil || len(roles) != 0 {
		return invalid
	}
	if fields["tag"] != nil && (json.Unmarshal(fields["tag"], &gotTag) != nil || gotTag != "") {
		return invalid
	}
	cf, err := exactFields(fields["compose"], "project", "services", "volumes", "blockers")
	if err != nil {
		return invalid
	}
	var plan deploy.ComposePlan
	if json.Unmarshal(fields["compose"], &plan) != nil || plan.Project != m.ComposeProject(env, req.App) || plan.Services == nil || plan.Volumes == nil {
		return invalid
	}
	for _, field := range []string{"services", "volumes"} {
		var entries []json.RawMessage
		if json.Unmarshal(cf[field], &entries) != nil || entries == nil {
			return invalid
		}
		for _, entry := range entries {
			item, err := exactFields(entry, "name", "action")
			var name, action string
			if err != nil || json.Unmarshal(item["name"], &name) != nil || json.Unmarshal(item["action"], &action) != nil || name == "" || action == "" {
				return invalid
			}
		}
	}
	if cf["blockers"] != nil && (json.Unmarshal(cf["blockers"], &plan.Blockers) != nil || plan.Blockers == nil) {
		return invalid
	}
	if len(plan.Blockers) > 0 {
		return errors.New("deploy plan has blockers")
	}
	for _, s := range plan.Services {
		if s.Name == "" || (s.Action != "create" && s.Action != "recreate" && s.Action != "keep" && s.Action != "remove") {
			return invalid
		}
	}
	for _, v := range plan.Volumes {
		if v.Name == "" || (v.Action != "create" && v.Action != "keep") {
			return invalid
		}
	}
	return nil
}
