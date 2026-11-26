package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/manifest"

	"github.com/distribution/reference"
)

func runVersions(ctx context.Context, c *http.Client, t *agentclient.Target, f commandFlags, out, errout io.Writer) int {
	m, err := manifest.Load(f.dir, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if !manifestMatchesTarget(errout, f.dir, m.Project, t.Project) {
		return 2
	}
	app, ok := m.Apps[f.pos[0]]
	if !ok || app.Kind != "image" {
		fmt.Fprintf(errout, "app %q is not an image app\n", f.pos[0])
		return 2
	}
	repo, err := reference.ParseNormalizedNamed(app.Image)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	progress := startWait(errout, !f.json, "reading versions")
	err = agentclient.CheckAgentVersion(ctx, c, t, *m)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, t.Redact(err.Error()))
		return 1
	}
	body, err := json.Marshal(api.ImageLookup{Manifest: *m, App: f.pos[0]})
	if err != nil {
		fmt.Fprintln(errout, "invalid manifest")
		return 1
	}
	progress = startWait(errout, !f.json, "reading versions")
	data, status, err := agentclient.Do(ctx, c, t.Base, t.Prefix, t.Token, "POST", "images/tags", "", body)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, t.Redact(err.Error()))
		return 1
	}
	switch status {
	case http.StatusForbidden:
		fmt.Fprintln(errout, "listing image tags requires a human administrator token")
		return 1
	case http.StatusNotFound:
		fmt.Fprintln(errout, "image tag listing is unavailable; upgrade the agent with berthy setup")
		return 1
	case http.StatusOK:
	default:
		fmt.Fprintln(errout, t.Redact(agentclient.Message(status, data)))
		return 1
	}
	result, valid := decodeImageTags(data)
	if !valid || result.Repository != reference.TrimNamed(repo).String() {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	seen := make(map[string]bool, len(result.Tags))
	for _, tag := range result.Tags {
		match := reference.TagRegexp.FindString(tag)
		if tag == "" || match != tag || seen[tag] {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		seen[tag] = true
	}
	if f.json {
		if err := json.NewEncoder(out).Encode(result); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	} else {
		for _, tag := range result.Tags {
			if _, err := fmt.Fprintln(out, tag); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
	}
	return 0
}

// decodeImageTags requires one exact spelling of each protocol field before using the response.
func decodeImageTags(data []byte) (api.ImageTags, bool) {
	var result api.ImageTags
	if !utf8.Valid(data) {
		return result, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return result, false
	}
	var repository *string
	var tags *[]string
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return result, false
		}
		key, ok := token.(string)
		if !ok {
			return result, false
		}
		switch {
		case strings.EqualFold(key, "repository"):
			if key != "repository" || repository != nil {
				return result, false
			}
			if dec.Decode(&repository) != nil || repository == nil {
				return result, false
			}
		case strings.EqualFold(key, "tags"):
			if key != "tags" || tags != nil {
				return result, false
			}
			if dec.Decode(&tags) != nil || tags == nil || *tags == nil {
				return result, false
			}
		default:
			var ignored json.RawMessage
			if dec.Decode(&ignored) != nil {
				return result, false
			}
		}
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') || repository == nil || tags == nil {
		return result, false
	}
	if _, err = dec.Token(); err != io.EOF {
		return result, false
	}
	result.Repository, result.Tags = *repository, *tags
	return result, true
}
