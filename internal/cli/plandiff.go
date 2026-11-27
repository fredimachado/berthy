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
	"strconv"
	"strings"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/strictjson"

	"github.com/distribution/reference"
)

type deployDiffPreview struct {
	Plan   []byte
	Base   int64
	Fields []deploy.DiffField
}

type deployPlanBlockers []string

func (deployPlanBlockers) Error() string { return "deploy plan has blockers" }

func readDeployBlockers(raw json.RawMessage) (deployPlanBlockers, error) {
	if raw == nil {
		return nil, nil
	}
	var values []*string
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, errors.New("invalid agent deploy plan")
	}
	var blockers deployPlanBlockers
	for _, v := range values {
		if v == nil {
			return nil, errors.New("invalid agent deploy plan")
		}
		blockers = append(blockers, *v)
	}
	return blockers, nil
}

func readDeployDiffPreview(data []byte) (deployDiffPreview, error) {
	invalid := errors.New("invalid agent deploy plan")
	var plan map[string]json.RawMessage
	if !utf8.Valid(data) || strictjson.Unmarshal(data, &plan) != nil || plan == nil {
		return deployDiffPreview{}, invalid
	}
	for key := range plan {
		if key != "release_diff" && strings.EqualFold(key, "release_diff") {
			return deployDiffPreview{}, invalid
		}
	}
	raw := plan["release_diff"]
	if raw == nil {
		return deployDiffPreview{Plan: data, Base: -1}, nil
	}
	var detail struct {
		Base   *int64 `json:"base"`
		Fields []struct {
			Path   []string        `json:"path"`
			Before json.RawMessage `json:"before"`
			After  json.RawMessage `json:"after"`
			Note   json.RawMessage `json:"note"`
		} `json:"fields"`
	}
	var compact deploy.Diff
	if strictjson.Unmarshal(raw, &detail) != nil || detail.Base == nil || *detail.Base < 0 || detail.Fields == nil ||
		!validImageDiff(plan["diff"]) || json.Unmarshal(plan["diff"], &compact) != nil ||
		compact.New != (*detail.Base == 0) || (*detail.Base == 0 && len(detail.Fields) != 0) {
		return deployDiffPreview{}, invalid
	}
	preview := deployDiffPreview{Base: *detail.Base, Fields: make([]deploy.DiffField, 0, len(detail.Fields))}
	for _, field := range detail.Fields {
		if len(field.Path) == 0 {
			return deployDiffPreview{}, invalid
		}
		for _, component := range field.Path {
			if component == "" {
				return deployDiffPreview{}, invalid
			}
		}
		row := deploy.DiffField{Path: field.Path}
		if field.Before != nil && (json.Unmarshal(field.Before, &row.Before) != nil || row.Before == nil) ||
			field.After != nil && (json.Unmarshal(field.After, &row.After) != nil || row.After == nil) {
			return deployDiffPreview{}, invalid
		}
		if field.Note != nil {
			if json.Unmarshal(field.Note, &row.Note) != nil || row.Note == "" || row.Before != nil || row.After != nil {
				return deployDiffPreview{}, invalid
			}
		} else if row.Before == nil && row.After == nil {
			return deployDiffPreview{}, invalid
		}
		preview.Fields = append(preview.Fields, row)
	}
	delete(plan, "release_diff")
	preview.Plan, _ = json.Marshal(plan)
	return preview, nil
}

func printDeployDiff(w io.Writer, project, env, app string, d deploy.Diff, fields []deploy.DiffField, s cliStyle) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Deploy %s/%s app %s\n", terminalSafe(project), terminalSafe(env), terminalSafe(app))
	if len(fields) == 0 {
		fmt.Fprintln(&buf, "  "+(deploy.Diff{New: d.New}).Lines()[0])
		return writeDeployOutput(w, &buf)
	}
	var parents []string
	for _, row := range fields {
		path := row.Path
		next := path[:len(path)-1]
		shared := 0
		for shared < min(len(parents), len(next)) && parents[shared] == next[shared] {
			shared++
		}
		for i := shared; i < len(next); i++ {
			fmt.Fprintln(&buf, strings.Repeat("  ", i+1)+terminalSafe(next[i])+":")
		}
		parents = next
		indent := strings.Repeat("  ", len(path))
		name := terminalSafe(path[len(path)-1])
		if row.Note != "" {
			if path[0] != "compose" {
				name += ": "
			} else {
				name = ""
			}
			fmt.Fprintln(&buf, indent+name+terminalSafe(row.Note))
			continue
		}
		writeSide := func(value *string, sign string, tone cliTone) {
			if value == nil {
				return
			}
			text := terminalSafe(*value)
			if text == "" {
				text = `""`
			}
			fmt.Fprintln(&buf, indent+s.paint(tone, sign+name+": "+text))
		}
		writeSide(row.Before, "- ", toneError)
		writeSide(row.After, "+ ", toneOK)
	}
	return writeDeployOutput(w, &buf)
}

