package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

// errAlertsNotConfigured means no alert channel is set up for the environment.
var errAlertsNotConfigured = errors.New("alerts are not configured")

// errAlertConfig means the environment's settings could not be read, so the alert settings are unknown.
var errAlertConfig = errors.New("alerts configuration unavailable")

// What a channel says while a secret it needs cannot be used. The messages name the kind of secret, never a value.
const (
	missingTelegramSecret       = "telegram bot token secret missing"
	missingWebhookHeadersSecret = "webhook headers secret missing"
)

// alertChannel is one configured channel as the agent can use it right now: its notifier, or the reason it cannot send.
type alertChannel struct {
	name     string
	okDetail string // what the doctor says while the channel's secrets are usable
	notifier monitor.Notifier
	err      error
}

// alertChannels returns the channels that a configures, in the order Telegram, webhook. A channel whose secrets are not
// usable carries its error, so the others still deliver and each failure is reported for its own channel.
func alertChannels(ctx context.Context, c Config, s *state.Store, a manifest.Alerts) []alertChannel {
	var out []alertChannel
	if a.Telegram != nil {
		n, err := telegramOf(ctx, c, s, *a.Telegram)
		out = append(out, alertChannel{name: "telegram", okDetail: "bot token secret present", notifier: n, err: err})
	}
	if a.Webhook != nil {
		n, err := webhookOf(ctx, s, *a.Webhook)
		detail := "url secret present"
		if a.Webhook.HeadersSecret != "" {
			detail = "url and headers secrets present"
		}
		out = append(out, alertChannel{name: "webhook", okDetail: detail, notifier: n, err: err})
	}
	return out
}

// alertNotifiers returns one notifier for every channel that a configures. A channel that cannot send is a notifier that
// fails with the reason.
func alertNotifiers(ctx context.Context, c Config, s *state.Store, a manifest.Alerts) monitor.Notifiers {
	var out monitor.Notifiers
	for _, ch := range alertChannels(ctx, c, s, a) {
		if ch.err != nil {
			out = append(out, brokenChannel{channel: ch.name, err: ch.err})
			continue
		}
		out = append(out, ch.notifier)
	}
	return out
}

// alertChannelStatus tells the doctor and the monitor whether the channel's secrets are usable, with the detail to show.
func alertChannelStatus(ctx context.Context, c Config, s *state.Store, a manifest.Alerts, name string) (bool, string) {
	for _, ch := range alertChannels(ctx, c, s, a) {
		if ch.name != name {
			continue
		}
		if ch.err != nil {
			return false, ch.err.Error()
		}
		return true, ch.okDetail
	}
	return false, "not configured"
}

// brokenChannel is a channel whose secrets cannot be used. Every delivery fails with the reason.
type brokenChannel struct {
	channel string
	err     error
}

func (b brokenChannel) Channel() string { return b.channel }

func (b brokenChannel) Notify(context.Context, monitor.Alert) error { return b.err }

// plainSecret returns the value of a plain secret. A registry credential, a missing secret and an empty one count as not
// stored.
func plainSecret(ctx context.Context, s *state.Store, name string) ([]byte, bool) {
	info, value, err := s.Secret(ctx, name)
	if err != nil || info.Registry != "" || len(value) == 0 {
		return nil, false
	}
	return value, true
}

// urlSecret returns the URL that a channel's secret holds. The error says what is wrong with the secret, by name, and never
// holds a value. channel is the word the message starts with, such as webhook or heartbeat.
func urlSecret(ctx context.Context, s *state.Store, channel, name string) (string, error) {
	raw, ok := plainSecret(ctx, s, name)
	if !ok {
		return "", fmt.Errorf("%s url secret missing", channel)
	}
	if u, err := url.Parse(string(raw)); err != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s url secret %s is not an http or https URL", channel, name)
	}
	return string(raw), nil
}

// telegramOf returns the Telegram sender of a channel's settings, or the error that says why it cannot send.
func telegramOf(ctx context.Context, c Config, s *state.Store, t manifest.Telegram) (monitor.Notifier, error) {
	token, ok := plainSecret(ctx, s, t.BotTokenSecret)
	if !ok {
		return nil, errors.New(missingTelegramSecret)
	}
	return &monitor.Telegram{BaseURL: c.TelegramAPIURL, Token: string(token), ChatID: t.ChatID}, nil
}

// webhookOf returns the webhook sender of a channel's settings, or the error that says why it cannot send. The URL and the
// headers are parsed here, once, where they are read. An error names the secret and never a value.
func webhookOf(ctx context.Context, s *state.Store, w manifest.Webhook) (monitor.Notifier, error) {
	target, err := urlSecret(ctx, s, "webhook", w.URLSecret)
	if err != nil {
		return nil, err
	}
	out := &monitor.Webhook{URL: target}
	if w.HeadersSecret != "" {
		value, ok := plainSecret(ctx, s, w.HeadersSecret)
		if !ok {
			return nil, errors.New(missingWebhookHeadersSecret)
		}
		headers, err := telemetry.ParseHeaders(string(value))
		if err != nil {
			return nil, fmt.Errorf("webhook headers secret %s is malformed: %w", w.HeadersSecret, err)
		}
		out.Headers = headers
	}
	return out, nil
}

// channelResult is what alerts test says about one channel.
type channelResult struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// sendTestAlert sends one test alert to every configured channel at the same time, and reports each channel. It returns
// errAlertsNotConfigured when no channel is set up, and errAlertConfig when the settings cannot be read.
func sendTestAlert(ctx context.Context, c Config, s *state.Store) ([]channelResult, error) {
	m, err := settingsManifest(ctx, s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errAlertsNotConfigured
	}
	if err != nil {
		return nil, errAlertConfig
	}
	if m.Alerts == nil {
		return nil, errAlertsNotConfigured
	}
	notifiers := alertNotifiers(ctx, c, s, *m.Alerts)
	if len(notifiers) == 0 {
		return nil, errAlertsNotConfigured
	}
	text := fmt.Sprintf("🧪 %s test alert from berthy", c.Environment)
	alert := monitor.Alert{Environment: c.Environment, Kind: "test", Name: "test", State: monitor.OK, Detail: "test alert from berthy", Since: time.Now().UTC(), Text: text}
	results := make([]channelResult, len(notifiers))
	var wg sync.WaitGroup
	for i, n := range notifiers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, alertTimeout)
			defer cancel()
			results[i] = channelResult{Channel: monitor.ChannelName(n), OK: true}
			if err := n.Notify(nctx, alert); err != nil {
				results[i].OK, results[i].Error = false, err.Error()
			}
		}()
	}
	wg.Wait()
	return results, nil
}

// alertError answers a test alert that could not be tried at all.
func alertError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAlertsNotConfigured) {
		writeError(w, http.StatusBadRequest, "alerts_not_configured", "alerts are not configured")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "alerts_unavailable", "alerts unavailable")
}
