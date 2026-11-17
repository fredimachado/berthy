package agent

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/url"
	"slices"
	"sync"
	"time"

	"berthy/internal/api"
	"berthy/internal/cloudflare"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/registrycheck"
	"berthy/internal/state"
)

// credentialVerdict is what asking a service about a stored credential found.
type credentialVerdict int

const (
	credentialAccepted credentialVerdict = iota + 1 // the service answered and accepts the credential
	credentialRefused                               // the service answered and refuses it
	credentialUnasked                               // the service could not be asked, so nothing is known
	credentialMissing                               // the secret is not stored (or is stored for something else)
)

// credentialAnswer is one answer about one credential. It never holds the credential.
type credentialAnswer struct {
	verdict credentialVerdict
	reason  string    // refused: what the service refused; unasked: why it could not be asked; empty otherwise
	expires time.Time // accepted only: when the service says the credential stops working; zero when it does not say
}

// credentialProbe is one credential the applied configuration needs, and the way to ask about it.
type credentialProbe struct {
	check    string // the check's name: "registry:<host>" or "cloudflare_token"
	secret   string // the secret that holds the credential
	registry string // the registry host of a registry password; empty for any other credential
	ask      func(context.Context) credentialAnswer
}

const (
	// credentialInterval is how often the monitor asks about a credential, and credentialFailures how many answers in a
	// row that are not ok change its stored state: one wrong refusal during a service's incident must not raise an alert.
	credentialInterval = 10 * time.Minute
	credentialFailures = 2
	// credentialExpiryWarning is how long before a credential's expiry its check starts to warn.
	credentialExpiryWarning = 14 * 24 * time.Hour
	// credentialDoctorTimeout bounds the doctor's questions about all credentials, which are asked at once.
	credentialDoctorTimeout = 5 * time.Second
)

// credentialProbes lists the credentials to check: the password of every registry of m that a deployed image app pulls
// from, sorted by host, then the Cloudflare API token when managed ingress is configured. m is the deployed manifest, or nil
// when there is none; ingress is the stored ingress configuration, or nil when managed ingress is not configured.
func credentialProbes(c Config, s *state.Store, m *manifest.Manifest, ingress *state.IngressState) []credentialProbe {
	var out []credentialProbe
	if m != nil {
		for _, host := range slices.Sorted(maps.Keys(m.Registries)) {
			reg := m.Registries[host]
			repos := m.ImageRepos(host)
			if len(repos) == 0 {
				continue
			}
			out = append(out, credentialProbe{
				check:    "registry:" + host,
				secret:   reg.PasswordSecret,
				registry: host,
				ask:      func(ctx context.Context) credentialAnswer { return askRegistry(ctx, s, host, reg, repos) },
			})
		}
	}
	if ingress != nil {
		in := *ingress
		out = append(out, credentialProbe{
			check:  "cloudflare_token",
			secret: in.APITokenSecret,
			ask:    func(ctx context.Context) credentialAnswer { return askCloudflare(ctx, c, s, in) },
		})
	}
	return out
}

// askRegistry asks registry host whether the stored password can pull every repository in repos.
func askRegistry(ctx context.Context, s *state.Store, host string, reg manifest.Registry, repos []string) credentialAnswer {
	info, value, err := s.Secret(ctx, reg.PasswordSecret)
	if errors.Is(err, sql.ErrNoRows) || err == nil && info.Registry != host {
		return credentialAnswer{verdict: credentialMissing}
	}
	if err != nil {
		return credentialAnswer{verdict: credentialUnasked, reason: "secret unavailable"}
	}
	probe := registrycheck.Probe{Host: host, Username: reg.Username, Password: string(value)}
	var failure string
	for _, repo := range repos {
		err := probe.CanPull(ctx, repo)
		switch {
		case err == nil:
		case errors.Is(err, registrycheck.ErrDenied):
			return credentialAnswer{verdict: credentialRefused, reason: host + " does not let it pull " + repo}
		case failure == "":
			failure = askFailure(err)
		}
	}
	if failure != "" {
		return credentialAnswer{verdict: credentialUnasked, reason: failure}
	}
	return credentialAnswer{verdict: credentialAccepted}
}

