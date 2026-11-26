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
	"strings"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
	"github.com/distribution/reference"
)

type promotionSnapshot struct {
	source, destination  *agentclient.Target
	app, image, tag      string
	sourceID, expectedID int64
}

// readApplied accepts absence only from a complete, unambiguous not_deployed response.
func readApplied(ctx context.Context, c *http.Client, t *agentclient.Target, app string, extraTokens ...string) (appliedRelease, bool, error) {
	var result appliedRelease
	u := *t.Base
	u.Path = t.Prefix + "applied/" + url.PathEscape(app)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return result, false, errors.New("invalid agent request")
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return result, false, errors.New(safeSecretText(err.Error(), append([]string{t.Token}, extraTokens...)...))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return result, false, agentclient.ErrInvalidResponse
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return result, false, errors.New(safeSecretText(agentclient.Message(resp.StatusCode, data), append([]string{t.Token}, extraTokens...)...))
	}
	fields, ok := appliedFields(data)
	if !ok {
		return result, false, agentclient.ErrInvalidResponse
	}
	if resp.StatusCode == http.StatusNotFound {
		var code, message string
		if v, ok := fields["code"]; ok && json.Unmarshal(v, &code) == nil && code == "not_deployed" && fields["message"] != nil && json.Unmarshal(fields["message"], &message) == nil && message != "" && fields["id"] == nil && fields["digest"] == nil && fields["tag"] == nil {
			return result, false, nil
		}
		return result, false, errors.New(safeSecretText(agentclient.Message(resp.StatusCode, data), append([]string{t.Token}, extraTokens...)...))
	}
	if fields["code"] != nil || fields["id"] == nil || fields["digest"] == nil || bytes.Equal(bytes.TrimSpace(fields["id"]), []byte("null")) || bytes.Equal(bytes.TrimSpace(fields["digest"]), []byte("null")) || json.Unmarshal(fields["id"], &result.ID) != nil || result.ID <= 0 || json.Unmarshal(fields["digest"], &result.Digest) != nil || result.Digest == "" {
		return appliedRelease{}, false, agentclient.ErrInvalidResponse
	}
	if tag := fields["tag"]; tag != nil && (bytes.Equal(bytes.TrimSpace(tag), []byte("null")) || json.Unmarshal(tag, &result.Tag) != nil) {
		return appliedRelease{}, false, agentclient.ErrInvalidResponse
	}
	return result, true, nil
}

func appliedFields(data []byte) (map[string]json.RawMessage, bool) {
	if !utf8.Valid(data) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		folded := strings.ToLower(key)
		if seen[folded] {
			return nil, false
		}
		seen[folded] = true
		if (folded == "id" || folded == "digest" || folded == "tag" || folded == "code" || folded == "message") && key != folded {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		fields[key] = value
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return nil, false
	}
	return fields, true
}

func selectPromotion(ctx context.Context, c *http.Client, dst *agentclient.Target, cfgPath, from string, m *manifest.Manifest, app string) (promotionSnapshot, error) {
	var snapshot promotionSnapshot
	if from == "" || from == dst.Env {
		return snapshot, errors.New("source and destination must be different environments")
	}
	if a, ok := m.Apps[app]; !ok || a.Kind != "image" {
		return snapshot, errors.New("promotion requires an image app")
	}
	src, err := agentclient.Resolve(cfgPath, agentclient.Selector{Project: dst.Project, Environment: from})
	if err != nil {
		return snapshot, errors.New(safeSecretText(err.Error(), dst.Token))
	}
	source, found, err := readApplied(ctx, c, src, app, dst.Token)
	if err != nil {
		return snapshot, fmt.Errorf("source: %w", err)
	}
	if !found {
		return snapshot, errors.New("source app is not deployed")
	}
	image, err := promotionImage(m, app, source.Digest)
	if err != nil {
		return snapshot, err
	}
	if source.Tag != "" && reference.TagRegexp.FindString(source.Tag) != source.Tag {
		return snapshot, errors.New("invalid source release tag")
	}
	dest, found, err := readApplied(ctx, c, dst, app, src.Token)
	if err != nil {
		return snapshot, fmt.Errorf("destination: %w", err)
	}
	if found {
		current, err := promotionImage(m, app, dest.Digest)
		if err != nil {
			return snapshot, fmt.Errorf("destination: %s", src.Redact(err.Error()))
		}
		if current == image {
			return snapshot, errors.New("source image is already deployed at destination")
		}
	}
	return promotionSnapshot{source: src, destination: dst, app: app, image: image, tag: source.Tag, sourceID: source.ID, expectedID: dest.ID}, nil
}

func promotionImage(m *manifest.Manifest, app, image string) (string, error) {
	if !strings.Contains(image, "@") {
		return "", errors.New("applied release requires a qualified image digest")
	}
	ref, err := m.ImageReference(app, image)
	if err != nil {
		return "", errors.New("applied release has an incompatible image")
	}
	canonical, ok := ref.(reference.Canonical)
	if !ok || canonical.Digest().Algorithm().String() != "sha256" {
		return "", errors.New("applied release requires a sha256 image digest")
	}
	pinned, err := reference.WithDigest(reference.TrimNamed(ref), canonical.Digest())
	if err != nil {
		return "", errors.New("invalid applied image digest")
	}
	return pinned.String(), nil
}

