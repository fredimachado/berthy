package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// OperationEvent is what the agent tells the alert channel and the telemetry backend about an operation.
type OperationEvent struct {
	OperationID, Environment, Kind, App, Actor string
	// Version is the tag asked for, else the short digest. Empty when unknown.
	Version string
	// Digest is the image digest (sha256:...) that runs or would have run. Empty when unknown.
	Digest string
	// State is "running" for a start and the final state for an end.
	State string
	// Reason is the public one-line cause of a failure, and Err the fixed category of it. Both may be empty.
	Reason, Err string
	Started     time.Time
	// Ended is zero for a start.
	Ended time.Time
	// Resumed marks the end of an operation that had already ended as recovery_required: a person or a later recovery
	// decided it. The alert channel was told when it first ended, so it does not hear it again.
	Resumed bool
	// Manifest is the manifest that decides the alert policy, when the controller knows it. Nil: the environment's stored settings.
	Manifest *manifest.Manifest
}

// Observer hears operation starts and ends. A call must return quickly: an observer that sends something does it in the
// background. ctx carries no deadline of the operation.
type Observer interface {
	OperationStarted(ctx context.Context, e OperationEvent)
	OperationEnded(ctx context.Context, e OperationEvent)
}

// target is what an operation acts on, as far as the controller knows it when the operation ends. Manifest decides the alert
// policy, Image is the image the plan pinned and Tag the tag asked for. Every field may be empty.
type target struct {
	Manifest   *manifest.Manifest
	Image, Tag string
}

// finish records an operation's outcome and then tells the observer. Every terminal write of an operation goes through it,
// so what the observer hears is what the state holds. It tells nobody when the write fails.
func (c *Controller) finish(ctx context.Context, op state.Operation, o state.Outcome, t target) error {
	if err := c.Store.Finish(ctx, op.ID, o); err != nil {
		return err
	}
	c.reportEnd(ctx, op, o, t)
	return nil
}

// reportEnd tells the observer that an operation has ended as o says. op is the operation as it was before the end.
func (c *Controller) reportEnd(ctx context.Context, op state.Operation, o state.Outcome, t target) {
	if c.Observer == nil {
		return
	}
	tag, digest := t.Tag, imageDigest(t.Image)
	if o.Release != nil {
		if o.Release.Tag != "" {
			tag = o.Release.Tag
		}
		if d := imageDigest(o.Release.Digest); d != "" {
			digest = d
		}
	}
	if tag == "" && digest == "" {
		var req struct {
			Version string `json:"version"`
		}
		_ = json.Unmarshal(op.PublicRequest, &req)
		tag, digest = versionParts(req.Version)
	}
	e := c.event(op, o.State, tag, digest)
	e.Reason, e.Err = o.Reason, o.Err
	e.Ended = time.Now()
	e.Resumed = op.State == "recovery_required"
	e.Manifest = t.Manifest
	c.Observer.OperationEnded(context.WithoutCancel(ctx), e)
}

// started tells the observer that an operation was admitted. req is the request that was admitted.
func (c *Controller) started(ctx context.Context, op state.Operation, req Request) {
	if c.Observer == nil {
		return
	}
	tag, digest := versionParts(req.Version)
	if req.recordTag != "" {
		tag = req.recordTag
	}
	c.Observer.OperationStarted(context.WithoutCancel(ctx), c.event(op, "running", tag, digest))
}

// event builds the event of an operation that has this tag and digest.
func (c *Controller) event(op state.Operation, st, tag, digest string) OperationEvent {
	version := tag
	if version == "" {
		version = ShortDigest(digest)
	}
	actor := op.ActorName
	if actor == "" {
		actor = op.ActorID
	}
	return OperationEvent{OperationID: op.ID, Environment: c.Environment, Kind: op.Kind, App: op.App, Actor: actor, Version: version, Digest: digest, State: st, Started: op.Started}
}

// digestOf returns the sha256 digest of an image reference, or of a bare digest. Anything without one gives "".
func imageDigest(ref string) string {
	if _, d, found := strings.Cut(ref, "@"); found {
		return d
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ref
	}
	return ""
}

// versionParts splits the version of a request, which is a tag, a digest, or an image reference with either, into its tag and
// its digest. Either may be empty.
func versionParts(v string) (tag, digest string) {
	switch digest = imageDigest(v); {
	case v == "":
	case !strings.ContainsAny(v, "/@:"):
		tag = v
	case strings.Contains(v, "/") || strings.Contains(v, "@"):
		tag = requestedTag(v)
	}
	return tag, digest
}
