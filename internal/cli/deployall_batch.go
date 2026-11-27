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

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
	"github.com/distribution/reference"
)

// prepareBatchTags completes all policy, registry and plan reads before any admission.
func prepareBatchTags(ctx context.Context, client *http.Client, target *agentclient.Target, f commandFlags, m *manifest.Manifest, co gitcheck.Checkout, steps []deployStep, tags map[string]string, out, errout io.Writer) (bool, int) {
	fail := func(err any) (bool, int) {
		printRefusal(errout, safeSecretText(fmt.Sprint(err), target.Token), !f.json)
		return true, 1
	}
	progress := newProgressLine(errout, !f.json && stderrTerminal(errout))
	progress.Start("Checking agent policy")
	policy, err := agentclient.AgentProtection(ctx, client, target)
	progress.Close()
	if err != nil {
		printAgentRefusal(errout, terminalSafe(err.Error()), !f.json, agentclient.IsOlderAgentError(err))
		return true, 1
	}
	protected := m.Protected || policy == agentclient.ProtectionProtected || policy == agentclient.ProtectionLegacyProtected
	if protected && len(co.Changed) > 0 {
		return fail("the manifest has local changes: commit them first: " + terminalSafe(strings.Join(co.Changed, ", ")))
	}
	if policy == agentclient.ProtectionLegacyProtected || (policy == agentclient.ProtectionLegacyUnprotected && protected) {
		return fail("protected tag deployment requires an agent upgrade or an explicit digest")
	}
	if err := guardTaggedKept(ctx, client, target, m, co, steps, policy, f); err != nil {
		return fail(err)
	}
	if protected {
		for i := range steps {
			step := &steps[i]
			if !step.req.KeepVersion || step.previewErr != "" {
				continue
			}
			step.req.Version = step.image
			step.req.VersionTag = step.tag
			step.req.ExpectedApplied = &step.appliedID
			step.req.KeepVersion = false
			step.req.KeepDigest = ""
		}
	}
	if protected && !f.dry && !interactiveDeploy(f, out) {
		return fail("deploy confirmation requires terminal input and preview output; run interactively or deploy an explicit digest")
	}
	fieldDiff := f.deployDiff && !f.json
	if !protected && (policy != agentclient.ProtectionLegacyUnprotected || fieldDiff) {
		return false, 0
	}
	if protected {
		for i := range steps {
			step := &steps[i]
			if m.Apps[step.app].Kind != "image" || step.req.KeepVersion {
				continue
			}
			if _, mutable := tags[step.app]; mutable {
				continue
			}
			image, err := m.CheckImageDeploy(step.app, step.req.Version, true)
			if err != nil {
				return fail(err)
			}
			step.image = image
		}
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
			var guardErr string
			if e != nil {
				guardErr = "legacy agent check failed"
			} else if status != http.StatusOK {
				guardErr = agentclient.Message(status, data)
			} else if e = imagePlan(data, m, guard, "", tag); e != nil {
				guardErr = e.Error()
			}
			if guardErr != "" {
				guardErr += "; upgrade the agent or deploy an explicit digest"
				if !f.dry {
					return fail(guardErr)
				}
				step.previewErr = guardErr
				continue
			}
			step.preview = data
			continue
		}
		payload, _ := json.Marshal(api.ImageLookup{Manifest: *m, App: step.app, Tag: tag})
		progress.Start("Resolving image tag")
		result, e := agentclient.DoResult(ctx, client, target.Base, target.Prefix, target.Token, "POST", "images/resolve", "", payload)
		data, status := result.Data, result.Status
		progress.Close()
		if e != nil {
			return fail("image tag lookup failed")
		}
		if status != http.StatusOK {
			printAgentRefusal(errout, safeSecretText(agentclient.Message(status, data), target.Token), !f.json, result.Older)
			return true, 1
		}
		fields, e := exactFields(data, "image", "tag")
		var image, gotTag string
		if e != nil || json.Unmarshal(fields["image"], &image) != nil || json.Unmarshal(fields["tag"], &gotTag) != nil || gotTag != tag || !pinnedImage(m, step.app, image) {
			return fail("invalid image tag lookup response")
		}
		step.req.Version, step.req.VersionTag = image, tag
		step.image = image
		if _, e = m.CheckImageDeploy(step.app, image, true); e != nil {
			return fail(e)
		}
	}
	if !protected {
		return false, 0
	}
	if fieldDiff {
		return prepareBatchFieldPreviews(ctx, client, target, f, steps, out, errout)
	}
	for i := range steps {
		step := &steps[i]
		if step.previewErr != "" {
			continue
		}
		preview := step.req
		preview.DryRun = true
		payload, _ := json.Marshal(preview)
		progress.Start("Planning deploy")
		result, e := agentclient.DoResult(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "", payload)
		data, status := result.Data, result.Status
		progress.Close()
		var planErr error
		if e != nil {
			planErr = errors.New("deploy preview failed")
		} else if status != http.StatusOK {
			planErr = errors.New(agentclient.Message(status, data))
		}
		image, tag := step.image, step.req.VersionTag
		if step.req.KeepVersion {
			tag = step.tag
		}
		if planErr == nil {
			planErr = batchPlan(data, m, f.env, step.req, image, tag, target.Token, f.json)
		}
		if planErr != nil {
			if !f.dry {
				if result.Older {
					printAgentRefusal(errout, safeSecretText(planErr.Error(), target.Token), !f.json, true)
					return true, 1
				}
				return fail(planErr)
			}
			step.previewErr = planErr.Error()
			step.previewOlder = result.Older
			continue
		}
		step.preview = data
	}
	if f.dry {
		return false, 0
	}
	for _, step := range steps {
		if err := showImagePlan(out, step.preview, target.Token, false, "Deploy "+target.Project+"/"+target.Env+" app "+step.app); err != nil {
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

func prepareBatchFieldPreviews(ctx context.Context, client *http.Client, target *agentclient.Target, f commandFlags, steps []deployStep, out, errout io.Writer) (bool, int) {
	stop := func(i int, err error, older bool) (bool, int) {
		code := reportDeployFieldError(errout, target, err, older)
		printDeployBatchStop(errout, target, steps, i, 0)
		return true, code
	}
	for i := range steps {
		step := &steps[i]
		if step.previewErr != "" {
			continue
		}
		preview, older, err := requestDeployFieldPreview(ctx, client, target, &step.req, step.appliedID, step.tag, errout)
		if err != nil && !errors.As(err, &step.diffBlockers) {
			if !f.dry {
				return stop(i, err, older)
			}
			step.previewErr, step.previewOlder = err.Error(), older
			continue
		}
		step.diffPreview = preview
		if f.dry {
			continue
		}
		if preview.Base < 0 {
			return stop(i, errDeployFieldUnavailable, false)
		}
		if len(step.diffBlockers) != 0 {
			_, renderErr := renderDeployFieldPreview(out, target, step.app, preview, step.diffBlockers, false)
			if renderErr != nil {
				err = renderErr
			}
			return stop(i, err, false)
		}
	}
	if f.dry {
		return false, 0
	}
	for i := range steps {
		step := &steps[i]
		var err error
		if i == 0 {
			step.diffShown, err = renderDeployFieldPreview(out, target, step.app, step.diffPreview, nil, false)
		} else {
			heading := "Deploy " + target.Project + "/" + target.Env + " app " + step.app
			err = showImagePlan(out, step.diffPreview.Plan, target.Token, false, heading)
		}
		if err != nil {
			return stop(i, err, false)
		}
	}
	yes, err := confirmDeploy(ctx, os.Stdin, out)
	if err != nil {
		return true, reportDeployFieldError(errout, target, err, false)
	}
	if !yes || ctx.Err() != nil {
		return true, 1
	}
	return false, 0
}

func batchPlan(data []byte, m *manifest.Manifest, env string, req deploy.Request, image, tag, token string, asJSON bool) error {
	invalid := errors.New("invalid agent deploy plan")
	if m.Apps[req.App].Kind != "compose" {
		return imagePlan(data, m, req, image, tag)
	}
	fields, err := exactFields(data, "app", "image", "tag", "roles", "blockers", "compose", "diff", "remove", "destroy", "restore", "exec", "command")
	if err != nil {
		return invalid
	}
	if raw := fields["diff"]; raw != nil && !validImageDiff(raw) {
		return invalid
	}
	for _, key := range []string{"remove", "destroy", "restore", "exec", "command"} {
		if raw := fields[key]; raw != nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return invalid
		}
	}
	var app string
	var gotImage, gotTag *string
	var roles []string
	if json.Unmarshal(fields["app"], &app) != nil || app != req.App || json.Unmarshal(fields["image"], &gotImage) != nil || gotImage == nil || *gotImage != "" || json.Unmarshal(fields["roles"], &roles) != nil || roles == nil || len(roles) != 0 {
		return invalid
	}
	if fields["tag"] != nil && (json.Unmarshal(fields["tag"], &gotTag) != nil || gotTag == nil || *gotTag != "") {
		return invalid
	}
	cf, err := exactFields(fields["compose"], "project", "services", "volumes", "blockers")
	if err != nil {
		return invalid
	}
	var plan deploy.ComposePlan
	if json.Unmarshal(fields["compose"], &plan) != nil || plan.Project != m.ComposeProject(env, req.App) || len(plan.Services) == 0 || plan.Volumes == nil {
		return invalid
	}
	for _, field := range []string{"services", "volumes"} {
		names := make(map[string]bool)
		var entries []json.RawMessage
		if json.Unmarshal(cf[field], &entries) != nil || entries == nil {
			return invalid
		}
		for _, entry := range entries {
			item, err := exactFields(entry, "name", "action")
			var name, action string
			if err != nil || json.Unmarshal(item["name"], &name) != nil || json.Unmarshal(item["action"], &action) != nil || name == "" || action == "" || names[name] {
				return invalid
			}
			names[name] = true
		}
	}
	top, err := readDeployBlockers(fields["blockers"])
	if err != nil {
		return invalid
	}
	compose, err := readDeployBlockers(cf["blockers"])
	if err != nil {
		return invalid
	}
	safeDecision := func(s string) bool {
		visible := redactSecretText(s, token)
		if !asJSON {
			visible = terminalSafe(visible)
		}
		return visible == s
	}
	if !safeDecision(plan.Project) {
		return errors.New("cannot safely display deploy selection")
	}
	desired := false
	for _, s := range plan.Services {
		if s.Name == "" || (s.Action != "create" && s.Action != "recreate" && s.Action != "keep" && s.Action != "remove") {
			return invalid
		}
		if !safeDecision(s.Name) || !safeDecision(s.Action) {
			return errors.New("cannot safely display deploy selection")
		}
		if s.Action != "remove" {
			desired = true
		}
	}
	if !desired {
		return invalid
	}
	for _, v := range plan.Volumes {
		if v.Name == "" || (v.Action != "create" && v.Action != "keep") {
			return invalid
		}
		if !safeDecision(v.Name) || !safeDecision(v.Action) {
			return errors.New("cannot safely display deploy selection")
		}
	}
	if blockers := append(top, compose...); len(blockers) > 0 {
		return blockers
	}
	return nil
}

// hasTaggedKept identifies historical tag-plus-digest releases that protected agents reject.
func hasTaggedKept(steps []deployStep) bool {
	for _, step := range steps {
		if !step.req.KeepVersion {
			continue
		}
		ref, err := reference.ParseNormalizedNamed(step.req.KeepDigest)
		if err == nil {
			if _, ok := ref.(reference.NamedTagged); ok {
				return true
			}
		}
	}
	return false
}

// guardTaggedKept checks only the historical form whose validity depends on effective protection.
func guardTaggedKept(ctx context.Context, client *http.Client, target *agentclient.Target, m *manifest.Manifest, co gitcheck.Checkout, steps []deployStep, policy agentclient.Protection, f commandFlags) error {
	for i := range steps {
		step := &steps[i]
		if !step.req.KeepVersion {
			continue
		}
		ref, err := reference.ParseNormalizedNamed(step.req.KeepDigest)
		if err != nil {
			return errors.New("invalid applied release")
		}
		if _, tagged := ref.(reference.NamedTagged); !tagged {
			continue
		}
		var guardErr error
		switch {
		case m.Protected || policy == agentclient.ProtectionProtected || policy == agentclient.ProtectionLegacyProtected:
			guardErr = fmt.Errorf("image app %s keeps a tagged digest; deploy an explicit digest", step.app)
		case policy == agentclient.ProtectionLegacyUnprotected && (!f.deployDiff || f.json):
			guard := step.req
			guard.DryRun = true
			guard.Modified = guard.Modified || len(co.Changed) > 0
			payload, _ := json.Marshal(guard)
			data, status, err := agentclient.Do(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "", payload)
			switch {
			case err != nil:
				guardErr = errors.New("legacy agent check failed; upgrade the agent or deploy an explicit digest")
			case status != http.StatusOK:
				guardErr = errors.New(agentclient.Message(status, data) + "; upgrade the agent or deploy an explicit digest")
			default:
				if err := legacyKeptPlan(data, m, *step); err != nil {
					guardErr = err
				} else if f.dry {
					step.preview = data
				}
			}
		}
		if guardErr != nil {
			if !f.dry {
				return guardErr
			}
			step.previewErr = guardErr.Error()
		}
	}
	return nil
}

func legacyKeptPlan(data []byte, m *manifest.Manifest, step deployStep) error {
	invalid := errors.New("invalid legacy deploy plan; upgrade the agent or deploy an explicit digest")
	fields, err := exactFields(data, "app", "image", "tag", "roles", "blockers", "compose", "diff", "remove", "destroy", "restore", "exec", "command", "schema_revert")
	if err != nil {
		return invalid
	}
	var image string
	if json.Unmarshal(fields["image"], &image) != nil {
		return invalid
	}
	canonical, err := promotionImage(m, step.app, image)
	if err != nil || canonical != step.image {
		return invalid
	}
	tag := step.tag
	if raw := fields["tag"]; raw != nil {
		var present *string
		if json.Unmarshal(raw, &present) != nil || present == nil || *present != tag {
			return invalid
		}
	} else if tag != "" {
		return invalid
	}
	var plan map[string]json.RawMessage
	if json.Unmarshal(data, &plan) != nil {
		return invalid
	}
	plan["image"], _ = json.Marshal(canonical)
	normalized, err := json.Marshal(plan)
	if err != nil || imagePlan(normalized, m, step.req, canonical, tag) != nil {
		return invalid
	}
	return nil
}
