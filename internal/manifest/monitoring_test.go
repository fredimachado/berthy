package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// monitoringManifest returns a valid JSON manifest with one image app, one role, and a compose app, plus the given
// top-level members.
func monitoringManifest(members string) string {
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},"db":{"kind":"compose","file":"db/compose.yml"}}` + members + `}`
}

// twoRoleManifest is a valid manifest whose image app has two roles, so a service probe must name one.
func twoRoleManifest(members string) string {
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{},"worker":{}}}}` + members + `}`
}

func TestServiceProbeNeedsRoleWhenAppHasSeveralRoles(t *testing.T) {
	for name, tc := range map[string]struct {
		members string
		valid   bool
	}{
		"role named":        {`,"probes":{"x":{"service":{"app":"api","role":"worker","port":80}}}`, true},
		"role omitted":      {`,"probes":{"x":{"service":{"app":"api","port":80}}}`, false},
		"role not an app's": {`,"probes":{"x":{"service":{"app":"api","role":"cron","port":80}}}`, false},
	} {
		_, err := DecodeJSON([]byte(twoRoleManifest(tc.members)))
		if (err == nil) != tc.valid {
			t.Errorf("%s: valid=%v err=%v", name, tc.valid, err)
		}
	}
}

func TestMonitoringValidation(t *testing.T) {
	valid := map[string]string{
		"http defaults":           `,"probes":{"site":{"http":"https://app.example.com/health"}}`,
		"http with status list":   `,"probes":{"site":{"http":"http://127.0.0.1:8080/","expect_status":[200,204]}}`,
		"tcp probe":               `,"probes":{"db-port":{"tcp":"10.0.0.5:5432"}}`,
		"service image role":      `,"probes":{"web":{"service":{"app":"api","port":8080,"path":"/health"}}}`,
		"service compose service": `,"probes":{"pg":{"service":{"app":"db","role":"postgres","port":5432}}}`,
		"service timeout 1s":      `,"probes":{"web":{"service":{"app":"api","port":8080},"timeout":"1s"}}`,
		"service timeout 25s":     `,"probes":{"web":{"service":{"app":"api","port":8080},"interval":"60s","timeout":"25s"}}`,
		"explicit intervals":      `,"probes":{"site":{"http":"http://h:80/","interval":"5s","timeout":"4s","failures":1}}`,
		"peers":                   `,"peers":{"prod":"https://prod.example.com:443","staging":"http://10.0.0.6:8080/"}`,
		"telegram":                `,"alerts":{"reminder":"0s","telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"-1001234567890"}}`,
		"webhook":                 `,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL"}}`,
		"webhook with headers":    `,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL","headers_secret":"ALERT_WEBHOOK_HEADERS"}}`,
		"both channels":           `,"alerts":{"deploys":"all","telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1"},"webhook":{"url_secret":"ALERT_WEBHOOK_URL"}}`,
		"heartbeat":               `,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL"}}`,
		"heartbeat at the least":  `,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"10s"}}`,
		"heartbeat every hour":    `,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"1h"}}`,
		"telegram channel":        `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"@berthy_alerts"}}`,
		"empty alerts":            `,"alerts":{}`,
	}
	for name, members := range valid {
		if _, err := DecodeJSON([]byte(monitoringManifest(members))); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	invalid := map[string]string{
		"no probe kind":                      `,"probes":{"x":{}}`,
		"two probe kinds":                    `,"probes":{"x":{"http":"http://h/","tcp":"h:80"}}`,
		"probe name":                         `,"probes":{"Bad Name":{"http":"http://h/"}}`,
		"http scheme":                        `,"probes":{"x":{"http":"ftp://h/"}}`,
		"http userinfo":                      `,"probes":{"x":{"http":"http://user:secret@h/"}}`,
		"http without host":                  `,"probes":{"x":{"http":"http:///path"}}`,
		"http relative":                      `,"probes":{"x":{"http":"/health"}}`,
		"tcp without port":                   `,"probes":{"x":{"tcp":"host"}}`,
		"tcp port zero":                      `,"probes":{"x":{"tcp":"host:0"}}`,
		"tcp port too large":                 `,"probes":{"x":{"tcp":"host:70000"}}`,
		"tcp without host":                   `,"probes":{"x":{"tcp":":80"}}`,
		"tcp port not digits":                `,"probes":{"x":{"tcp":"host:http"}}`,
		"service unknown app":                `,"probes":{"x":{"service":{"app":"missing","port":80}}}`,
		"service unknown role":               `,"probes":{"x":{"service":{"app":"api","role":"worker","port":80}}}`,
		"service port zero":                  `,"probes":{"x":{"service":{"app":"api","port":0}}}`,
		"service timeout below the helper":   `,"probes":{"web":{"service":{"app":"api","port":8080},"timeout":"500ms"}}`,
		"service timeout above the budget":   `,"probes":{"web":{"service":{"app":"api","port":8080},"interval":"60s","timeout":"30s"}}`,
		"service path relative":              `,"probes":{"x":{"service":{"app":"api","port":80,"path":"health"}}}`,
		"expect below 100":                   `,"probes":{"x":{"http":"http://h/","expect_status":[99]}}`,
		"expect above 599":                   `,"probes":{"x":{"http":"http://h/","expect_status":[600]}}`,
		"interval below 5s":                  `,"probes":{"x":{"http":"http://h/","interval":"4s"}}`,
		"interval not a duration":            `,"probes":{"x":{"http":"http://h/","interval":"often"}}`,
		"timeout zero":                       `,"probes":{"x":{"http":"http://h/","timeout":"0s"}}`,
		"timeout not below default interval": `,"probes":{"x":{"http":"http://h/","timeout":"30s"}}`,
		"default timeout at 5s interval":     `,"probes":{"x":{"http":"http://h/","interval":"5s"}}`,
		"failures zero":                      `,"probes":{"x":{"http":"http://h/","failures":0}}`,
		"peer scheme":                        `,"peers":{"prod":"ftp://h:21"}`,
		"peer without port":                  `,"peers":{"prod":"https://prod.example.com"}`,
		"peer with path":                     `,"peers":{"prod":"https://prod.example.com:443/api"}`,
		"peer with user":                     `,"peers":{"prod":"http://user@h:8080"}`,
		"peer with query":                    `,"peers":{"prod":"http://h:8080?x=1"}`,
		"peer name":                          `,"peers":{"Prod East":"http://h:8080"}`,
		"telegram reserved token":            `,"alerts":{"telegram":{"bot_token_secret":"TUNNEL_TOKEN","chat_id":"1"}}`,
		"telegram secret name":               `,"alerts":{"telegram":{"bot_token_secret":"bot-token","chat_id":"1"}}`,
		"telegram chat id text":              `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"chat one"}}`,
		"telegram short channel":             `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"@ab"}}`,
		"negative reminder":                  `,"alerts":{"reminder":"-1h"}`,
		"reminder text":                      `,"alerts":{"reminder":"soon"}`,
		"the old telegram reminder":          `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1","reminder":"1h"}}`,
		"the old telegram deploys":           `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1","deploys":"all"}}`,
		"webhook without a url secret":       `,"alerts":{"webhook":{}}`,
		"webhook url secret name":            `,"alerts":{"webhook":{"url_secret":"alert-url"}}`,
		"webhook reserved url secret":        `,"alerts":{"webhook":{"url_secret":"TUNNEL_TOKEN"}}`,
		"webhook reserved headers secret":    `,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL","headers_secret":"TUNNEL_TOKEN"}}`,
		"webhook headers secret name":        `,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL","headers_secret":"bad name"}}`,
		"webhook url in the manifest":        `,"alerts":{"webhook":{"url":"https://hooks.example.com/x"}}`,
		"heartbeat without a url secret":     `,"alerts":{"heartbeat":{}}`,
		"heartbeat url secret name":          `,"alerts":{"heartbeat":{"url_secret":"beat url"}}`,
		"heartbeat reserved url secret":      `,"alerts":{"heartbeat":{"url_secret":"TUNNEL_TOKEN"}}`,
		"heartbeat interval below 10s":       `,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"9s"}}`,
		"heartbeat interval zero":            `,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"0s"}}`,
		"heartbeat interval text":            `,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"often"}}`,
		"heartbeat url in the manifest":      `,"alerts":{"heartbeat":{"url":"https://hc.example.com/x"}}`,
	}
	for name, members := range invalid {
		if _, err := DecodeJSON([]byte(monitoringManifest(members))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMonitoringRejectsUnknownFieldsInJSON(t *testing.T) {
	for name, members := range map[string]string{
		"probe field":    `,"probes":{"x":{"http":"http://h/","bogus":1}}`,
		"service field":  `,"probes":{"x":{"service":{"app":"api","port":80,"bogus":true}}}`,
		"telegram field": `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1","bogus":1}}}`,
		"webhook field":  `,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL","bogus":1}}}`,
		"alerts field":   `,"alerts":{"email":{}}`,
	} {
		if _, err := DecodeJSON([]byte(monitoringManifest(members))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMonitoringRejectsUnknownFieldsInYAML(t *testing.T) {
	base := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	for name, block := range map[string]string{
		"probe field":    "probes:\n  site:\n    http: http://h/\n    retries: 3\n",
		"telegram field": "alerts:\n  telegram:\n    bot_token_secret: TELEGRAM_BOT_TOKEN\n    chat_id: \"1\"\n    unknown: x\n",
		"old reminder":   "alerts:\n  telegram:\n    bot_token_secret: TELEGRAM_BOT_TOKEN\n    chat_id: \"1\"\n    reminder: 1h\n",
		"peers nested":   "peers:\n  prod:\n    url: http://h:8080\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base+block), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, "dev"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	dir := t.TempDir()
	valid := base + "probes:\n  site:\n    http: https://app.example.com/health\n    expect_status: [200]\npeers:\n  prod: http://10.0.0.5:8080\nalerts:\n  telegram:\n    bot_token_secret: TELEGRAM_BOT_TOKEN\n    chat_id: \"-1001234567890\"\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "dev")
	if err != nil {
		t.Fatalf("valid YAML refused: %v", err)
	}
	if m.Probes["site"].HTTP != "https://app.example.com/health" || m.Peers["prod"] != "http://10.0.0.5:8080" || m.Alerts.Telegram.ChatID != "-1001234567890" {
		t.Fatalf("monitoring not loaded: %+v", m)
	}
}

func TestMonitoringDefaultsAndAccessors(t *testing.T) {
	m, err := DecodeJSON([]byte(monitoringManifest(`,"probes":{"site":{"http":"http://h/"},"fast":{"tcp":"h:80","interval":"10s","timeout":"2s","failures":2}},"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	site := m.Probes["site"]
	if site.Interval != "30s" || site.Timeout != "5s" || site.Failures == nil || *site.Failures != 3 {
		t.Fatalf("defaults not applied: %+v", site)
	}
	if interval, timeout := site.Durations(); interval != 30*time.Second || timeout != 5*time.Second || site.FailureLimit() != 3 {
		t.Fatalf("site durations %v %v %d", interval, timeout, site.FailureLimit())
	}
	if interval, timeout := m.Probes["fast"].Durations(); interval != 10*time.Second || timeout != 2*time.Second || m.Probes["fast"].FailureLimit() != 2 {
		t.Fatalf("fast durations %v %v", interval, timeout)
	}
	if got := m.Alerts.ReminderInterval(); got != 4*time.Hour || m.Alerts.Reminder != "4h0m0s" {
		t.Fatalf("reminder %v (%q)", got, m.Alerts.Reminder)
	}
	if (Alerts{Reminder: "0s"}).ReminderInterval() != 0 {
		t.Fatal("0s reminder should disable reminders")
	}
	if (Probe{}).FailureLimit() != 3 {
		t.Fatal("zero probe should use the default failure limit")
	}
}

func TestAlertsDefaultsApplyWhateverTheChannels(t *testing.T) {
	m, err := DecodeJSON([]byte(monitoringManifest(`,"alerts":{"webhook":{"url_secret":"ALERT_WEBHOOK_URL"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Alerts.Reminder != "4h0m0s" || m.Alerts.Deploys != "failed" || m.Alerts.Telegram != nil || m.Alerts.Webhook.HeadersSecret != "" {
		t.Fatalf("alerts %+v", m.Alerts)
	}
}

func TestHeartbeatIntervalDefaultsToAMinuteAndIsAtLeastTenSeconds(t *testing.T) {
	m, err := DecodeJSON([]byte(monitoringManifest(`,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if hb := m.Alerts.Heartbeat; hb.Interval != "1m0s" || hb.Every() != time.Minute {
		t.Fatalf("heartbeat %+v every %v", hb, hb.Every())
	}
	m, err = DecodeJSON([]byte(monitoringManifest(`,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"10s"}}`)))
	if err != nil || m.Alerts.Heartbeat.Every() != 10*time.Second {
		t.Fatalf("10s: %v %+v", err, m.Alerts)
	}
	if (Heartbeat{Interval: "1s"}).Every() != time.Minute || (Heartbeat{}).Every() != time.Minute {
		t.Fatal("an unusable interval should fall back to the default")
	}
}

func TestAlertsDeploysDefaultsToFailedAndTakesAllOrNone(t *testing.T) {
	telegram := func(extra string) string {
		return `,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1"}` + extra + `}`
	}
	for extra, want := range map[string]string{
		``:                    "failed",
		`,"deploys":"failed"`: "failed",
		`,"deploys":"all"`:    "all",
		`,"deploys":"none"`:   "none",
	} {
		m, err := DecodeJSON([]byte(monitoringManifest(telegram(extra))))
		if err != nil {
			t.Errorf("%q refused: %v", extra, err)
			continue
		}
		if got := m.Alerts.Deploys; got != want {
			t.Errorf("%q: deploys %q, want %q", extra, got, want)
		}
	}
	for _, extra := range []string{`,"deploys":"always"`, `,"deploys":"ALL"`, `,"deploys":"success"`, `,"deploys":true`} {
		if _, err := DecodeJSON([]byte(monitoringManifest(telegram(extra)))); err == nil {
			t.Errorf("%q accepted", extra)
		}
	}
}
