package deploy

import (
	"context"
	"errors"

	"berthy/internal/state"
)

// Refusal is a planning error whose text is safe to show to API clients.
type Refusal string

func (r Refusal) Error() string { return string(r) }

const (
	errNoMigration      Refusal = "no recorded migration to revert"
	errSourceMissing    Refusal = "migration source unavailable"
	errNoRevertCommand  Refusal = "schema revert command unavailable"
	errSourceImage      Refusal = "source image unavailable"
	errSourceNotPinned  Refusal = "source image is not pinned"
	errTargetMissing    Refusal = "schema revert target unavailable"
	errNoEarlierRelease Refusal = "rollback release unavailable"
	errNewerRelease     Refusal = "a newer release followed the migration; use --to"
	errBadRevertCommand Refusal = "invalid schema revert command"
	errAdminRequired    Refusal = "schema revert requires administrator access"
	errNotRecovering    Refusal = "operation is not waiting for recovery"
	errResolveAdmin     Refusal = "resolving recovery requires administrator access"

	errReplaceAdmin          Refusal = "replacing existing routes or records requires administrator access"
	errCloudflareSecret      Refusal = "Cloudflare API token secret unavailable"
	errCloudflareToken       Refusal = "Cloudflare token check failed"
	errCloudflareUnavailable Refusal = "Cloudflare unavailable"
	errTunnelNotRemote       Refusal = "tunnel is not remotely managed"
	errIngressConflict       Refusal = "ingress hostname conflict"
	errIngressPublish        Refusal = "ingress publish failed"
	errIngressNotConfigured  Refusal = "managed ingress is not configured"
	errTunnelToken           Refusal = "tunnel token unavailable"
	errTunnelTokenRemoval    Refusal = "tunnel token removal failed"
	errConnectorRemoval      Refusal = "connector removal failed"
	errIngressState          Refusal = "ingress state recording failed"
	errIngressNotApplied     Refusal = "managed ingress is not applied; run ingress apply"
	errIngressChanged        Refusal = "managed ingress changed; run ingress apply"
)

// PublicMessage reports whether s is a refusal text that may be shown to clients.
func PublicMessage(s string) bool {
	switch Refusal(s) {
	case errNoMigration, errSourceMissing, errNoRevertCommand, errSourceImage, errSourceNotPinned, errTargetMissing, errNoEarlierRelease, errNewerRelease, errBadRevertCommand, errAdminRequired, errNotRecovering, errResolveAdmin,
		errReplaceAdmin, errCloudflareSecret, errCloudflareToken, errCloudflareUnavailable, errTunnelNotRemote, errIngressConflict, errIngressPublish, errIngressNotConfigured, errTunnelToken, errTunnelTokenRemoval, errConnectorRemoval, errIngressState,
		errIngressNotApplied, errIngressChanged,
		errAllowRecreateAdmin, errComposeFile, errComposeNotCompose, errComposeUnavailable, errComposeFiles, errComposeConfig, errComposePlan, errComposeJournal, errComposeUp, errComposeRestart, errComposeFailed,
		errComposeNoFiles, errComposeSourceMissing, errComposeInterrupted, errComposeEnv, errComposeSecret, errScaleCompose, errExecCompose, errComposeRole:
		return true
	}
	return composeMessage.MatchString(s)
}

var errRevertMismatch = errors.New("schema revert did not reach target")

// runSchemaRevert runs the migration's revert from its own image, after the service is rolled back to the release before it when needed.
func (c *Controller) runSchemaRevert(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	sr, err := c.schemaRevertPlan(ctx, req)
	if err != nil {
		var r Refusal
		if errors.As(err, &r) {
			return state.Outcome{State: "failed", Err: string(r)}
		}
		return state.Outcome{State: "failed", Err: "schema revert unavailable"}
	}
	// The revert image may have been pruned since the migration ran; check it before the service changes.
	if _, msg, err := c.ensureImage(ctx, sr.Prepared); err != nil {
		return state.Outcome{State: "failed", Err: msg}
	}
	out := state.Outcome{State: "succeeded"}
	if sr.Image != nil {
		imgReq := req
		imgReq.RevertSchema = false
		imgReq.TargetRelease = sr.Image.ID
		resolved, err := c.rollbackRequest(ctx, imgReq)
		if err != nil {
			return state.Outcome{State: "failed", Err: "rollback release unavailable"}
		}
		resolved.Kind = "deploy"
		// The older code must serve on the newer schema before the schema moves back.
		out = c.runDeploy(ctx, op, actor, resolved, true)
		if out.State != "succeeded" {
			return out
		}
	}
	before, after, err := c.runRevert(ctx, op.ID, sr)
	out.SchemaBefore, out.SchemaAfter = before, after
	switch {
	case err == nil:
	case errors.Is(err, ErrOneOffUncertain):
		// Recovery owns the rest: it stops any revert container still running, then finishes the operation.
		return state.Outcome{State: "recovery_required", Err: "schema revert outcome uncertain", SchemaBefore: before, SchemaAfter: after, Reason: c.recordOneOffFailure(ctx, op.ID, err)}
	case errors.Is(err, errRevertMismatch):
		out.State, out.Err = "failed", errRevertMismatch.Error()
	default:
		out.State, out.Err = "failed", "schema revert failed"
		out.Reason = c.recordOneOffFailure(ctx, op.ID, err)
	}
	return out
}

// runRevert records the schema state, runs the revert from the migration's image, and records the state again.
func (c *Controller) runRevert(ctx context.Context, opID string, sr schemaRevert) (string, string, error) {
	before, err := c.runOneOff(ctx, opID, sr.Prepared, 30000, "revert_before", sr.Version, true)
	if err != nil {
		return "", "", err
	}
	_, revertErr := c.runOneOff(ctx, opID, sr.Prepared, 30001, "migration_revert", sr.Revert, false)
	if errors.Is(revertErr, ErrOneOffUncertain) {
		return before, "", revertErr
	}
	after, afterErr := c.runOneOff(ctx, opID, sr.Prepared, 30002, "revert_after", sr.Version, true)
	if revertErr != nil {
		return before, after, revertErr
	}
	if afterErr != nil {
		return before, "", afterErr
	}
	if after != sr.Target {
		return before, after, errRevertMismatch
	}
	return before, after, nil
}
