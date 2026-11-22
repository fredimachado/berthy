package jobs

import (
	"context"

	"berthy/internal/manifest"
)

// VerifiedRelease identifies the image release already verified by an ongoing deployment.
type VerifiedRelease struct {
	App, Image, Tag string
	Manifest        manifest.Manifest
}

type verifiedReleaseKey struct{}

// WithVerifiedRelease limits a backup hook's commands to the verified release of its own app.
func WithVerifiedRelease(ctx context.Context, release VerifiedRelease) context.Context {
	return context.WithValue(ctx, verifiedReleaseKey{}, release)
}

// VerifiedFromContext returns a release supplied by a post-deploy backup hook.
func VerifiedFromContext(ctx context.Context) (VerifiedRelease, bool) {
	release, ok := ctx.Value(verifiedReleaseKey{}).(VerifiedRelease)
	return release, ok
}