func runPromote(ctx context.Context, c *http.Client, dst *agentclient.Target, cfgPath, from string, f commandFlags, out, errout io.Writer) int {
	fail := func(err error) int { printRefusal(errout, safeSecretText(err.Error(), dst.Token), !f.json); return 1 }
	if len(f.pos) != 1 || from == "" || from == dst.Env {
		fmt.Fprintln(errout, "promotion needs an app and a different source environment")
		return 2
	}
	if f.json && !f.dry {
		fmt.Fprintln(errout, "promotion with --json requires --dry-run; deploy an explicit digest for scripts")
		return 2
	}
	if !f.dry && !interactiveDeploy(f, out) {
		fmt.Fprintln(errout, "deploy confirmation requires terminal input and preview output; run interactively or deploy an explicit digest")
		return 1
	}

	before, err := gitcheck.Capture(ctx, f.dir)
	if err != nil {
		return fail(err)
	}
	loaded, err := loadDeployManifest(f.dir, dst.Env)
	if err != nil {
		return fail(err)
	}
	m := loaded.Manifest
	if !manifestMatchesTarget(errout, f.dir, m.Project, dst.Project) {
		return 2
	}
	if a, ok := m.Apps[f.pos[0]]; !ok || a.Kind != "image" {
		fmt.Fprintln(errout, "promotion requires an image app")
		return 2
	}
	req := deploy.Request{App: f.pos[0], Commit: f.commit, RequestID: f.id, DryRun: true}
	if req.RequestID == "" {
		req.RequestID, err = newRequestID()
		if err != nil {
			return fail(err)
		}
	}
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c = &client
	progress := newProgressLine(errout, !f.json && stderrTerminal(errout))
	progress.Start("checking checkout")
	co, err := agentclient.PrepareCapturedDeployFiles(ctx, f.dir, dst.Env, loaded, []*deploy.Request{&req}, before)
	progress.Close()
	if err != nil {
		return fail(err)
	}
	warnCheckout(errout, f.commit, co)
	progress.Start("checking destination")
	err = refuseLocalChanges(ctx, c, dst, m.Protected, co)
	if err == nil {
		err = agentclient.CheckAgentVersion(ctx, c, dst, *m)
		if err != nil {
			progress.Close()
			printRefusal(errout, terminalSafe(err.Error()), !f.json)
			return 1
		}
	}
	progress.Close()
	if err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	progress.Start("reading applied releases")
	snapshot, err := selectPromotion(ctx, c, dst, cfgPath, from, m, req.App)
	progress.Close()
	if err != nil {
		printRefusal(errout, err.Error(), !f.json)
		return 1
	}
	req.Version, req.VersionTag = snapshot.image, snapshot.tag
	expected := snapshot.expectedID
	req.ExpectedApplied = &expected
	if _, err = m.CheckImageDeploy(req.App, req.Version, m.Protected); err != nil {
		return fail(err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fail(err)
	}
	progress.Start("planning promotion")
	result, err := agentclient.DoResult(ctx, c, dst.Base, dst.Prefix, dst.Token, "POST", "deploys", "", payload)
	data, status := result.Data, result.Status
	progress.Close()
	redact := func(s string) string { return safeSecretText(s, snapshot.source.Token, dst.Token) }
	if err != nil {
		printRefusal(errout, redact(err.Error()), !f.json)
		return 1
	}
	if status != http.StatusOK {
		printAgentRefusal(errout, safeSecretText(agentclient.Message(status, data), snapshot.source.Token, dst.Token), !f.json, result.Older)
		if status == http.StatusBadRequest || status == http.StatusNotFound {
			fmt.Fprintln(errout, "promotion may require a newer agent; upgrade with berthy setup or deploy an explicit digest")
		}
		return 1
	}
	if err := imagePlan(data, m, req, snapshot.image, snapshot.tag); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	heading := fmt.Sprintf("Source: %s release %d\nDestination: %s/%s app %s\nImage: %s\nTag: %s", from, snapshot.sourceID, dst.Project, dst.Env, req.App, req.Version, req.VersionTag)
	if err := showImagePlan(out, data, dst.Token, f.json, heading, snapshot.source.Token); err != nil {
		printRefusal(errout, safeSecretText(err.Error(), dst.Token, snapshot.source.Token), !f.json)
		return 1
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if f.dry {
		return 0
	}
	yes, err := confirmDeploy(ctx, os.Stdin, out)
	if err != nil {
		return fail(err)
	}
	if !yes {
		fmt.Fprintln(errout, "promotion cancelled")
		return 1
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	req.DryRun = false
	f.redactToken = snapshot.source.Token
	return sendCommand(ctx, c, dst.Base, dst.Prefix, dst.Token, "POST", "deploys", req, f, "deploy", out, errout)
}
