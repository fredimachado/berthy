package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestHostAlertNamesServerOnBothChannels(t *testing.T) {
	for _, tc := range []struct{ name, env, server, check, detail, text string }{
		{"environment", "lab", "", "probe:site", "HTTP 503", "🟠 demo/lab probe:site: HTTP 503"},
		{"server", "", "hills", "public_ports", "published port", "🟠 server hills public_ports: published port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, openStore(t))
			r.m.Env = tc.env
			r.m.Server = tc.server
			if tc.server != "" {
				r.m.Project = ""
			}
			ch := &scripted{name: tc.check, interval: time.Minute, failures: 1}
			ch.set(Warning, tc.detail)
			r.checks = []Check{ch}
			tg := &recordTransport{reply: `{"ok":true}`}
			hook := &recordTransport{}
			r.m.Notifier = func(context.Context) (Notifier, time.Duration, error) {
				return Notifiers{
					&Telegram{Token: testToken, ChatID: "1", HTTP: &http.Client{Transport: tg}},
					&Webhook{URL: "https://example.test/alerts", Client: &http.Client{Transport: hook}},
				}, 0, nil
			}
			r.step(0)
			if row := r.stored(tc.check); row.NotifiedState != Warning {
				t.Fatalf("notification not recorded: %+v", row)
			}
			readBody := func(req *http.Request) []byte {
				t.Helper()
				if req == nil {
					t.Fatal("channel received no request")
				}
				copy, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				defer copy.Close()
				body, err := io.ReadAll(copy)
				if err != nil {
					t.Fatal(err)
				}
				return body
			}
			form, err := url.ParseQuery(string(readBody(tg.last)))
			if err != nil || form.Get("text") != tc.text {
				t.Errorf("Telegram text = %q, want %q (%v)", form.Get("text"), tc.text, err)
			}
			want := map[string]string{
				"environment": tc.env, "kind": "check", "name": tc.check, "state": "warning", "detail": tc.detail,
				"since": r.now.Format(time.RFC3339), "text": tc.text,
			}
			if tc.server != "" {
				want["server"] = tc.server
			} else {
				want["project"] = "demo"
			}
			var got map[string]string
			if err := json.Unmarshal(readBody(hook.last), &got); err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("webhook = %v, want %v (%v)", got, want, err)
			}
		})
	}
}
