package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/dialtest"
)

// secretURLPath stands for the credential that a webhook URL often holds.
const secretURLPath = "/hooks/s3cr3t-token-value"

func sampleAlert() Alert {
	return Alert{
		Environment: "lab",
		Kind:        "check",
		Name:        "probe:site",
		State:       Critical,
		Detail:      "HTTP 503",
		Since:       time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
		Text:        "🔴 lab probe:site: HTTP 503",
	}
}

func TestWebhookPostsTheAlertAsJSON(t *testing.T) {
	var mu sync.Mutex
	var req *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		req = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	wh := &Webhook{URL: srv.URL + secretURLPath, Headers: map[string]string{"Authorization": "Bearer abc"}}
	if err := wh.Notify(context.Background(), sampleAlert()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if req.Method != http.MethodPost || req.URL.Path != secretURLPath {
		t.Fatalf("request %s %s", req.Method, req.URL.Path)
	}
	if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Authorization") != "Bearer abc" {
		t.Fatalf("headers %v", req.Header)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	want := map[string]any{
		"environment": "lab", "kind": "check", "name": "probe:site", "state": "critical", "detail": "HTTP 503",
		"since": "2026-11-16T10:00:00Z", "text": "🔴 lab probe:site: HTTP 503",
	}
	if len(got) != len(want) {
		t.Fatalf("fields %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestWebhookLeavesOutAnUnknownSince(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { body, _ = io.ReadAll(r.Body) }))
	defer srv.Close()
	a := sampleAlert()
	a.Since = time.Time{}
	if err := (&Webhook{URL: srv.URL}).Notify(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "since") {
		t.Fatalf("body %s", body)
	}
}

func TestWebhookReportsAStatusAndNeverTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream says "+r.URL.Path)
	}))
	defer srv.Close()
	err := (&Webhook{URL: srv.URL + secretURLPath}).Notify(context.Background(), sampleAlert())
	var status *WebhookStatusError
	if !errors.As(err, &status) || status.Code != http.StatusBadGateway {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") || err.Error() != "webhook returned HTTP 502" {
		t.Fatalf("error text %q", err)
	}
}

func TestWebhookTransportErrorsNameAClassNotTheURL(t *testing.T) {
	err := (&Webhook{URL: "http://" + dialtest.Refused + secretURLPath, Client: dialtest.Client()}).Notify(context.Background(), sampleAlert())
	if !errors.Is(err, ErrWebhookUnavailable) || strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "dialtest.invalid") {
		t.Fatalf("error %v", err)
	}
	err = (&Webhook{URL: "http://bad host" + secretURLPath}).Notify(context.Background(), sampleAlert())
	if !errors.Is(err, ErrWebhookUnavailable) || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("invalid URL: %v", err)
	}
}

func TestWebhookTimeoutIsItsOwnClass(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := (&Webhook{URL: srv.URL + secretURLPath}).Notify(ctx, sampleAlert())
	if !errors.Is(err, ErrWebhookTimeout) || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error %v", err)
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	err := (&Webhook{URL: srv.URL}).Notify(context.Background(), sampleAlert())
	var status *WebhookStatusError
	if !errors.As(err, &status) || status.Code != http.StatusTemporaryRedirect || hits != 0 {
		t.Fatalf("error %v, redirect target hit %d times", err, hits)
	}
}

func TestWebhookFormatsWithoutItsURL(t *testing.T) {
	wh := Webhook{URL: "https://hooks.example.com" + secretURLPath, Headers: map[string]string{"Authorization": "Bearer abc"}}
	for _, s := range []string{wh.String(), (&wh).String(), strings.TrimSpace(fmtAll(wh))} {
		if strings.Contains(s, "s3cr3t") || strings.Contains(s, "Bearer") {
			t.Fatalf("formatted webhook leaks: %q", s)
		}
	}
}

// fmtAll formats v the ways a log line could.
func fmtAll(v any) string {
	return fmt.Sprintf("%v %+v %#v %s", v, v, v, v)
}

// fakeChannel is a notifier that records what it was given. err is what it answers.
type fakeChannel struct {
	name string
	err  error
	mu   sync.Mutex
	got  []Alert
}

func (c *fakeChannel) Channel() string { return c.name }
func (c *fakeChannel) Notify(_ context.Context, a Alert) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, a)
	return c.err
}

func TestNotifiersDeliverToEveryChannel(t *testing.T) {
	a, b := &fakeChannel{name: "telegram"}, &fakeChannel{name: "webhook"}
	if err := (Notifiers{a, b}).Notify(context.Background(), sampleAlert()); err != nil {
		t.Fatal(err)
	}
	if len(a.got) != 1 || len(b.got) != 1 || a.got[0] != sampleAlert() {
		t.Fatalf("delivered %v and %v", a.got, b.got)
	}
}

func TestNotifiersNameTheChannelThatFailedAndStillReachTheOthers(t *testing.T) {
	a, b := &fakeChannel{name: "telegram"}, &fakeChannel{name: "webhook", err: &WebhookStatusError{Code: 500}}
	err := (Notifiers{b, a}).Notify(context.Background(), sampleAlert())
	var partial *DeliveryError
	if !errors.As(err, &partial) || partial.Delivered != 1 {
		t.Fatalf("error %v", err)
	}
	if err.Error() != "webhook: webhook returned HTTP 500" || !errors.Is(err, b.err) || len(a.got) != 1 {
		t.Fatalf("error %q, telegram got %d", err, len(a.got))
	}
}

func TestNotifiersThatAllFailReturnAPlainError(t *testing.T) {
	a, b := &fakeChannel{name: "telegram", err: ErrTelegramUnavailable}, &fakeChannel{name: "webhook", err: ErrWebhookUnavailable}
	err := (Notifiers{a, b}).Notify(context.Background(), sampleAlert())
	var partial *DeliveryError
	if err == nil || errors.As(err, &partial) {
		t.Fatalf("error %v", err)
	}
	if err.Error() != "telegram: telegram unavailable\nwebhook: webhook unavailable" {
		t.Fatalf("error %q", err)
	}
}

func TestNotifiersDoNotLetASlowChannelHoldBackTheOthers(t *testing.T) {
	release := make(chan struct{})
	slow := notifierFunc(func(ctx context.Context, _ Alert) error { <-release; return nil })
	fast := &fakeChannel{name: "telegram"}
	done := make(chan error, 1)
	go func() { done <- (Notifiers{slow, fast}).Notify(context.Background(), sampleAlert()) }()
	deadline := time.After(2 * time.Second)
	for {
		fast.mu.Lock()
		n := len(fast.got)
		fast.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the fast channel waited for the slow one")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type notifierFunc func(context.Context, Alert) error

func (f notifierFunc) Notify(ctx context.Context, a Alert) error { return f(ctx, a) }

func TestChannelNameFallsBackForAnUnnamedNotifier(t *testing.T) {
	if got := ChannelName(&fakeChannel{name: "webhook"}); got != "webhook" {
		t.Fatal(got)
	}
	if got := ChannelName(notifierFunc(nil)); got != "alert channel" {
		t.Fatal(got)
	}
	if ChannelName(&Telegram{}) != "telegram" || ChannelName(&Webhook{}) != "webhook" {
		t.Fatal("the channels do not name themselves")
	}
}
