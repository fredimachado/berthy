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
	"strings"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/manifest"
	"github.com/distribution/reference"
)

type promotionSnapshot struct {
	source, destination  *agentclient.Target
	app, image, tag      string
	sourceID, expectedID int64
}

// readApplied accepts absence only from a complete, unambiguous not_deployed response.
func readApplied(ctx context.Context, c *http.Client, t *agentclient.Target, app string) (appliedRelease, bool, error) {
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
		return result, false, errors.New(t.Redact(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return result, false, agentclient.ErrInvalidResponse
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return result, false, errors.New(t.Redact(agentclient.Message(resp.StatusCode, data)))
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
		return result, false, errors.New(t.Redact(agentclient.Message(resp.StatusCode, data)))
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
		return snapshot, err
	}
	source, found, err := readApplied(ctx, c, src, app)
	if err != nil {
		return snapshot, fmt.Errorf("source: %s", dst.Redact(err.Error()))
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
	dest, found, err := readApplied(ctx, c, dst, app)
	if err != nil {
		return snapshot, fmt.Errorf("destination: %s", src.Redact(err.Error()))
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
