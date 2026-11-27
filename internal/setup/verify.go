package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/scope"
)

// ErrUnreachable is what a DoctorFunc returns when the agent could not be reached at all, as opposed to answering with an
// error.
var ErrUnreachable = errors.New("unreachable")

// DoctorFunc asks the agent at baseURL for the doctor of the hosted environment project/env with token.
type DoctorFunc func(ctx context.Context, baseURL, project, env, token string) ([]api.Check, error)

// httpDoctor is the DoctorFunc that sends the request from this machine, with a 5 second connect timeout.
func httpDoctor(ctx context.Context, baseURL, project, env, token string) ([]api.Check, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	prefix := scope.ID{Project: project, Environment: env}.APIPath()
	body, status, err := agentclient.Do(ctx, agentclient.NewHTTPClient(), u, prefix, token, "GET", "doctor", "", nil)
	if err != nil {
		if errors.Is(err, agentclient.ErrInvalidResponse) {
			return nil, err
		}
		return nil, ErrUnreachable
	}
	if status != 200 {
		return nil, errors.New(agentclient.Message(status, body))
	}
	return parseChecks(body)
}

func parseChecks(body []byte) ([]api.Check, error) {
	var d struct {
		Checks []api.Check `json:"checks"`
	}
	if json.Unmarshal(body, &d) != nil || len(d.Checks) == 0 {
		return nil, errors.New("the agent's doctor answer cannot be read")
	}
	return d.Checks, nil
}

// AdminToken is the live token this run made, or the token found for the CLI entry.
func (s *Session) AdminToken() string {
	if s.Token != "" && s.TokenLive {
		return s.Token
	}
	token, _ := s.existingCredential()
	return token
}

func (s *Session) existingCredential() (string, agentclient.TokenSource) {
	if s.credentialRead {
		return s.credential, s.credentialSource
	}
	store := s.Opts.Tokens
	if !s.cliConfigKnown || !agentclient.StoreUsable(s.Opts.Project, s.Opts.Env, s.cliEntry) {
		store = nil
	}
	e := s.cliEntry
	if e.TokenEnv == "" {
		e.TokenEnv = s.TokenEnv
	}
	s.credential, s.credentialSource, _ = agentclient.FindToken(s.Opts.getenv, store, s.Opts.Project, s.Opts.Env, e)
	s.credentialRead = true
	return s.credential, s.credentialSource
}

// summarize turns the checks into the step's result: every critical check must be ok.
func summarize(checks []api.Check) (string, error) {
	version := "unknown version"
	var failed, warned []string
	for _, c := range checks {
		if c.Name == "agent" && c.Detail != "" {
			version = c.Detail
		}
		if c.OK {
			continue
		}
		item := c.Name
		if c.Detail != "" {
			item += ": " + c.Detail
		}
		if c.Critical {
			failed = append(failed, item)
		} else {
			warned = append(warned, item)
		}
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("doctor found %d critical %s: %s", len(failed), plural(len(failed), "problem", "problems"), strings.Join(failed, "; "))
	}
	ok := len(checks) - len(warned)
	detail := fmt.Sprintf("agent %s, %d of %d checks ok", version, ok, len(checks))
	if len(warned) > 0 {
		detail += " (not ok: " + strings.Join(warned, "; ") + ")"
	}
	return detail, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// verifyStep asks the new agent for its doctor. From this machine when it can reach the agent, from the server otherwise.
// Without a token it only checks that the agent answers.
func verifyStep(ctx context.Context, s *Session) (string, string, error) {
	if s.DryRun {
		// Without an admin token a real run only checks that the agent answers, with the health script. With one, it asks the
		// doctor from this machine, which sends no script; only a real run finds out whether this machine can reach the agent.
		if !s.dryHasToken() {
			s.Log.Would(healthScript(s.Opts.listen()))
		}
		return StatusSkipped, "would check the agent", nil
	}
	listen := s.Opts.listen()
	base := agentURL(listen)
	token := s.AdminToken()
	if token == "" {
		if !s.healthy(ctx) {
			return "", "", fmt.Errorf("the agent does not answer on %s", base)
		}
		return StatusOK, "the agent answers; run berthy doctor with an admin token to check it fully", nil
	}
	doctor := s.Opts.Doctor
	if doctor == nil {
		doctor = httpDoctor
	}
	checks, err := doctor(ctx, base, s.Opts.Project, s.Opts.Env, token)
	prefix := ""
	switch {
	case err == nil:
	case errors.Is(err, ErrUnreachable):
		prefix = fmt.Sprintf("this machine cannot reach %s; checked on the server: ", base)
		if checks, err = s.doctorOnServer(ctx, listen, token); err != nil {
			return "", "", fmt.Errorf("this machine cannot reach %s; checking on the server failed: %s", base, redactDoctor(err.Error(), token))
		}
	default:
		return "", "", errors.New(redactDoctor(err.Error(), token))
	}
	detail, err := summarize(checks)
	if err != nil {
		return "", "", errors.New(redactDoctor(err.Error(), token))
	}
	return StatusOK, redactDoctor(prefix+detail, token), nil
}

func redactDoctor(s, token string) string { return strings.ReplaceAll(s, token, "<token>") }

// healthScript asks the server whether the agent answers on listen, for five seconds.
func healthScript(listen string) string { return waitScript(listen, 5) }

// dryHasToken says whether a real run would have an admin token to verify with.
func (s *Session) dryHasToken() bool {
	return s.dryToken || s.AdminToken() != ""
}

// healthy reports whether the agent answers /healthz, asked from the server.
func (s *Session) healthy(ctx context.Context) bool {
	res, err := s.R.Run(ctx, healthScript(s.Opts.listen()), nil)
	return err == nil && res.Code == 0
}

// doctorOnServer passes the token separately on stdin after selecting the probe.
func (s *Session) doctorOnServer(ctx context.Context, listen, token string) ([]api.Check, error) {
	id := scope.ID{Project: s.Opts.Project, Environment: s.Opts.Env}
	script := selectProbeScript(Staged{}) + "exec " + probeAsAgent + "\"$probe_bin\" agent setup-probe doctor --listen " +
		shellQuote(listen) + " --scope " + shellQuote(id.String())
	res, err := s.R.Run(ctx, script, strings.NewReader(token+"\n"))
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, errors.New(failureText(Result{Stderr: []byte(redactDoctor(string(res.Stderr), token)), Code: res.Code}))
	}
	return parseChecks(res.Stdout)
}
