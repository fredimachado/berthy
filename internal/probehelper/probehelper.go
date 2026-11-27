// Package probehelper is the server side of the overlay helper: the probe-helper process that runs in a container on the
// shared network and makes the agent's service probes from there. Its only endpoint is POST /probe, which needs the
// helper's bearer token and runs one HTTP or TCP probe. It keeps no state and does nothing else.
package probehelper

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"berthy/internal/monitor"
)

const (
	maxBody      = 4 << 10
	maxExpect    = 16
	minTimeoutMS = 1000
	maxTimeoutMS = 30000
)

var (
	namePattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	portPattern  = regexp.MustCompile(`^[0-9]{1,5}$`)
	pathPattern  = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]{0,1000}$`)
	tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidToken reports whether token has the form the agent issues: 32 random bytes in lowercase hex.
func ValidToken(token string) bool { return tokenPattern.MatchString(token) }

// Handler returns the helper's handler for token. Every request needs "Authorization: Bearer <token>", and any other
// request is refused with 401. An empty token refuses everything.
func Handler(token string) http.Handler { return handler(token, nil) }

func handler(token string, dial monitor.DialFunc) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if token == "" || !authorized(r, want) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		if r.URL.Path != "/probe" {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		req, err := decode(w, r)
		if err != nil {
			var refused *requestError
			errors.As(err, &refused)
			writeError(w, refused.status, refused.code, refused.message)
			return
		}
		ok, detail := run(r.Context(), req, dial)
		_ = json.NewEncoder(w).Encode(response{OK: ok, Detail: detail})
	})
}

// Serve answers probes on addr until ctx ends. It refuses to start without a valid token, and the token never appears
// in its errors.
func Serve(ctx context.Context, addr, token string) error {
	if !ValidToken(token) {
		return errors.New("probe helper token missing or invalid")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           Handler(token),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A probe may run for the longest timeout the helper accepts, so its answer may take that long plus a margin.
		WriteTimeout:   maxTimeoutMS*time.Millisecond + 10*time.Second,
		IdleTimeout:    30 * time.Second,
		MaxHeaderBytes: 8 << 10,
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		case <-done:
		}
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// authorized reports whether r carries the bearer token. Both sides are hashed first, so the comparison takes the same
// time whatever the token is.
func authorized(r *http.Request, want [sha256.Size]byte) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// requestError is a refused request. Its message is fixed text and never echoes the request.
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.message }

func invalid(message string) error {
	return &requestError{status: http.StatusBadRequest, code: "bad_request", message: message}
}

// decode reads the request body, which must be one JSON object of at most maxBody bytes with no unknown fields, and
// validates it.
func decode(w http.ResponseWriter, r *http.Request) (monitor.HelperProbe, error) {
	var req monitor.HelperProbe
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, bodyError(err, "request is not a probe")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return req, bodyError(err, "request has trailing data")
	}
	return req, validate(req)
}

// bodyError maps a body read failure to its refusal: too large is 413, anything else is a bad request.
func bodyError(err error, message string) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return &requestError{status: http.StatusRequestEntityTooLarge, code: "too_large", message: "request too large"}
	}
	return invalid(message)
}

// validate checks the probe the request asks for. Targets are name:port, so a probe reaches a service on the network and
// nothing else: no scheme, no user information and no path in the target.
func validate(req monitor.HelperProbe) error {
	if req.TimeoutMS < minTimeoutMS || req.TimeoutMS > maxTimeoutMS {
		return invalid("timeout_ms must be from 1000 to 30000")
	}
	if !validTarget(req.Target) {
		return invalid("target must be name:port")
	}
	switch req.Kind {
	case monitor.KindHTTP:
		if req.Path != "" && !pathPattern.MatchString(req.Path) {
			return invalid("path must start with / and use URL characters")
		}
		if len(req.Expect) > maxExpect {
			return invalid("expect lists too many statuses")
		}
		for _, code := range req.Expect {
			if code < 100 || code > 599 {
				return invalid("expect must list statuses from 100 to 599")
			}
		}
	case monitor.KindTCP:
		if req.Path != "" || len(req.Expect) > 0 {
			return invalid("tcp probes take no path or expect")
		}
	default:
		return invalid("kind must be http or tcp")
	}
	return nil
}

// validTarget reports whether target is a service name with a port from 1 to 65535.
func validTarget(target string) bool {
	i := strings.LastIndex(target, ":")
	if i < 0 {
		return false
	}
	name, port := target[:i], target[i+1:]
	if !namePattern.MatchString(name) || !portPattern.MatchString(port) {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// run makes the probe within its timeout. A probe that fails is an answer, not an error.
func run(ctx context.Context, req monitor.HelperProbe, dial monitor.DialFunc) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
	defer cancel()
	var res monitor.Result
	if req.Kind == monitor.KindHTTP {
		path := req.Path
		if path == "" {
			path = "/"
		}
		res = monitor.ProbeHTTP(ctx, "http://"+req.Target+path, req.Expect, dial)
	} else {
		res = monitor.ProbeTCP(ctx, req.Target, dial)
	}
	return res.State == monitor.OK, res.Detail
}

// response is the answer to a probe that ran.
type response struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
