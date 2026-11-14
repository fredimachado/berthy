package engine

import (
	"berthy/internal/logs"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