// showDeployFieldPreview writes the comparison and the checked selection before any confirmation or admission.
func showDeployFieldPreview(out io.Writer, target *agentclient.Target, app string, preview deployDiffPreview, blockers deployPlanBlockers) (*deploy.Diff, error) {
	plan, err := safeDeployPlan(preview.Plan, target.Token, false)
	if err != nil {
		return nil, err
	}
	var raw struct{ Diff *deploy.Diff }
	if json.Unmarshal(preview.Plan, &raw) != nil || raw.Diff == nil {
		return nil, errors.New("invalid agent deploy plan")
	}
	delete(plan, "app")
	delete(plan, "diff")
	delete(plan, "blockers")
	if compose, ok := plan["compose"].(map[string]any); ok {
		delete(compose, "blockers")
	}
	redact := func(s string) string { return redactSecretText(s, target.Token) }
	for i := range preview.Fields {
		row := &preview.Fields[i]
		for j, key := range row.Path {
			row.Path[j] = redact(key)
		}
		if row.Before != nil {
			*row.Before = redact(*row.Before)
		}
		if row.After != nil {
			*row.After = redact(*row.After)
		}
		row.Note = redact(row.Note)
	}
	s := styleFor(out, true)
	var buf bytes.Buffer
	if err := printDeployDiff(&buf, redact(target.Project), redact(target.Env), redact(app), *raw.Diff, preview.Fields, s); err != nil {
		return nil, err
	}
	printStyledText(&buf, plan, "", s, shapeGeneric)
	if len(blockers) != 0 {
		fmt.Fprintln(&buf, "blockers:")
		seen := make(map[string]bool, len(blockers))
		for _, reason := range blockers {
			safe := terminalSafe(redact(reason))
			if !seen[safe] {
				fmt.Fprintln(&buf, "  - "+s.paint(toneError, safe))
				seen[safe] = true
			}
		}
	}
	return raw.Diff, writeDeployOutput(out, &buf)
}

func runDeployFieldPreview(ctx context.Context, client *http.Client, target *agentclient.Target, f commandFlags, req deploy.Request, confirm bool, out, errout io.Writer) int {
	fail := func(err error) int {
		printRefusal(errout, safeSecretText(err.Error(), target.Token), true)
		return 1
	}
	m := &req.Manifest
	var selected reference.Named
	var image, tag string
	if m.Apps[req.App].Kind == "image" {
		var err error
		selected, err = m.ImageReference(req.App, req.Version)
		if err != nil {
			return fail(err)
		}
		if _, pinned := selected.(reference.Canonical); pinned {
			image = selected.String()
		}
		tag = req.VersionTag
		if tagged, ok := selected.(reference.NamedTagged); ok && tag == "" {
			tag = tagged.Tag()
		}
	}
	previewReq := req
	previewReq.DryRun = true
	payload, err := json.Marshal(previewReq)
	if err != nil {
		return fail(errors.New("deploy preview failed"))
	}
	progress := startWait(errout, true, "Planning deploy")
	result, err := agentclient.DoResult(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", "diff=fields", payload)
	progress.Close()
	if err != nil {
		return fail(fmt.Errorf("deploy preview failed: %w", err))
	}
	if result.Status != http.StatusOK {
		printAgentRefusal(errout, safeSecretText(agentclient.Message(result.Status, result.Data), target.Token), true, result.Older)
		return 1
	}
	preview, err := readDeployDiffPreview(result.Data)
	if err != nil {
		return fail(err)
	}
	var blockers deployPlanBlockers
	planErr := batchPlan(preview.Plan, m, target.Env, req, image, tag, target.Token, false)
	if planErr != nil && !errors.As(planErr, &blockers) {
		return fail(planErr)
	}
	if preview.Base < 0 {
		if !f.dry {
			return fail(errors.New("deploy field detail is unavailable; upgrade the agent or run without --diff"))
		}
		if err := showImagePlan(out, preview.Plan, target.Token, false, ""); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "deploy field detail is unavailable")
		if confirm && len(blockers) != 0 {
			return 1
		}
		return 0
	}
	shown, err := showDeployFieldPreview(out, target, req.App, preview, blockers)
	if err != nil {
		return fail(err)
	}
	if len(blockers) != 0 && (!f.dry || confirm) {
		return fail(blockers)
	}
	if f.dry {
		return 0
	}
	if confirm {
		yes, err := confirmDeploy(ctx, os.Stdin, out)
		if err != nil {
			return fail(err)
		}
		if !yes || ctx.Err() != nil {
			return fail(errors.New("deploy cancelled"))
		}
	}
	if _, pinned := selected.(reference.Canonical); selected != nil && !pinned {
		var plan struct{ Image string }
		if json.Unmarshal(preview.Plan, &plan) != nil {
			return fail(errors.New("invalid agent deploy plan"))
		}
		req.Version, req.VersionTag = plan.Image, tag
	}
	req.DryRun, req.ExpectedApplied = false, &preview.Base
	f.shownDeployDiff = shown
	return sendCommand(ctx, client, target.Base, target.Prefix, target.Token, "POST", "deploys", req, f, "deploy", out, errout)
}

