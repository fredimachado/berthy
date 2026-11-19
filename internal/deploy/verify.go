package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// Reacher opens a TCP connection to host:port from inside the shared network. A nil error means the port answers. An error
// that wraps ErrReachUnavailable means the check could not run; any other error means the port did not answer.
type Reacher interface {
	Reach(ctx context.Context, network, host string, port int) error
}

// ErrReachUnavailable marks the error of a Reacher that could not make the check, as opposed to one whose check failed.
var ErrReachUnavailable = errors.New("check unavailable")

// The outside check asks each hostname up to outsideTries times, outsidePauseDefault apart, so DNS and the route have about
// twenty seconds to settle. One try waits at most outsideTimeout. A hostname answers when it returns a status below 500.
const (
	outsideTries        = 4
	outsidePauseDefault = 5 * time.Second
	outsideTimeout      = 5 * time.Second
	// maxVerifyDetail bounds the reason of a check that could not run, as the event shows it.
	maxVerifyDetail = 200
)

// verifyDeploy checks, once a deploy of app has succeeded, that each role with a port answers on the network and that each
// hostname a published role has answers from outside. The checks warn and never change the outcome: the new version is
// already serving. Each result is one event of the operation. The app's deploy.verify: false turns the checks off.
func (c *Controller) verifyDeploy(ctx context.Context, opID, network string, app manifest.App, work []roleWork, published bool) {
	if !app.Deploy.Verifies() {
		return
	}
	for _, w := range work {
		role := w.intent.Role
		port := app.Roles[role].Port
		// A role scaled to zero has no task to answer.
		if c.Reacher != nil && port > 0 && w.replicas > 0 {
			c.verifyNetwork(ctx, opID, network, role, w.intent.Name, port)
		}
	}
	if !published {
		return
	}
	for _, w := range work {
		role := w.intent.Role
		if w.replicas > 0 {
			c.verifyOutside(ctx, opID, role, app.Roles[role].Hostnames)
		}
	}
}

// verifyNetwork records whether the Swarm service of a role answers on its port from the shared network.
func (c *Controller) verifyNetwork(ctx context.Context, opID, network, role, service string, port int) {
	err := c.Reacher.Reach(ctx, network, service, port)
	switch {
	case err == nil:
		c.verifyEvent(ctx, opID, role, "network", "ok", fmt.Sprintf("role %s: port %d answers on the network", role, port))
	case errors.Is(err, ErrReachUnavailable):
		c.verifyEvent(ctx, opID, role, "network", "unknown", fmt.Sprintf("role %s: port %d could not be checked on the network (%s)", role, port, detailOf(err)))
	default:
		c.verifyEvent(ctx, opID, role, "network", "warn", fmt.Sprintf("role %s: healthy, but port %d does not answer on the network; is the app listening on 127.0.0.1?", role, port))
	}
}

// detailOf is the reason inside an unavailable check's error: the text after "check unavailable: ", cut to a line.
func detailOf(err error) string {
	text := err.Error()
	if prefix := ErrReachUnavailable.Error() + ": "; len(text) > len(prefix) && text[:len(prefix)] == prefix {
		text = text[len(prefix):]
	}
	out := make([]rune, 0, len(text))
	for _, r := range text {
		if r >= 0x20 && r < 0x7f {
			out = append(out, r)
		}
		if len(out) == maxVerifyDetail {
			break
		}
	}
	return string(out)
}

// verifyOutside asks each hostname of a role from outside, together, and records one event for each in the order of the
// manifest.
func (c *Controller) verifyOutside(ctx context.Context, opID, role string, hostnames []string) {
	lines := make([]verifyResult, len(hostnames))
	var wg sync.WaitGroup
	for i, host := range hostnames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lines[i] = c.askOutside(ctx, role, host)
		}()
	}
	wg.Wait()
	for _, l := range lines {
		c.verifyEvent(ctx, opID, role, "outside", l.status, l.text)
	}
}

type verifyResult struct{ status, text string }

// askOutside makes up to outsideTries GET requests to https://host/ and stops at the first status below 500. It does not
// follow redirects: a redirect is an answer.
func (c *Controller) askOutside(ctx context.Context, role, host string) verifyResult {
	client := http.Client{Timeout: outsideTimeout}
	if c.Outside != nil {
		client = *c.Outside
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	pause := c.outsidePause
	if pause <= 0 {
		pause = outsidePauseDefault
	}
	url := "https://" + host
	status := 0
	for try := 0; try < outsideTries && ctx.Err() == nil; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(pause):
			}
		}
		rctx, cancel := context.WithTimeout(ctx, outsideTimeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, url+"/", nil)
		if err != nil {
			cancel()
			break
		}
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			status = 0
			continue
		}
		resp.Body.Close()
		status = resp.StatusCode
		if status < 500 {
			return verifyResult{"ok", fmt.Sprintf("role %s: %s answers with status %d from outside", role, url, status)}
		}
	}
	if status == 0 {
		return verifyResult{"warn", fmt.Sprintf("role %s: nothing answers at %s from outside; DNS or the route may still be settling", role, url)}
	}
	return verifyResult{"warn", fmt.Sprintf("role %s: %s answers with status %d from outside; the route or the app is not ready", role, url, status)}
}

// verifyEvent records the result of one check. status is ok, warn or unknown, and text is the line people read.
func (c *Controller) verifyEvent(ctx context.Context, opID, role, check, status, text string) {
	data, err := json.Marshal(map[string]string{"role": role, "check": check, "status": status, "step": text})
	if err != nil {
		return
	}
	_ = c.Store.AppendEvent(ctx, opID, state.Event{Kind: "verify", Data: data})
}
