package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/dialtest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// hookPath stands for the credential a webhook URL often holds. No log line, error or check detail may show it.
const hookPath = "/hooks/s3cr3t-hook-token"

// hookCall is one request the fake webhook received.
type hookCall struct {
	method string
	path   string
	header http.Header
	body   map[string]any
}

// hookReceiver is an HTTP endpoint for the webhook channel. status is the HTTP status it answers with. While block is not
// nil, a request waits until it is closed.
type hookReceiver struct {
	srv    *httptest.Server
	status atomic.Int32
	block  chan struct{}
	mu     sync.Mutex
	calls  []hookCall
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	h := &hookReceiver{}
	h.status.Store(http.StatusOK)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := hookCall{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: map[string]any{}}
		_ = json.Unmarshal(raw, &call.body)
		h.mu.Lock()
		h.calls = append(h.calls, call)
		block := h.block
		h.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(int(h.status.Load()))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookReceiver) url() string { return h.srv.URL + hookPath }

func (h *hookReceiver) received() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

// waitUntil waits up to 10 seconds for cond.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const alertsBlock = `"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"-1001234567890"},"webhook":{"url_secret":"ALERT_WEBHOOK_URL","headers_secret":"ALERT_WEBHOOK_HEADERS"}`

// bothChannelsStore returns a store with a manifest that alerts to Telegram and to a webhook, whose three secrets are stored.
// members are more members of the manifest, such as probes; they start with a comma. alertMembers are more members of the
// alerts block.
func bothChannelsStore(t *testing.T, hookURL, members, alertMembers string) *state.Store {
	t.Helper()
	s := openIngressStore(t)
	ctx := context.Background()
	for name, value := range map[string]string{
		"TELEGRAM_BOT_TOKEN":    reportsBotToken,
		"ALERT_WEBHOOK_URL":     hookURL,
		"ALERT_WEBHOOK_HEADERS": "Authorization=Bearer hook-secret-value",
	} {
		if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	recordRelease(t, s, "alerts", monitoredManifest(members+`,"alerts":{`+alertsBlock+alertMembers+`}`))
	return s
}

func TestAFailedDeployIsOnePostAndOneTelegramMessage(t *testing.T) {
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	r := NewReports(monitoringConfig(tg.srv.URL), bothChannelsStore(t, hook.url(), "", ""), nil)
	r.OperationEnded(context.Background(), succeededDeploy())
	r.OperationEnded(context.Background(), failedDeploy())
	r.Wait()
	texts, calls := tg.texts(), hook.received()
	if len(texts) != 1 || len(calls) != 1 {
		t.Fatalf("telegram %q, webhook %d calls", texts, len(calls))
	}
	call := calls[0]
	if call.method != http.MethodPost || call.path != hookPath || call.header.Get("Content-Type") != "application/json" || call.header.Get("Authorization") != "Bearer hook-secret-value" {
		t.Fatalf("request %s %s %v", call.method, call.path, call.header)
	}
	want := map[string]any{
		"environment": "prod", "kind": "deploy", "name": "api", "state": "rolled_back",
		"detail": "role web: container never became healthy (last check: exit 1)",
		"since":  "2026-11-18T10:00:48Z", "text": texts[0],
	}
	if len(call.body) != len(want) {
		t.Fatalf("body %v", call.body)
	}
	for k, v := range want {
		if call.body[k] != v {
			t.Errorf("%s = %v, want %v", k, call.body[k], v)
		}
	}
	if call.body["text"] != deployAlertText(failedDeploy()) {
		t.Errorf("the webhook text is not the Telegram text: %v", call.body["text"])
	}
}

func TestWebhookFollowsTheDeploysPolicyOfTheAlertsBlock(t *testing.T) {
	for _, tc := range []struct {
		member string
		events []deploy.OperationEvent
		want   int
	}{
		{``, []deploy.OperationEvent{succeededDeploy()}, 0},
		{`,"deploys":"all"`, []deploy.OperationEvent{succeededDeploy()}, 1},
		{`,"deploys":"all"`, []deploy.OperationEvent{succeededDeploy(), failedDeploy()}, 2},
		{`,"deploys":"none"`, []deploy.OperationEvent{succeededDeploy(), failedDeploy()}, 0},
	} {
		tg, hook := newReportsTelegram(t), newHookReceiver(t)
		r := NewReports(monitoringConfig(tg.srv.URL), bothChannelsStore(t, hook.url(), "", tc.member), nil)
		for _, e := range tc.events {
			r.OperationEnded(context.Background(), e)
		}
		r.Wait()
		if got := len(hook.received()); got != tc.want || len(tg.texts()) != tc.want {
			t.Errorf("%q: %d posts and %d messages, want %d each", tc.member, got, len(tg.texts()), tc.want)
		}
	}
	// A success is told as a deploy that succeeded, with its version.
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	r := NewReports(monitoringConfig(tg.srv.URL), bothChannelsStore(t, hook.url(), "", `,"deploys":"all"`), nil)
	r.OperationEnded(context.Background(), succeededDeploy())
	r.Wait()
	if calls := hook.received(); len(calls) != 1 || calls[0].body["state"] != "succeeded" || calls[0].body["detail"] != "v1.4.2" || calls[0].body["kind"] != "deploy" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestAWebhookThatDoesNotAnswerNeitherDelaysADeployNorHoldsBackTelegram(t *testing.T) {
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	hook.block = make(chan struct{})
	r := NewReports(monitoringConfig(tg.srv.URL), bothChannelsStore(t, hook.url(), "", ""), nil)
	started := time.Now()
	r.OperationEnded(context.Background(), failedDeploy())
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the operation waited %v for its alert", took)
	}
	waitUntil(t, "the Telegram message", func() bool { return len(tg.texts()) == 1 })
	waitUntil(t, "the webhook request", func() bool { return len(hook.received()) == 1 })
	close(hook.block)
	r.Wait()
}

func TestWebhookFailuresAreLoggedWithoutTheURLOrTheHeaders(t *testing.T) {
	for name, setup := range map[string]func(*hookReceiver) string{
		"an error status": func(h *hookReceiver) string { h.status.Store(http.StatusInternalServerError); return h.url() },
		"nothing listening": func(h *hookReceiver) string {
			return "http://" + dialtest.Refused + hookPath
		},
	} {
		tg, hook := newReportsTelegram(t), newHookReceiver(t)
		url := setup(hook)
		var mu sync.Mutex
		var logged []string
		cfg := monitoringConfig(tg.srv.URL)
		cfg.dial = dialtest.Dial
		r := NewReports(cfg, bothChannelsStore(t, url, "", ""), func(s string) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, s)
		})
		r.OperationEnded(context.Background(), failedDeploy())
		r.Wait()
		mu.Lock()
		if len(logged) != 1 || !strings.Contains(logged[0], "deploy alert not sent") || !strings.Contains(logged[0], "webhook") {
			t.Errorf("%s: logged %q", name, logged)
		}
		for _, line := range logged {
			for _, secret := range []string{"s3cr3t", "hook-secret-value", "127.0.0.1", "dialtest.invalid", reportsBotToken} {
				if strings.Contains(line, secret) {
					t.Errorf("%s: the log line leaks %q: %q", name, secret, line)
				}
			}
		}
		mu.Unlock()
		if len(tg.texts()) != 1 {
			t.Errorf("%s: a failed webhook cost Telegram its message: %q", name, tg.texts())
		}
	}
}

