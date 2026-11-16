package engine

import (
	"berthy/internal/logs"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) Open(ctx context.Context, id string, opts logs.OpenOptions) (io.ReadCloser, bool, error) {
	var info struct{ Config struct{ Tty bool } }
	if err := e.Get(ctx, "/containers/"+url.PathEscape(id)+"/json", &info); err != nil {
		return nil, false, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(opts.Tail)}}
	if opts.Follow {
		q.Set("follow", "1")
	}
	if !opts.Since.IsZero() {
		q.Set("since", strconv.FormatInt(opts.Since.Unix(), 10))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/v1.44/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	client := http.Client{Transport: e.client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("Docker unavailable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, false, &StatusError{Code: resp.StatusCode}
	}
	return resp.Body, info.Config.Tty, nil
}

// LogTail returns the last lines of a container's stdout and stderr, interleaved in time order, each as
// "<timestamp> <stdout|stderr> <message>". It keeps the newest lines that fit in maxBytes and reports whether it dropped
// any. The caller sets the deadline.
func (e *Engine) LogTail(ctx context.Context, id string, lines, maxBytes int) (string, bool, error) {
	records, err := logs.Tail(ctx, e, []logs.Identity{{ContainerID: id}}, logs.Options{Tail: lines})
	if err != nil {
		return "", false, err
	}
	formatted := make([]string, len(records))
	for i, r := range records {
		formatted[i] = r.Timestamp.UTC().Format(time.RFC3339Nano) + " " + r.Stream + " " + r.Message + "\n"
	}
	size, from := 0, len(formatted)
	for from > 0 && size+len(formatted[from-1]) <= maxBytes {
		from--
		size += len(formatted[from])
	}
	return strings.ToValidUTF8(strings.Join(formatted[from:], ""), "?"), from > 0, nil
}
