package deploy

import (
	"berthy/internal/api"
	"berthy/internal/registrycheck"
	"context"
	"errors"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

func (c *Controller) lookupImage(ctx context.Context, q api.ImageLookup) (reference.Named, registrycheck.Probe, error) {
	if err := c.CheckSubmitted(ctx, Submitted{Manifest: q.Manifest}); err != nil {
		return nil, registrycheck.Probe{}, err
	}
	app, ok := q.Manifest.Apps[q.App]
	if !ok || app.Kind != "image" {
		return nil, registrycheck.Probe{}, errors.New("image app required")
	}
	named, err := reference.ParseNormalizedNamed(app.Image)
	if err != nil || !reference.IsNameOnly(named) {
		return nil, registrycheck.Probe{}, errors.New("invalid image repository")
	}
	auth, err := c.registryAuth(ctx, q.Manifest, reference.Domain(named))
	if err != nil {
		return nil, registrycheck.Probe{}, err
	}
	p := registrycheck.Probe{Host: reference.Domain(named)}
	if auth != nil {
		p.Username, p.Password = auth.Username, auth.Password
	}
	return named, p, nil
}
func (c *Controller) ResolveTag(ctx context.Context, q api.ImageLookup) (api.ResolvedImage, error) {
	named, probe, err := c.lookupImage(ctx, q)
	if err != nil {
		return api.ResolvedImage{}, err
	}
	d, err := probe.ResolveTag(ctx, reference.Path(named), q.Tag)
	if err != nil {
		return api.ResolvedImage{}, errors.New("image resolution failed")
	}
	pinned, err := reference.WithDigest(named, digest.Digest(d))
	if err != nil {
		return api.ResolvedImage{}, errors.New("invalid resolved image digest")
	}
	return api.ResolvedImage{Image: pinned.String(), Tag: q.Tag}, nil
}
func (c *Controller) ImageTags(ctx context.Context, q api.ImageLookup) (api.ImageTags, error) {
	if q.Tag != "" {
		return api.ImageTags{}, errors.New("tag not allowed")
	}
	named, probe, err := c.lookupImage(ctx, q)
	if err != nil {
		return api.ImageTags{}, err
	}
	tags, err := probe.Tags(ctx, reference.Path(named))
	if err != nil {
		return api.ImageTags{}, errors.New("registry tag lookup failed")
	}
	return api.ImageTags{Repository: named.String(), Tags: tags}, nil
}
