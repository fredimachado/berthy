package agent

import (
	"encoding/json"
	"errors"
	"net/http"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/state"
)

// isKeyRotation reports whether rest is the route that replaces the state key.
func isKeyRotation(rest []string) bool {
	return len(rest) == 2 && rest[0] == "key" && rest[1] == "rotate"
}

// serveKeyRotate replaces the key the state is sealed with, or with dry_run reports what it would do. The answer names the
// keys by ID and never shows one.
func serveKeyRotate(w http.ResponseWriter, r *http.Request, d *deploy.Controller, actor state.Actor) {
	var req struct {
		DryRun bool `json:"dry_run"`
	}
	if decodeBody(w, r, &req) != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	rep, err := d.Store.RotateKey(r.Context(), actor, req.DryRun)
	switch {
	case errors.Is(err, state.ErrConflict):
		writeError(w, 409, "conflict", "environment or request conflict")
		return
	case errors.Is(err, state.ErrKeyFiles):
		writeError(w, 500, "key_rotation_incomplete", state.ErrKeyFiles.Error())
		return
	case err != nil:
		writeError(w, 500, "key_rotation_failed", "key rotation failed")
		return
	}
	json.NewEncoder(w).Encode(api.KeyRotation{OldKeyID: rep.OldKeyID, NewKeyID: rep.NewKeyID, Values: rep.Values, KeyFile: rep.KeyFile, DryRun: req.DryRun})
}
