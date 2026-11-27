package agent

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/state"
)

// generateAlphabet is the alphabet of an alnum secret: the 26 capital letters, the 26 small ones and the ten digits.
const generateAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var secretRandom io.Reader = rand.Reader

// generateSecret returns a secret of n units in format, taking its random bytes from r. hex is n bytes in hex, so 2n characters.
// base64 is n bytes in URL-safe base64 without padding. alnum is n characters from generateAlphabet. An alnum character takes
// one byte of r, and a byte of 248 or more is skipped: 248 is 4 times 62, so the bytes that are kept give every character the same
// chance. It returns the error of r when r fails.
func generateSecret(r io.Reader, n int, format string) (string, error) {
	switch format {
	case "hex", "base64":
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if format == "hex" {
			return hex.EncodeToString(b), nil
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	case "alnum":
		const keep = 248 // 4 * len(generateAlphabet)
		out := make([]byte, 0, n)
		for len(out) < n {
			// Each round reads as many bytes as characters are still missing. A skipped byte costs one more round.
			buf := make([]byte, n-len(out))
			if _, err := io.ReadFull(r, buf); err != nil {
				return "", err
			}
			for _, v := range buf {
				if v < keep {
					out = append(out, generateAlphabet[v%byte(len(generateAlphabet))])
				}
			}
		}
		return string(out), nil
	}
	return "", errors.New("unknown format")
}

func (rt *routeRuntime) handleSecretGenerate(call routeCall, read readRouteBody[api.SecretGenerate]) {
	serveSecretGenerate(call.w, call.r, rt.d, call.stateActor(), read)
}

// serveSecretGenerate answers POST secrets/generate. The agent makes the value from crypto/rand and stores it, so the value never
// leaves the agent: the answer and the dry run name the secret, its length and its format, and never hold a value. A stored
// secret is refused unless the request replaces it, and a registry password is refused always.
func serveSecretGenerate(w http.ResponseWriter, r *http.Request, d *deploy.Controller, sa state.Actor, read readRouteBody[api.SecretGenerate]) {
	ctx := r.Context()
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	n, format, err := api.CheckSecretGenerate(req)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	_, replace, err := d.Store.ClassifySecrets(ctx, []string{req.Name})
	if err != nil {
		secretGenerateError(w, req.Name, err)
		return
	}
	plan := api.SecretGeneratePlan{Action: "secret generate", Name: req.Name, Exists: len(replace) == 1, Bytes: n, Format: format}
	if !req.Replace && !plan.Exists {
		var sub *deploy.Submitted
		if req.Manifest != nil {
			sub = &deploy.Submitted{Manifest: *req.Manifest, ComposeFiles: req.ComposeFiles}
			if err := d.CheckSubmitted(ctx, *sub); err != nil {
				operationError(w, err)
				return
			}
		}
		finding, err := d.GuardSecret(ctx, req.Name, sub)
		if err != nil {
			operationError(w, err)
			return
		}
		if finding != nil {
			plan.App, plan.Volume = finding.App, finding.Volume
			if !req.DryRun {
				message := "app " + finding.App + " was deployed here"
				if finding.Volume != "" {
					message = "the volume " + finding.Volume + " of app " + finding.App + " exists"
				}
				writeError(w, 409, "conflict", message+" and may hold the value of "+req.Name+"; set it with berthy secret set, or pass --replace")
				return
			}
		}
	}
	if req.DryRun {
		json.NewEncoder(w).Encode(plan) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	value, err := generateSecret(secretRandom, n, format)
	if err != nil {
		writeError(w, 503, "unavailable", "secret unavailable")
		return
	}
	replaced, err := d.Store.GenerateSecret(ctx, sa, req.Name, req.Replace, []byte(value))
	if err != nil {
		secretGenerateError(w, req.Name, err)
		return
	}
	json.NewEncoder(w).Encode(api.SecretGenerated{Name: req.Name, Bytes: n, Format: format, Replaced: replaced}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleSecretsGenerateDeclared(call routeCall, read readRouteBody[api.SecretsGenerateDeclared]) {
	serveSecretsGenerateDeclared(call.w, call.r, rt.d, call.stateActor(), read)
}

func serveSecretsGenerateDeclared(w http.ResponseWriter, r *http.Request, d *deploy.Controller, sa state.Actor, read readRouteBody[api.SecretsGenerateDeclared]) {
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	plan, err := d.DeclaredSecrets(r.Context(), deploy.Submitted{Manifest: req.Manifest, ComposeFiles: req.ComposeFiles})
	if err != nil {
		operationError(w, err)
		return
	}
	if !req.DryRun {
		for i := range plan {
			item := &plan[i]
			if item.Outcome != api.DeclaredGenerate {
				continue
			}
			value, err := generateSecret(secretRandom, item.Bytes, item.Format)
			if err != nil {
				writeError(w, 503, "unavailable", "secret unavailable")
				return
			}
			_, err = d.Store.GenerateSecret(r.Context(), sa, item.Name, false, []byte(value))
			if errors.Is(err, state.ErrSecretExists) {
				item.Outcome = api.DeclaredKeep
			} else if err != nil {
				operationError(w, err)
				return
			}
		}
	}
	json.NewEncoder(w).Encode(api.SecretsDeclared{Action: "secret generate declared", Secrets: plan}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

// secretGenerateError answers a failed generate. A registry password is refused with what the name is; a stored secret that the
// request does not replace is a conflict. Any other failure is answered as operationError answers it.
func secretGenerateError(w http.ResponseWriter, name string, err error) {
	var rs state.RegistrySecretError
	switch {
	case errors.As(err, &rs):
		writeError(w, 400, "bad_request", name+" is a registry password and cannot be generated")
	case errors.Is(err, state.ErrSecretExists):
		writeError(w, 409, "conflict", "secret "+name+" exists; --replace replaces it")
	default:
		operationError(w, err)
	}
}
