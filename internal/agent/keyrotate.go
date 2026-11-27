package agent

import (
	"encoding/json"
	"errors"

	"berthy/internal/api"
	"berthy/internal/state"
)

// serveKeyRotate replaces the key the state is sealed with, or with dry_run reports what it would do. The answer names the
// keys by ID and never shows one.
func (rt *routeRuntime) serveKeyRotate(call routeCall, read readRouteBody[dryRunBody]) {
	w := call.w
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	rep, err := rt.d.Store.RotateKey(call.r.Context(), call.stateActor(), req.DryRun)
	switch {
	case errors.Is(err, state.ErrConflict):
		writeError(w, 409, "conflict", "the state key cannot be rotated while an operation runs or the deploy lock is held")
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
