package deploy

import (
	"berthy/internal/api"
	"berthy/internal/manifest"
	"context"
)

// CheckSubmitted validates a manifest against the receiving environment.
func (c *Controller) CheckSubmitted(ctx context.Context, sub Submitted) error {
	if err := checkRequestManifest(sub.Manifest); err != nil {
		return err
	}
	if err := sub.Manifest.Validate(); err != nil {
		return err
	}
	return c.checkProject(ctx, Request{Manifest: sub.Manifest})
}

// DeclaredSecrets decides the outcomes without writing secrets.
func (c *Controller) DeclaredSecrets(ctx context.Context, sub Submitted) ([]api.DeclaredSecretOutcome, error) {
	if err := c.CheckSubmitted(ctx, sub); err != nil {
		return nil, err
	}
	secrets, err := c.Store.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	stored := map[string]bool{}
	for _, s := range secrets {
		stored[s.Name] = true
	}
	found := map[string]GuardFinding{}
	for _, name := range sortedKeys(sub.Manifest.Secrets) {
		if stored[name] {
			continue
		}
		finding, err := c.GuardSecret(ctx, name, &sub)
		if err != nil {
			return nil, err
		}
		if finding != nil {
			found[name] = *finding
		}
	}
	return decideDeclared(sub.Manifest, stored, found), nil
}
func decideDeclared(m manifest.Manifest, stored map[string]bool, found map[string]GuardFinding) []api.DeclaredSecretOutcome {
	out := make([]api.DeclaredSecretOutcome, 0, len(m.Secrets))
	for _, name := range sortedKeys(m.Secrets) {
		rule := m.Secrets[name].Generate
		item := api.DeclaredSecretOutcome{Name: name, Outcome: api.DeclaredGenerate, Bytes: rule.Length(), Format: rule.Format}
		if stored[name] {
			item.Outcome = api.DeclaredKeep
		} else if finding, ok := found[name]; ok {
			item.Outcome = api.DeclaredRefuse
			item.App = finding.App
			item.Volume = finding.Volume
		}
		out = append(out, item)
	}
	return out
}
