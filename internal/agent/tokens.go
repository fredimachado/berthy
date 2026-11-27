package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"berthy/internal/api"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
)

// Where a token is defined: the agent's configuration file, or the agent's state, which the API changes.
const (
	originConfig = "config"
	originAPI    = "api"
)

// configPath is where berthy setup puts the agent's configuration, and so what a refusal to change a token of that file names.
const configPath = "/etc/berthy/agent.json"

var tokenNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidTokenName reports whether name may name a token: letters, digits, dots, dashes and underscores, starting with a letter or
// a digit. A token name is unique across the configuration file and the API.
func ValidTokenName(name string) bool { return tokenNamePattern.MatchString(name) }

// A tokenEntry is a token from either source. Its name is never empty: a token of the configuration file without one is named
// from its fingerprint.
type tokenEntry struct {
	Token
	origin  string
	created time.Time // zero for a token of the configuration file, which does not say
	expires *time.Time
}

func (e tokenEntry) expired(now time.Time) bool { return e.expires != nil && !now.Before(*e.expires) }

// admin reports whether the token is a person's admin token that works now: the kind of token that must always remain.
func (e tokenEntry) admin(now time.Time) bool {
	return e.Scope == "admin" && !e.Agent && !e.expired(now)
}

// sum is the SHA-256 of the token's value, which is its digest. ok is false for a digest that is not 32 bytes of hex.
func (e tokenEntry) sum() (sum [32]byte, ok bool) {
	b, err := hex.DecodeString(e.Digest)
	if err != nil || len(b) != 32 {
		return sum, false
	}
	copy(sum[:], b)
	return sum, true
}

func stateEntry(t state.Token) tokenEntry {
	exp := t.Expires
	return tokenEntry{Token: Token{Name: t.Name, Digest: t.Digest, Scope: t.Scope, Agent: t.Agent}, origin: originAPI, created: t.Created, expires: exp}
}

// tokenSource is every token that may authenticate: the ones in agent.json and the ones the API made, which the state holds.
// It is the one place that builds that set, so authentication, authorization, the approvals and the revoke guard agree on who
// exists. An agent without state has the file's tokens alone.
type tokenSource struct {
	cfg   Config
	store *state.Store
	mu    sync.Mutex
	// written is when each token's use was last recorded, so a busy token costs one write a minute.
	written map[string]time.Time
}

func newTokenSource(c Config, s *state.Store) *tokenSource {
	return &tokenSource{cfg: c, store: s, written: map[string]time.Time{}}
}

func failedEnvironmentHandler(c Config, store *state.Store, reason string) http.Handler {
	tokens := newTokenSource(c, store)
	message := c.ID().String() + ": " + reason
	return serveAPI(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, status := tokens.identify(r); status != http.StatusOK {
			refuseToken(w, http.StatusUnauthorized)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "environment_failed", message)
	}))
}

const tokenUseInterval = time.Minute

// entries returns every token, the file's first, then the state's oldest first, expired ones included. When the state cannot
// be read it returns the file's tokens and the error, so a token of the file still works while the state is down.
func (t *tokenSource) entries(ctx context.Context) ([]tokenEntry, error) {
	out := make([]tokenEntry, 0, len(t.cfg.Tokens))
	for _, c := range t.cfg.Tokens {
		c.Digest = strings.ToLower(c.Digest)
		e := tokenEntry{Token: c, origin: originConfig}
		if e.Name == "" {
			sum, _ := e.sum()
			e.Name = "token:" + tokenID(sum)[:12]
		}
		out = append(out, e)
	}
	if t.store == nil {
		return out, nil
	}
	rows, err := t.store.Tokens(ctx)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out = append(out, stateEntry(r))
	}
	return out, nil
}

// identify finds the token a request carries. status is 200 when it is one that works now, 401 when the request carries no
// valid token for this environment, and 503 when the state could not be read and the token is not one of the file's. A token
// that works is recorded as used.
func (t *tokenSource) identify(r *http.Request) (tokenEntry, int) {
	header := r.Header.Get("Authorization")
	raw := strings.TrimPrefix(header, "Bearer ")
	if !strings.HasPrefix(header, "Bearer ") || !tokenPattern(scope.ID{Project: t.cfg.Project, Environment: t.cfg.Environment}).MatchString(raw) {
		return tokenEntry{}, http.StatusUnauthorized
	}
	sum := sha256.Sum256([]byte(raw))
	entries, err := t.entries(r.Context())
	now := t.cfg.now()
	var found *tokenEntry
	for i := range entries {
		want, ok := entries[i].sum()
		if ok && subtle.ConstantTimeCompare(sum[:], want[:]) == 1 && !entries[i].expired(now) {
			found = &entries[i]
		}
	}
	switch {
	case found != nil:
		t.touch(r.Context(), found.Digest, now)
		return *found, http.StatusOK
	case err != nil:
		return tokenEntry{}, http.StatusServiceUnavailable
	}
	return tokenEntry{}, http.StatusUnauthorized
}

