package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// HeartbeatTimeout is how long one ping may take. A ping is never retried: a missed ping is the signal.
const HeartbeatTimeout = 5 * time.Second

// Heartbeat is the dead man's switch. Its check sends one GET to a URL every interval while Docker answers, and records when
// a ping was last accepted, so that a service that watches for the pings can tell when they stop. The check is ok after an
// accepted ping and unknown otherwise, and it is never a warning or critical, so it never alerts: the channel an alert would
// use may be down for the same reason as the pings. The URL is a secret, so no detail, error or log line holds it.
type Heartbeat struct {
	// Alive reports whether Docker answers. No ping is sent while it does not.
	Alive func(ctx context.Context) bool
	// Client sends the pings. Nil means a client that never follows redirects.
	Client *http.Client
	// Timeout bounds one ping. Zero means HeartbeatTimeout.
	Timeout time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu   sync.Mutex
	last time.Time // when a ping was last accepted
}

// Check returns the check named heartbeat, which runs every interval. url returns the URL to ping, or the reason there is
// none, such as a secret that is not stored; its error must not hold the URL.
func (h *Heartbeat) Check(interval time.Duration, url func(context.Context) (string, error)) Check {
	return NewFunc("heartbeat", interval, 1, func(ctx context.Context) Result { return h.run(ctx, url) })
}

func (h *Heartbeat) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// run sends one ping, unless there is no URL or Docker does not answer.
func (h *Heartbeat) run(ctx context.Context, url func(context.Context) (string, error)) Result {
	target, err := url(ctx)
	if err != nil {
		return Result{State: Unknown, Detail: h.withLast(err.Error())}
	}
	if h.Alive != nil && !h.Alive(ctx) {
		return Result{State: Unknown, Detail: h.withLast("Docker unavailable: no ping sent")}
	}
	if err := h.ping(ctx, target); err != nil {
		return Result{State: Unknown, Detail: h.withLast(err.Error())}
	}
	now := h.now()
	h.mu.Lock()
	h.last = now
	h.mu.Unlock()
	return Result{State: OK, Detail: "last ping accepted at " + now.UTC().Format(time.RFC3339)}
}

// withLast adds when a ping was last accepted to a reason that none was sent or accepted now.
func (h *Heartbeat) withLast(reason string) string {
	h.mu.Lock()
	last := h.last
	h.mu.Unlock()
	if last.IsZero() {
		return reason + "; no ping accepted yet"
	}
	return reason + "; last ping accepted at " + last.UTC().Format(time.RFC3339)
}

// ping sends the GET. Any 2xx counts as accepted. The error names a class and never the URL.
func (h *Heartbeat) ping(ctx context.Context, target string) error {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = HeartbeatTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("ping failed: invalid URL")
	}
	req.Header.Set("User-Agent", "berthy-heartbeat")
	client := h.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		var timedOut interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timedOut) && timedOut.Timeout() {
			return errors.New("ping failed: timeout")
		}
		return errors.New("ping failed: unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ping not accepted: HTTP %d", resp.StatusCode)
	}
	return nil
}
