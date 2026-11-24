package deploy

import (
	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/registrycheck"
	"context"
	"errors"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

func (c *Controller) lookupImage(ctx context.Context, q api.ImageLookup) (reference.Named, *engine.RegistryAuth, error) {
	if err := c.CheckSubmitted(ctx, Submitted{Manifest: q.Manifest}); err != nil {
		return nil, nil, err
	}
	app, ok := q.Manifest.Apps[q.App]
	if !ok || app.Kind != "image" {
		return nil, nil, errors.New("image app required")
	}
	named, err := reference.ParseNormalizedNamed(app.Image)
	if err != nil || !reference.IsNameOnly(named) {
		return nil, nil, errors.New("invalid image repository")
	}
	auth, err := c.registryAuth(ctx, q.Manifest, reference.Domain(named))
	return named, auth, err
}
func (c *Controller) ResolveTag(ctx context.Context, q api.ImageLookup) (api.ResolvedImage, error) {
	named, auth, err := c.lookupImage(ctx, q)
	if err != nil {
		return api.ResolvedImage{}, err
	}
	tagged, err := reference.WithTag(named, q.Tag)
	if err != nil || q.Tag == "" {
		return api.ResolvedImage{}, errors.New("invalid image tag")
	}
	if c.Engine == nil {
		return api.ResolvedImage{}, errors.New("image resolution unavailable")
	}
	d, err := c.Engine.ResolveImage(ctx, tagged.String(), auth)
	if err != nil {
		return api.ResolvedImage{}, errors.New("image resolution failed")
	}
	parsed, err := digest.Parse(d)
	if err != nil || parsed.Algorithm() != digest.SHA256 {
		return api.ResolvedImage{}, errors.New("invalid resolved image digest")
	}
	pinned, err := reference.WithDigest(named, parsed)
	if err != nil {
		return api.ResolvedImage{}, errors.New("invalid resolved image digest")
	}
	return api.ResolvedImage{Image: pinned.String(), Tag: q.Tag}, nil
}
func (c *Controller) ImageTags(ctx context.Context, q api.ImageLookup) (api.ImageTags, error) {
	if q.Tag != "" {
		return api.ImageTags{}, errors.New("tag not allowed")
	}
	named, auth, err := c.lookupImage(ctx, q)
	if err != nil {
		return api.ImageTags{}, err
	}
	p := registrycheck.Probe{Host: reference.Domain(named)}
	if auth != nil {
		p.Username = auth.Username
		p.Password = auth.Password
	}
	tags, err := p.Tags(ctx, reference.Path(named))
	if err != nil {
		return api.ImageTags{}, errors.New("registry tag lookup failed")
	}
	return api.ImageTags{Repository: named.String(), Tags: tags}, nil
}
