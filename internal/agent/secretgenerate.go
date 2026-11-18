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

// serveSecretGenerate answers POST secrets/generate. The agent makes the value from crypto/rand and stores it, so the value never
// leaves the agent: the answer and the dry run name the secret, its length and its format, and never hold a value. A stored
// secret is refused unless the request replaces it, and a registry password is refused always.
func serveSecretGenerate(w http.ResponseWriter, r *http.Request, d *deploy.Controller, sa state.Actor) {
	ctx := r.Context()
	var req api.SecretGenerate
	if decodeBody(w, r, &req) != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	n, format, err := api.CheckSecretGenerate(req)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if req.DryRun {
		_, replace, err := d.Store.ClassifySecrets(ctx, []string{req.Name})
		if err != nil {
			secretGenerateError(w, req.Name, err)
			return
		}
		json.NewEncoder(w).Encode(api.SecretGeneratePlan{Action: "secret generate", Name: req.Name, Exists: len(replace) == 1, Bytes: n, Format: format})
		return
	}
	value, err := generateSecret(rand.Reader, n, format)
	if err != nil {
		writeError(w, 503, "unavailable", "secret unavailable")
		return
	}
	replaced, err := d.Store.GenerateSecret(ctx, sa, req.Name, req.Replace, []byte(value))
	if err != nil {
		secretGenerateError(w, req.Name, err)
		return
	}
	json.NewEncoder(w).Encode(api.SecretGenerated{Name: req.Name, Bytes: n, Format: format, Replaced: replaced})
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
