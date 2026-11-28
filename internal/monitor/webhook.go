package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrWebhookUnavailable means the webhook could not be reached. Its text never includes the URL, which is often the
// credential.
var ErrWebhookUnavailable = errors.New("webhook unavailable")

// ErrWebhookTimeout means the webhook did not answer in time. Its text never includes the URL either.
var ErrWebhookTimeout = errors.New("webhook timed out")

// WebhookStatusError means the webhook answered with a status outside 200 to 299.
type WebhookStatusError struct{ Code int }

func (e *WebhookStatusError) Error() string {
	return fmt.Sprintf("webhook returned HTTP %d", e.Code)
}

// Webhook sends each alert as an HTTP POST of a JSON document. The URL is often the credential and the headers may be one,
// so this type never formats them, and no error it returns contains the URL.
type Webhook struct {
	URL     string
	Headers map[string]string
	Client  *http.Client // nil means a client with a 10-second timeout that never follows redirects
}

// String hides the URL and the headers, so a formatted Webhook cannot leak them into a log.
func (w Webhook) String() string { return "webhook" }

// GoString hides the URL and the headers for %#v.
func (w Webhook) GoString() string { return w.String() }

// Channel names the channel in an error or a log line.
func (Webhook) Channel() string { return "webhook" }

// webhookBody is the JSON document of an alert.
type webhookBody struct {
	Environment string     `json:"environment"`
	Server      string     `json:"server,omitempty"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	State       string     `json:"state"`
	Detail      string     `json:"detail"`
	Since       *time.Time `json:"since,omitempty"`
	Text        string     `json:"text"`
}

// Notify posts the alert as JSON. Any status from 200 to 299 counts as delivered.
func (w *Webhook) Notify(ctx context.Context, a Alert) error {
	body := webhookBody{Environment: a.Environment, Server: a.Server, Kind: a.Kind, Name: a.Name, State: a.State, Detail: a.Detail, Text: a.Text}
	if !a.Since.IsZero() {
		since := a.Since.UTC()
		body.Since = &since
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ErrWebhookUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(payload))
	if err != nil {
		return ErrWebhookUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "berthy-alert")
	for name, value := range w.Headers {
		req.Header.Set(name, value)
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error is a *url.Error that holds the URL, so only its class is kept.
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			return ErrWebhookTimeout
		}
		return ErrWebhookUnavailable
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck // Draining only permits connection reuse; the status determines delivery.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &WebhookStatusError{Code: resp.StatusCode}
	}
	return nil
}