// askCloudflare asks Cloudflare about the API token of managed ingress.
func askCloudflare(ctx context.Context, c Config, s *state.Store, ingress state.IngressState) credentialAnswer {
	info, value, err := s.Secret(ctx, ingress.APITokenSecret)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (info.Registry != "" || len(value) == 0) {
		return credentialAnswer{verdict: credentialMissing}
	}
	if err != nil {
		return credentialAnswer{verdict: credentialUnasked, reason: "secret unavailable"}
	}
	cf := &cloudflare.Client{BaseURL: c.CloudflareAPIURL, Token: string(value)}
	st, err := cf.TokenStatus(ctx, ingress.AccountID)
	switch {
	case err == nil && st.Status == "active":
		return credentialAnswer{verdict: credentialAccepted, expires: st.Expires}
	case err == nil && (st.Status == "expired" || st.Status == "disabled"):
		return credentialAnswer{verdict: credentialRefused, reason: "Cloudflare reports the token as " + st.Status}
	case err == nil:
		// An unknown status string is never echoed.
		return credentialAnswer{verdict: credentialRefused, reason: "Cloudflare reports the token as not active"}
	case cloudflare.Refused(err):
		return credentialAnswer{verdict: credentialRefused, reason: "Cloudflare does not accept the token: it is wrong, expired or disabled"}
	}
	return credentialAnswer{verdict: credentialUnasked, reason: askFailure(err)}
}

// askFailure says briefly why a service could not be asked. A transport failure is reduced to "timeout", "connection
// refused" or "unreachable", because its text carries the whole URL. Any other error comes from Berthy's own clients, whose
// errors never hold a credential, and is given as it is.
func askFailure(err error) string {
	var transport *url.Error
	if errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return monitor.FailureDetail(err)
	}
	return err.Error()
}

// credentialResult turns an answer into the check's state and detail. It is the one place that decides what the doctor and
// an alert say about a credential. env is the environment's name, for the command the detail names.
func credentialResult(p credentialProbe, a credentialAnswer, env string, now time.Time) monitor.Result {
	command := "berthy secret set " + p.secret + " -e " + env
	if p.registry != "" {
		command = "berthy secret set " + p.secret + " --registry " + p.registry + " -e " + env
	}
	switch a.verdict {
	case credentialAccepted:
		switch {
		case a.expires.IsZero():
			return monitor.Result{State: monitor.OK, Detail: "accepted"}
		case a.expires.Sub(now) > credentialExpiryWarning:
			return monitor.Result{State: monitor.OK, Detail: "accepted, expires " + a.expires.UTC().Format("2006-01-02")}
		}
		return monitor.Result{State: monitor.Warning, Detail: "expires in " + api.HumanAge(a.expires.Sub(now)) + " (" + a.expires.UTC().Format("2006-01-02") + "): replace it with " + command}
	case credentialRefused:
		return monitor.Result{State: monitor.Critical, Detail: "refused: " + a.reason + ": run " + command}
	case credentialMissing:
		return monitor.Result{State: monitor.Critical, Detail: "secret " + p.secret + " is not stored: run " + command}
	case credentialUnasked:
		return monitor.Result{State: monitor.Unknown, Detail: "could not be asked: " + a.reason}
	}
	return monitor.Result{State: monitor.Unknown, Detail: "could not be asked"}
}

// credentialChecks returns the monitor's check of each credential (see credentialProbes).
func (w *Monitoring) credentialChecks(m *manifest.Manifest, ingress *state.IngressState) []monitor.Check {
	var out []monitor.Check
	for _, p := range credentialProbes(w.cfg, w.store, m, ingress) {
		out = append(out, monitor.NewFunc(p.check, credentialInterval, credentialFailures, func(ctx context.Context) monitor.Result {
			return credentialResult(p, p.ask(ctx), w.cfg.Environment, w.clock())
		}))
	}
	return out
}

// credentialDoctorChecks asks about every credential now, all at once, and reports each as a check that is never critical:
// a credential in trouble is the operator's to replace, and the agent itself is healthy. now is the clock for expiry.
func credentialDoctorChecks(ctx context.Context, c Config, s *state.Store, now time.Time) []api.Check {
	var m *manifest.Manifest
	if dm, _, err := deployedManifest(ctx, s); err == nil {
		m = &dm
	}
	var ingress *state.IngressState
	if st, err := s.Ingress(ctx); err == nil {
		ingress = &st
	}
	probes := credentialProbes(c, s, m, ingress)
	ctx, cancel := context.WithTimeout(ctx, credentialDoctorTimeout)
	defer cancel()
	out := make([]api.Check, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := credentialResult(p, p.ask(ctx), c.Environment, now)
			out[i] = api.Check{Name: p.check, OK: res.State == monitor.OK, Detail: res.Detail}
		}()
	}
	wg.Wait()
	return out
}