// touch records that the token with digest was used at now, at most once a minute. The record is best effort: a request is
// never refused because it could not be written.
func (t *tokenSource) touch(ctx context.Context, digest string, now time.Time) {
	if t.store == nil {
		return
	}
	t.mu.Lock()
	last, seen := t.written[digest]
	if seen {
		if since := now.Sub(last); since >= 0 && since < tokenUseInterval {
			t.mu.Unlock()
			return
		}
	}
	t.written[digest] = now
	t.mu.Unlock()
	_ = t.store.TouchTokens(ctx, map[string]time.Time{digest: now})
}

// tokenID is the fingerprint of a token's digest, in hex. It names the token's actor without revealing what verifies it.
func tokenID(sum [32]byte) string {
	fingerprint := sha256.Sum256(sum[:])
	return hex.EncodeToString(fingerprint[:])
}

var errLastAdmin = errors.New("the last admin token")

// tokenView is what the tokens route shows of a token. It never shows the digest.
type tokenView struct {
	Name     string     `json:"name"`
	Scope    string     `json:"scope"`
	Agent    bool       `json:"agent"`
	Origin   string     `json:"origin"`
	Created  *time.Time `json:"created,omitempty"`
	Expires  *time.Time `json:"expires,omitempty"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

// serveTokens answers the tokens routes: list, create and revoke. The caller has checked the scope and the method, and that the
// token is not an agent's when the request changes anything.
func serveTokens(w http.ResponseWriter, r *http.Request, rest []string, c Config, store *state.Store, src *tokenSource, sa state.Actor) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	entries, err := src.entries(ctx)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	now := c.now()
	switch {
	case len(rest) == 1 && r.Method == "GET":
		use, err := store.TokenUse(ctx)
		if err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		out := make([]tokenView, 0, len(entries))
		for _, e := range entries {
			v := tokenView{Name: e.Name, Scope: e.Scope, Agent: e.Agent, Origin: e.origin, Expires: e.expires}
			if !e.created.IsZero() {
				created := e.created
				v.Created = &created
			}
			if at, ok := use[e.Digest]; ok {
				v.LastUsed = &at
			}
			out = append(out, v)
		}
		_ = json.NewEncoder(w).Encode(out)
	case len(rest) == 1:
		createToken(w, r, c, store, entries, sa, now)
	default:
		revokeToken(w, r, rest[1], store, entries, sa, now)
	}
}

func createToken(w http.ResponseWriter, r *http.Request, c Config, store *state.Store, entries []tokenEntry, sa state.Actor, now time.Time) {
	var req createTokenBody
	if decodeBody(w, r, &req) != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if !ValidTokenName(req.Name) {
		writeError(w, 400, "bad_request", "invalid name: use letters, digits, dots, dashes and underscores")
		return
	}
	if req.Scope != "read" && req.Scope != "deploy" && req.Scope != "admin" {
		writeError(w, 400, "bad_request", "invalid scope: use read, deploy or admin")
		return
	}
	var expires *time.Time
	if req.Expires != "" {
		at, err := time.Parse(time.RFC3339, req.Expires)
		if err != nil {
			writeError(w, 400, "bad_request", "invalid expires: use an RFC 3339 time")
			return
		}
		at = at.UTC().Truncate(time.Millisecond)
		if !at.After(now) {
			writeError(w, 400, "bad_request", "expires must be in the future")
			return
		}
		expires = &at
	}
	for _, e := range entries {
		if e.Name == req.Name {
			writeError(w, 409, "token_exists", "a token named "+req.Name+" exists already")
			return
		}
	}
	answer := map[string]any{"name": req.Name, "scope": req.Scope, "agent": req.Agent}
	if expires != nil {
		answer["expires"] = expires
	}
	if req.DryRun {
		answer["action"] = "token create"
		_ = json.NewEncoder(w).Encode(answer)
		return
	}
	value, digest, err := NewToken(scope.ID{Project: c.Project, Environment: c.Environment})
	if err != nil {
		writeError(w, 503, "unavailable", "operation unavailable")
		return
	}
	err = store.CreateToken(r.Context(), sa, state.Token{Name: req.Name, Digest: digest, Scope: req.Scope, Agent: req.Agent, Created: now.UTC().Truncate(time.Millisecond), Expires: expires})
	if errors.Is(err, state.ErrConflict) {
		writeError(w, 409, "token_exists", "a token named "+req.Name+" exists already")
		return
	}
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	answer["token"] = value
	_ = json.NewEncoder(w).Encode(answer)
}

func revokeToken(w http.ResponseWriter, r *http.Request, name string, store *state.Store, entries []tokenEntry, sa state.Actor, now time.Time) {
	var req dryRunBody
	if err := decodeOptionalBody(w, r, &req); err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	var target *tokenEntry
	var kept []tokenEntry
	for i := range entries {
		if entries[i].Name == name {
			target = &entries[i]
		} else {
			kept = append(kept, entries[i])
		}
	}
	switch {
	case target == nil:
		writeError(w, 404, "not_found", "no token named "+name)
		return
	case target.origin == originConfig:
		writeError(w, 409, "token_in_config", "token "+name+" is defined in "+configPath+"; remove it there")
		return
	}
	// The tokens of the file that remain are fixed, so the guard needs only the state's, which the store hands it inside the
	// transaction.
	var fromFile []tokenEntry
	for _, e := range kept {
		if e.origin == originConfig {
			fromFile = append(fromFile, e)
		}
	}
	guard := func(remaining []state.Token) error {
		if !target.admin(now) {
			return nil
		}
		for _, e := range fromFile {
			if e.admin(now) {
				return nil
			}
		}
		for _, t := range remaining {
			if stateEntry(t).admin(now) {
				return nil
			}
		}
		return errLastAdmin
	}
	if req.DryRun {
		var remaining []state.Token
		for _, e := range kept {
			if e.origin == originAPI {
				remaining = append(remaining, state.Token{Name: e.Name, Scope: e.Scope, Agent: e.Agent, Expires: e.expires})
			}
		}
		if err := guard(remaining); err != nil {
			refuseLastAdmin(w, name)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "token revoke", "name": name, "scope": target.Scope, "agent": target.Agent})
		return
	}
	err := store.RevokeToken(r.Context(), sa, name, guard)
	switch {
	case errors.Is(err, errLastAdmin):
		refuseLastAdmin(w, name)
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, 404, "not_found", "no token named "+name)
	case err != nil:
		writeError(w, 503, "state_unavailable", "state unavailable")
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": name})
	}
}

func refuseLastAdmin(w http.ResponseWriter, name string) {
	writeError(w, 409, "last_admin", "token "+name+" is the last admin token: create another admin token first, or run berthy setup --new-token on the server")
}

// tokenExpiryInterval is how often the monitor looks at a token's expiry.
const tokenExpiryInterval = time.Hour

// tokenResult is what the check of a token that expires says: ok until credentialExpiryWarning before the expiry, then a
// warning, and a warning still once the token has expired and until it is revoked.
func tokenResult(t state.Token, now time.Time) monitor.Result {
	if t.Expires == nil {
		return monitor.Result{State: monitor.OK, Detail: "does not expire"}
	}
	date := t.Expires.UTC().Format("2006-01-02")
	switch left := t.Expires.Sub(now); {
	case left <= 0:
		return monitor.Result{State: monitor.Warning, Detail: "token " + t.Name + " expired on " + date + ": create a new one with berthy token create"}
	case left <= credentialExpiryWarning:
		return monitor.Result{State: monitor.Warning, Detail: "token " + t.Name + " expires in " + api.HumanAge(left) + " (" + date + "): create a new one with berthy token create"}
	}
	return monitor.Result{State: monitor.OK, Detail: "expires " + date}
}

// tokenChecks returns the monitor's check of each token that has an expiry.
func (w *Monitoring) tokenChecks(ctx context.Context) ([]monitor.Check, error) {
	tokens, err := w.store.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	var out []monitor.Check
	for _, t := range tokens {
		if t.Expires == nil {
			continue
		}
		out = append(out, monitor.NewFunc("token:"+t.Name, tokenExpiryInterval, 1, func(context.Context) monitor.Result {
			return tokenResult(t, w.clock())
		}))
	}
	return out, nil
}
