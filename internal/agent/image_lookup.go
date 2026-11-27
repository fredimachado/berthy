package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"encoding/json"
	"net/http"
)

// imageTagsLookup is an image lookup without a tag; tags rejects that field.
type imageTagsLookup struct {
	Manifest manifest.Manifest `json:"manifest"`
	App      string            `json:"app"`
}

func serveImageLookup(w http.ResponseWriter, r *http.Request, d *deploy.Controller, action string) {
	var q api.ImageLookup
	if action == "tags" {
		var tags imageTagsLookup
		if decodeBody(w, r, &tags) != nil {
			writeError(w, 400, "bad_request", "invalid request")
			return
		}
		q = api.ImageLookup{Manifest: tags.Manifest, App: tags.App}
	} else if decodeBody(w, r, &q) != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if action == "resolve" {
		result, err := d.ResolveTag(r.Context(), q)
		if err != nil {
			writeError(w, 400, "lookup_failed", "image lookup failed")
			return
		}
		json.NewEncoder(w).Encode(result)
		return
	}
	result, err := d.ImageTags(r.Context(), q)
	if err != nil {
		writeError(w, 400, "lookup_failed", "image lookup failed")
		return
	}
	json.NewEncoder(w).Encode(result)
}