func TestAFailingProbeIsOnePostAndOneTelegramMessage(t *testing.T) {
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	probes := `,"probes":{"site":{"http":"http://` + dialtest.Refused + `/health","failures":1}}`
	s := bothChannelsStore(t, hook.url(), probes, "")
	var mu sync.Mutex
	var logged []string
	cfg := monitoringConfig(tg.srv.URL)
	cfg.dial = dialtest.Dial
	w := NewMonitoring(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, line)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitUntil(t, "the alert on both channels", func() bool { return len(tg.texts()) >= 1 && len(hook.received()) >= 1 })
	time.Sleep(300 * time.Millisecond)
	texts, calls := tg.texts(), hook.received()
	if len(texts) != 1 || len(calls) != 1 {
		t.Fatalf("telegram %q, webhook %d calls", texts, len(calls))
	}
	body := calls[0].body
	if body["kind"] != "check" || body["name"] != "probe:site" || body["state"] != "critical" || body["environment"] != "lab" || body["text"] != texts[0] || body["since"] == nil {
		t.Fatalf("body %v", body)
	}
	if !strings.HasPrefix(texts[0], "🔴 lab probe:site: ") {
		t.Fatalf("text %q", texts[0])
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "s3cr3t") {
			t.Fatalf("a log line leaks the URL: %q", line)
		}
	}
}

