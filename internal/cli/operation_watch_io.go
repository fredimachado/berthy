package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"berthy/internal/agentclient"
)

type watchReader struct {
	client                    *http.Client
	base                      *url.URL
	token, prefix, id, events string
}

type watchOutput struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

func (r watchReader) stream(ctx context.Context, after uint64, progress bool, accept func(operationEvent)) (last uint64, refusal string, older bool, err error) {
	streamURL := *r.base
	streamURL.Path, streamURL.RawPath, streamURL.RawQuery = r.events, "", ""
	if progress {
		streamURL.RawQuery = "progress=1"
	}
	streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, streamURL.String(), nil)
	if err != nil {
		return after, "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if after != 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(after, 10))
	}
	resp, err := agentclient.SendRequest(streamCtx, r.client, req, r.token)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return after, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		refusal = agentclient.Message(resp.StatusCode, body)
		older = agentclient.IsOlderAgent(resp)
	}
	if resp.StatusCode != http.StatusOK {
		return after, refusal, older, errors.New("operation stream unavailable")
	}
	last, err = readOperationEvents(resp.Body, after, accept)
	return last, "", false, err
}

func (r watchReader) status(ctx context.Context) (state string, data []byte) {
	statusURL := *r.base
	statusURL.Path, statusURL.RawPath, statusURL.RawQuery = r.prefix+"operations/"+url.PathEscape(r.id), "", ""
	resp, err := agentclient.Send(ctx, r.client, http.MethodGet, statusURL.String(), r.token, nil)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return "", nil
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	resp.Body.Close()
	var v struct {
		State string `json:"state"`
	}
	if err == nil && len(data) <= 1<<20 && resp.StatusCode == http.StatusOK && json.Unmarshal(data, &v) == nil {
		switch v.State {
		case "running", "succeeded", "failed", "rolled_back", "interrupted", "recovery_required":
			return v.State, data
		}
	}
	return "", data
}

func (r watchReader) output(ctx context.Context, state string) (*watchOutput, error) {
	outputURL := *r.base
	outputURL.Path, outputURL.RawPath, outputURL.RawQuery = r.prefix+"operations/"+url.PathEscape(r.id)+"/output", "", ""
	resp, err := agentclient.Send(ctx, r.client, http.MethodGet, outputURL.String(), r.token, nil)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && state == "failed" {
		return nil, nil
	}
	var result watchOutput
	if err != nil || len(data) > 1<<20 || resp.StatusCode != http.StatusOK || json.Unmarshal(data, &result) != nil {
		return nil, agentclient.ErrInvalidResponse
	}
	return &result, nil
}
