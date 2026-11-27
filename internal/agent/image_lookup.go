package agent

import (
	"encoding/json"

	"berthy/internal/api"
	"berthy/internal/manifest"
)

// imageTagsLookup is an image lookup without a tag; tags rejects that field.
type imageTagsLookup struct {
	Manifest manifest.Manifest `json:"manifest"`
	App      string            `json:"app"`
}

func (rt *routeRuntime) handleImageResolve(call routeCall, read readRouteBody[api.ImageLookup]) {
	w := call.w
	q, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	result, err := rt.d.ResolveTag(call.r.Context(), q)
	if err != nil {
		writeError(w, 400, "lookup_failed", "image lookup failed")
		return
	}
	json.NewEncoder(w).Encode(result) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleImageTags(call routeCall, read readRouteBody[imageTagsLookup]) {
	w := call.w
	tags, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	q := api.ImageLookup{Manifest: tags.Manifest, App: tags.App}
	result, err := rt.d.ImageTags(call.r.Context(), q)
	if err != nil {
		writeError(w, 400, "lookup_failed", "image lookup failed")
		return
	}
	json.NewEncoder(w).Encode(result) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}