func takeDiff(plan map[string]any) (deploy.Diff, bool) {
	raw, ok := plan["diff"]
	if !ok {
		return deploy.Diff{}, false
	}
	b, err := json.Marshal(raw)
	var d deploy.Diff
	if err != nil || json.Unmarshal(b, &d) != nil {
		return deploy.Diff{}, false
	}
	delete(plan, "diff")
	return d, true
}

func diffMarker(change string) cliTone {
	switch {
	case strings.HasPrefix(change, "+ "):
		return toneOK
	case strings.HasPrefix(change, "- "):
		return toneError
	case strings.HasPrefix(change, "~ "):
		return toneWarning
	default:
		return tonePlain
	}
}

// diffMarkers retains only classification from redacted, unsanitized text.
func diffMarkers(data []byte, held bool, tokens ...string) []cliTone {
	var plan map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&plan) != nil || dec.Decode(new(any)) != io.EOF {
		return []cliTone{}
	}
	if held {
		plan, _ = plan["plan"].(map[string]any)
	}
	d, ok := takeDiff(plan)
	if !ok {
		return []cliTone{}
	}
	tones := make([]cliTone, len(d.Changes))
	for i, c := range d.Changes {
		tones[i] = diffMarker(redactSecretText(c.Change, tokens...))
	}
	return tones
}

func printStyledDiff(w io.Writer, indent string, d deploy.Diff, s cliStyle) {
	printStyledDiffMarkers(w, indent, d, s, nil)
}

func printStyledDiffMarkers(w io.Writer, indent string, d deploy.Diff, s cliStyle, markers []cliTone) {
	lines := d.Lines()
	for i, line := range lines {
		if !d.New && len(d.Changes) > 0 && i < len(d.Changes) && strings.HasSuffix(line, d.Changes[i].Change) {
			change := d.Changes[i].Change
			area := line[:len(line)-len(change)]
			tone := diffMarker(change)
			if markers != nil {
				tone = tonePlain
				if i < len(markers) {
					tone = markers[i]
				}
			}
			if tone != tonePlain && len(change) >= 2 {
				change = s.paint(tone, terminalSafe(change[:2])) + terminalSafe(change[2:])
			} else {
				change = terminalSafe(change)
			}
			fmt.Fprintln(w, indent+s.paint(toneMuted, terminalSafe(area))+change)
		} else {
			fmt.Fprintln(w, indent+terminalSafe(line))
		}
	}
}

// printDryRun prints the plan of a dry-run deploy or rollback as text: the app, what it changes, then the rest of the plan.
// A plan without a diff, such as an adoption's, prints as any answer does.
func printDryRun(out, errout io.Writer, b []byte) int {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	plan, ok := v.(map[string]any)
	if !ok {
		printText(out, v, "")
		return 0
	}
	diff, ok := takeDiff(plan)
	if !ok {
		printText(out, plan, "")
		return 0
	}
	app, _ := plan["app"].(string)
	delete(plan, "app")
	fmt.Fprintln(out, terminalSafe(app))
	printStyledDiff(out, "  ", diff, styleFor(out, true))
	if len(plan) > 0 {
		printText(out, plan, "")
	}
	return 0
}

// printDiff prints the answer of history --diff: the lines of a diff.
func printDiff(out, errout io.Writer, b []byte) int {
	var d deploy.Diff
	if json.Unmarshal(b, &d) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	printStyledDiff(out, "", d, styleFor(out, true))
	return 0
}

// releaseNumber reports whether s is the number of a release: a whole number from 1.
func releaseNumber(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= 1
}