func TestTheMonitorsReminderIntervalIsTheAlertsBlocks(t *testing.T) {
	hook := newHookReceiver(t)
	s := bothChannelsStore(t, hook.url(), "", `,"reminder":"90m"`)
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	n, reminder, err := w.notifier(context.Background())
	if err != nil || n == nil || reminder != 90*time.Minute {
		t.Fatalf("notifier %v, reminder %v, error %v", n, reminder, err)
	}
	if got := monitor.ChannelName(n.(monitor.Notifiers)[0]) + "," + monitor.ChannelName(n.(monitor.Notifiers)[1]); got != "telegram,webhook" {
		t.Fatalf("channels %s", got)
	}
}

// testAlertChannels runs alerts test through the handler and returns the status and the per-channel results.
func testAlertChannels(t *testing.T, h http.Handler) (int, []map[string]any) {
	t.Helper()
	w := serveRequest(t, h, http.MethodPost, "/v1/projects/demo/environments/lab/alerts/test", monitoringToken("b"))
	var body struct {
		Channels []map[string]any `json:"channels"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("answer %s: %v", w.Body.String(), err)
		}
	}
	for _, leak := range []string{"s3cr3t", "hook-secret-value", reportsBotToken} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("the answer leaks %q: %s", leak, w.Body.String())
		}
	}
	return w.Code, body.Channels
}

func TestAlertsTestReportsEachChannel(t *testing.T) {
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	s := bothChannelsStore(t, hook.url(), "", "")
	h := NewDeploymentHandler(monitoringConfig(tg.srv.URL), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})

	code, channels := testAlertChannels(t, h)
	if code != http.StatusOK || len(channels) != 2 || channels[0]["channel"] != "telegram" || channels[0]["ok"] != true || channels[1]["channel"] != "webhook" || channels[1]["ok"] != true {
		t.Fatalf("both channels: %d %v", code, channels)
	}
	if texts := tg.texts(); len(texts) != 1 || texts[0] != "🧪 lab test alert from berthy" {
		t.Fatalf("telegram %q", texts)
	}
	calls := hook.received()
	if len(calls) != 1 || calls[0].body["text"] != "🧪 lab test alert from berthy" || calls[0].body["kind"] != "test" || calls[0].body["environment"] != "lab" {
		t.Fatalf("webhook %+v", calls)
	}

	hook.status.Store(http.StatusBadGateway)
	code, channels = testAlertChannels(t, h)
	if code != http.StatusOK || channels[0]["ok"] != true || channels[1]["ok"] != false || channels[1]["error"] != "webhook returned HTTP 502" {
		t.Fatalf("webhook down: %d %v", code, channels)
	}
	rows, err := alertAudits(s)
	if err != nil || len(rows) != 2 || rows[0].Outcome != "failed" || rows[1].Outcome != "succeeded" {
		t.Fatalf("audit: %+v %v", rows, err)
	}
}

func TestAlertsTestWithOnlyAWebhook(t *testing.T) {
	hook := newHookReceiver(t)
	s := openIngressStore(t)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "ALERT_WEBHOOK_URL"}, []byte(hook.url())); err != nil {
		t.Fatal(err)
	}
	recordRelease(t, s, "hook", monitoredManifest(`,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL"}}`))
	h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	code, channels := testAlertChannels(t, h)
	if code != http.StatusOK || len(channels) != 1 || channels[0]["channel"] != "webhook" || channels[0]["ok"] != true {
		t.Fatalf("%d %v", code, channels)
	}
	if calls := hook.received(); len(calls) != 1 || calls[0].header.Get("Authorization") != "" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestAlertsTestNamesAMissingOrBrokenSecretPerChannel(t *testing.T) {
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	s := bothChannelsStore(t, hook.url(), "", "")
	ctx := context.Background()
	h := NewDeploymentHandler(monitoringConfig(tg.srv.URL), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	put := func(name, value string) {
		t.Helper()
		if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}

	put("ALERT_WEBHOOK_HEADERS", "no equals sign hook-secret-value")
	code, channels := testAlertChannels(t, h)
	if code != http.StatusOK || channels[0]["ok"] != true || channels[1]["ok"] != false || channels[1]["error"] != "webhook headers secret ALERT_WEBHOOK_HEADERS is malformed: header entry 1: expected name=value" {
		t.Fatalf("malformed headers: %d %v", code, channels)
	}

	put("ALERT_WEBHOOK_HEADERS", "Authorization=Bearer x")
	put("ALERT_WEBHOOK_URL", "ftp://hooks.example.com/s3cr3t")
	_, channels = testAlertChannels(t, h)
	if channels[1]["ok"] != false || channels[1]["error"] != "webhook url secret ALERT_WEBHOOK_URL is not an http or https URL" {
		t.Fatalf("bad URL: %v", channels)
	}

	if err := s.DeleteSecret(ctx, state.Actor{ID: "admin"}, "ALERT_WEBHOOK_URL"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, state.Actor{ID: "admin"}, "TELEGRAM_BOT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	code, channels = testAlertChannels(t, h)
	if code != http.StatusOK || channels[0]["error"] != "telegram bot token secret missing" || channels[1]["error"] != "webhook url secret missing" {
		t.Fatalf("missing secrets: %d %v", code, channels)
	}
}

func TestDoctorAndTheMonitorCheckTheWebhookSecrets(t *testing.T) {
	tg, hook := newReportsTelegram(t), newHookReceiver(t)
	s := bothChannelsStore(t, hook.url(), "", "")
	ctx := context.Background()
	doctor := func() map[string]api.Check {
		t.Helper()
		h := NewDeploymentHandler(monitoringConfig(tg.srv.URL), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
		w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/doctor", monitoringToken("a"))
		var body struct {
			Checks []api.Check `json:"checks"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		found := map[string]api.Check{}
		for _, c := range body.Checks {
			found[c.Name] = c
		}
		return found
	}
	monitored := func() map[string]monitor.Check {
		t.Helper()
		w := NewMonitoring(monitoringConfig(tg.srv.URL), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
		out := map[string]monitor.Check{}
		for _, c := range w.checks(ctx) {
			out[c.Name()] = c
		}
		return out
	}

	found, checks := doctor(), monitored()
	if c := found["webhook"]; !c.OK || c.Critical || c.Detail != "url and headers secrets present" {
		t.Errorf("doctor webhook: %+v", c)
	}
	if c := found["telegram"]; !c.OK || c.Detail != "bot token secret present" {
		t.Errorf("doctor telegram: %+v", c)
	}
	if got := checks["webhook"].Run(ctx); got.State != monitor.OK {
		t.Errorf("monitor webhook: %+v", got)
	}

	if err := s.DeleteSecret(ctx, state.Actor{ID: "admin"}, "ALERT_WEBHOOK_HEADERS"); err != nil {
		t.Fatal(err)
	}
	found, checks = doctor(), monitored()
	if c := found["webhook"]; c.OK || c.Critical || c.Detail != "webhook headers secret missing" {
		t.Errorf("doctor, headers missing: %+v", c)
	}
	if got := checks["webhook"].Run(ctx); got.State != monitor.Warning || got.Detail != "webhook headers secret missing" {
		t.Errorf("monitor, headers missing: %+v", got)
	}

	if err := s.DeleteSecret(ctx, state.Actor{ID: "admin"}, "ALERT_WEBHOOK_URL"); err != nil {
		t.Fatal(err)
	}
	if c := doctor()["webhook"]; c.OK || c.Detail != "webhook url secret missing" {
		t.Errorf("doctor, url missing: %+v", c)
	}
}
