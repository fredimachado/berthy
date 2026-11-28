package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

const reportsBotToken = "123456789:TEST-bot-token-not-real"

// reportsTelegram is a Telegram Bot API that records the text of every message. status is the HTTP status it answers with.
type reportsTelegram struct {
	srv      *httptest.Server
	status   atomic.Int32
	mu       sync.Mutex
	messages []string
}

func newReportsTelegram(t *testing.T) *reportsTelegram {
	t.Helper()
	f := &reportsTelegram{}
	f.status.Store(http.StatusOK)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if code := int(f.status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
			return
		}
		f.mu.Lock()
		f.messages = append(f.messages, r.PostForm.Get("text"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *reportsTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messages...)
}

// alertStore returns a store whose last applied manifest sends alerts to Telegram, with the given extra members of the alerts
// block, and that holds the bot token.
func alertStore(t *testing.T, deploysMember string) *state.Store {
	t.Helper()
	s := openIngressStore(t)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "TELEGRAM_BOT_TOKEN"}, []byte(reportsBotToken)); err != nil {
		t.Fatal(err)
	}
	recordRelease(t, s, "alerts", monitoredManifest(`,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"-1001234567890"}`+deploysMember+`}`))
	return s
}

func failedDeploy() deploy.OperationEvent {
	start := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	return deploy.OperationEvent{
		OperationID: "op123", Environment: "prod", Kind: "deploy", App: "api", Actor: "ci", Version: "v1.4.3",
		State: "rolled_back", Reason: "role web: container never became healthy (last check: exit 1)", Err: "rollout failed and was cleaned up",
		Started: start, Ended: start.Add(48 * time.Second),
	}
}

func succeededDeploy() deploy.OperationEvent {
	e := failedDeploy()
	e.State, e.Reason, e.Err, e.Version = "succeeded", "", "", "v1.4.2"
	return e
}

func TestADeployReportNamesTheProject(t *testing.T) {
	failed := failedDeploy()
	for name, tc := range map[string]struct {
		mutate func(*deploy.OperationEvent)
		want   string
	}{
		"success": {func(e *deploy.OperationEvent) { *e = succeededDeploy() }, "✅ demo/lab: api v1.4.2 deployed by ci in 48s"},
		"rolled back": {func(*deploy.OperationEvent) {},
			"🔴 demo/lab: api v1.4.3 rolled back: role web: container never became healthy (last check: exit 1)\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"failed": {func(e *deploy.OperationEvent) { e.State = "failed" },
			"🔴 demo/lab: api v1.4.3 failed: role web: container never became healthy (last check: exit 1)\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"interrupted": {func(e *deploy.OperationEvent) { e.State = "interrupted" },
			"🔴 demo/lab: api v1.4.3 interrupted: role web: container never became healthy (last check: exit 1)\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"recovery required": {func(e *deploy.OperationEvent) { e.State = "recovery_required" },
			"🔴 demo/lab: api v1.4.3 needs recovery: role web: container never became healthy (last check: exit 1)\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"no reason falls back to the public category": {func(e *deploy.OperationEvent) { e.State, e.Reason = "failed", "" },
			"🔴 demo/lab: api v1.4.3 failed: rollout failed and was cleaned up\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"private category is not shown": {func(e *deploy.OperationEvent) {
			e.State, e.Reason, e.Err = "failed", "", "dial tcp 10.0.0.9:5432: refused"
		},
			"🔴 demo/lab: api v1.4.3 failed: operation failed\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"reason with application output is dropped": {func(e *deploy.OperationEvent) { e.State, e.Reason, e.Err = "failed", "caf\u00e9\nsecret=1", "" },
			"🔴 demo/lab: api v1.4.3 failed\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"no version": {func(e *deploy.OperationEvent) { e.Version = "" },
			"🔴 demo/lab: api rolled back: role web: container never became healthy (last check: exit 1)\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"short digest": {func(e *deploy.OperationEvent) { *e = succeededDeploy(); e.Version = "sha256:0123456789ab" },
			"✅ demo/lab: api sha256:0123456789ab deployed by ci in 48s"},
		"minutes": {func(e *deploy.OperationEvent) { *e = succeededDeploy(); e.Ended = e.Started.Add(125 * time.Second) },
			"✅ demo/lab: api v1.4.2 deployed by ci in 2m5s"},
		"rollback success": {func(e *deploy.OperationEvent) { *e = succeededDeploy(); e.Kind = "rollback"; e.Version = "v1.0.1" },
			"✅ demo/lab: api rolled back to v1.0.1 by ci in 48s"},
		"rollback failure": {func(e *deploy.OperationEvent) { e.Kind = "rollback"; e.State = "failed"; e.Version = "v1.0.1" },
			"🔴 demo/lab: api rollback to v1.0.1 failed: role web: container never became healthy (last check: exit 1)\nby ci in 48s\nberthy diagnostics op123 -e lab --project demo"},
		"control characters in the actor": {func(e *deploy.OperationEvent) { *e = succeededDeploy(); e.Actor = "ci\nbot" },
			"✅ demo/lab: api v1.4.2 deployed by ci bot in 48s"},
	} {
		e := failed
		tc.mutate(&e)
		if got := deployAlertText(e); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

func TestWantsDeployAlert(t *testing.T) {
	states := []string{"succeeded", "failed", "rolled_back", "interrupted", "recovery_required"}
	for _, tc := range []struct {
		deploys string
		want    map[string]bool
	}{
		{"", map[string]bool{"failed": true, "rolled_back": true, "interrupted": true, "recovery_required": true}},
		{"failed", map[string]bool{"failed": true, "rolled_back": true, "interrupted": true, "recovery_required": true}},
		{"all", map[string]bool{"succeeded": true, "failed": true, "rolled_back": true, "interrupted": true, "recovery_required": true}},
		{"none", map[string]bool{}},
	} {
		for _, st := range states {
			for _, kind := range []string{"deploy", "rollback"} {
				e := deploy.OperationEvent{Kind: kind, State: st}
				if got := wantsDeployAlert(tc.deploys, e); got != tc.want[st] {
					t.Errorf("deploys %q, %s %s: %v, want %v", tc.deploys, kind, st, got, tc.want[st])
				}
			}
		}
	}
	for _, kind := range []string{"restart", "scale", "exec", "run", "remove", "destroy", "prune", "ingress", "restore"} {
		if wantsDeployAlert("all", deploy.OperationEvent{Kind: kind, State: "failed"}) {
			t.Errorf("a %s is alerted", kind)
		}
	}
	if wantsDeployAlert("all", deploy.OperationEvent{Kind: "deploy", State: "failed", Resumed: true}) {
		t.Error("the later end of a recovery_required operation is alerted again")
	}
}

func TestDeployReportsSendOneMessageForAFailedDeployUnderTheDefaultPolicy(t *testing.T) {
	tg := newReportsTelegram(t)
	r := NewReports(monitoringConfig(tg.srv.URL), alertStore(t, ""), nil)
	r.OperationEnded(context.Background(), succeededDeploy())
	r.OperationEnded(context.Background(), failedDeploy())
	r.Wait()
	got := tg.texts()
	if len(got) != 1 {
		t.Fatalf("messages %q", got)
	}
	for _, want := range []string{"demo/lab", "api", "v1.4.3", "by ci", "container never became healthy (last check: exit 1)", "berthy diagnostics op123 -e lab --project demo", "48s"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("message lacks %q: %q", want, got[0])
		}
	}
}

func TestDeployReportsPolicies(t *testing.T) {
	for _, tc := range []struct {
		member string
		events []deploy.OperationEvent
		want   []string
	}{
		{`,"deploys":"failed"`, []deploy.OperationEvent{succeededDeploy()}, nil},
		{`,"deploys":"all"`, []deploy.OperationEvent{succeededDeploy()}, []string{"✅ demo/lab: api v1.4.2 deployed by ci in 48s"}},
		{`,"deploys":"all"`, []deploy.OperationEvent{failedDeploy()}, []string{deployAlertText(failedDeploy())}},
		{`,"deploys":"none"`, []deploy.OperationEvent{succeededDeploy(), failedDeploy()}, nil},
	} {
		tg := newReportsTelegram(t)
		r := NewReports(monitoringConfig(tg.srv.URL), alertStore(t, tc.member), nil)
		for _, e := range tc.events {
			r.OperationEnded(context.Background(), e)
		}
		r.Wait()
		if got := tg.texts(); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: messages %q, want %q", tc.member, got, tc.want)
		}
	}
}

func TestDeployReportsNeverSendForOtherKindsOrForARecoveryThatWasAlreadyAlerted(t *testing.T) {
	tg := newReportsTelegram(t)
	r := NewReports(monitoringConfig(tg.srv.URL), alertStore(t, `,"deploys":"all"`), nil)
	restart := failedDeploy()
	restart.Kind = "restart"
	resumed := failedDeploy()
	resumed.Resumed = true
	r.OperationEnded(context.Background(), restart)
	r.OperationEnded(context.Background(), resumed)
	r.OperationStarted(context.Background(), deploy.OperationEvent{Kind: "deploy", State: "running"})
	r.Wait()
	if got := tg.texts(); len(got) != 0 {
		t.Fatalf("messages %q", got)
	}
}

func TestDeployReportsFollowTheManifestOfTheEvent(t *testing.T) {
	// The last applied manifest says none, the request's manifest says failed: the request's decides.
	tg := newReportsTelegram(t)
	r := NewReports(monitoringConfig(tg.srv.URL), alertStore(t, `,"deploys":"none"`), nil)
	e := failedDeploy()
	e.Manifest = &manifest.Manifest{Alerts: &manifest.Alerts{Deploys: "failed", Telegram: &manifest.Telegram{BotTokenSecret: "TELEGRAM_BOT_TOKEN", ChatID: "-1001234567890"}}}
	r.OperationEnded(context.Background(), e)
	r.Wait()
	if got := tg.texts(); len(got) != 1 {
		t.Fatalf("messages %q", got)
	}

	// A first deploy that fails has no release: nothing was applied, and the request's manifest still sends the alert.
	tg = newReportsTelegram(t)
	empty := openIngressStore(t)
	if err := empty.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "TELEGRAM_BOT_TOKEN"}, []byte(reportsBotToken)); err != nil {
		t.Fatal(err)
	}
	r = NewReports(monitoringConfig(tg.srv.URL), empty, nil)
	r.OperationEnded(context.Background(), e)
	r.Wait()
	if got := tg.texts(); len(got) != 1 {
		t.Fatalf("first deploy: messages %q", got)
	}

	// Nothing applied and no request manifest: no alerts are configured, so nothing is sent and nothing is logged.
	tg = newReportsTelegram(t)
	var logged []string
	r = NewReports(monitoringConfig(tg.srv.URL), openIngressStore(t), func(s string) { logged = append(logged, s) })
	r.OperationEnded(context.Background(), failedDeploy())
	r.Wait()
	if got := tg.texts(); len(got) != 0 || len(logged) != 0 {
		t.Fatalf("messages %q, logged %q", got, logged)
	}
}

func TestDeployReportsLogATelegramFailureWithoutTheToken(t *testing.T) {
	tg := newReportsTelegram(t)
	tg.status.Store(http.StatusUnauthorized)
	var mu sync.Mutex
	var logged []string
	r := NewReports(monitoringConfig(tg.srv.URL), alertStore(t, ""), func(s string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, s)
	})
	r.OperationEnded(context.Background(), failedDeploy())
	r.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "deploy alert") || !strings.Contains(logged[0], "telegram returned HTTP 401") {
		t.Fatalf("logged %q", logged)
	}
	if strings.Contains(logged[0], reportsBotToken) || strings.Contains(logged[0], "TEST-bot-token") || strings.Contains(logged[0], tg.srv.URL) {
		t.Fatalf("the log line leaks the token or the URL: %q", logged[0])
	}
}

func TestDeployReportsLogAMissingBotTokenSecret(t *testing.T) {
	tg := newReportsTelegram(t)
	s := openIngressStore(t)
	recordRelease(t, s, "alerts", monitoredManifest(`,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"-1001234567890"}}`))
	var logged []string
	r := NewReports(monitoringConfig(tg.srv.URL), s, func(s string) { logged = append(logged, s) })
	r.OperationEnded(context.Background(), failedDeploy())
	r.Wait()
	if len(tg.texts()) != 0 || len(logged) != 1 || !strings.Contains(logged[0], "deploy alert not sent") {
		t.Fatalf("messages %q, logged %q", tg.texts(), logged)
	}
}

// logsReceiver is an OTLP/HTTP logs endpoint that keeps the records it receives.
type logsReceiver struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []*logspb.LogRecord
	res map[string]string
}

func newLogsReceiver(t *testing.T) *logsReceiver {
	t.Helper()
	rc := &logsReceiver{res: map[string]string{}}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/logs" || r.Header.Get("Authorization") != "Bearer TEST-ONLY-LOGS" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(bytes.Clone(body), &req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		rc.mu.Lock()
		defer rc.mu.Unlock()
		for _, rl := range req.GetResourceLogs() {
			for _, kv := range rl.GetResource().GetAttributes() {
				rc.res[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			for _, sl := range rl.GetScopeLogs() {
				rc.got = append(rc.got, sl.GetLogRecords()...)
			}
		}
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *logsReceiver) records() []*logspb.LogRecord {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]*logspb.LogRecord(nil), rc.got...)
}

func recordAttrs(r *logspb.LogRecord) (map[string]string, map[string]int64) {
	str, num := map[string]string{}, map[string]int64{}
	for _, kv := range r.GetAttributes() {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_IntValue:
			num[kv.GetKey()] = v.IntValue
		case *commonpb.AnyValue_StringValue:
			str[kv.GetKey()] = v.StringValue
		}
	}
	return str, num
}

func telemetryAlertStore(t *testing.T, endpoint string) *state.Store {
	t.Helper()
	s := openIngressStore(t)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "OTLP_HEADERS"}, []byte("authorization=Bearer TEST-ONLY-LOGS")); err != nil {
		t.Fatal(err)
	}
	recordRelease(t, s, "telemetry", monitoredManifest(`,"telemetry":{"otlp_endpoint":"`+endpoint+`","headers_secret":"OTLP_HEADERS"}`))
	return s
}

func waitRecords(t *testing.T, rc *logsReceiver, n int) []*logspb.LogRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if got := rc.records(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d log records, want %d", len(rc.records()), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReportsSendOneLogRecordAtTheStartAndOneAtTheEndOfEveryOperation(t *testing.T) {
	rc := newLogsReceiver(t)
	r := NewReports(monitoringConfig(""), telemetryAlertStore(t, rc.srv.URL), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	start := failedDeploy()
	start.State, start.Ended, start.Reason, start.Err = "running", time.Time{}, "", ""
	end := succeededDeploy()
	end.Digest = "sha256:" + strings.Repeat("a", 64)
	restartStart := start
	restartStart.OperationID, restartStart.Kind, restartStart.Version = "op456", "restart", ""
	restartEnd := end
	restartEnd.OperationID, restartEnd.Kind, restartEnd.Version, restartEnd.Digest, restartEnd.State = "op456", "restart", "", "", "failed"
	r.OperationStarted(ctx, start)
	r.OperationEnded(ctx, end)
	r.OperationStarted(ctx, restartStart)
	r.OperationEnded(ctx, restartEnd)

	recs := waitRecords(t, rc, 4)
	if len(recs) != 4 {
		t.Fatalf("%d records", len(recs))
	}
	byBody := map[string]*logspb.LogRecord{}
	for _, rec := range recs {
		attrs, _ := recordAttrs(rec)
		if attrs["berthy.project"] != "demo" || attrs["berthy.environment"] != "lab" {
			t.Errorf("record identity %v", attrs)
		}
		byBody[rec.GetBody().GetStringValue()+"/"+attrStr(rec, "berthy.operation.id")] = rec
	}
	started := byBody["deploy started/op123"]
	ended := byBody["deploy succeeded/op123"]
	if started == nil || ended == nil || byBody["restart started/op456"] == nil || byBody["restart failed/op456"] == nil {
		t.Fatalf("records %v", byBody)
	}
	str, num := recordAttrs(started)
	for k, want := range map[string]string{"berthy.environment": "lab", "berthy.app": "api", "berthy.operation.id": "op123", "berthy.operation.kind": "deploy", "berthy.version": "v1.4.3", "berthy.operation.state": "running", "berthy.actor": "ci"} {
		if str[k] != want {
			t.Errorf("start %s = %q, want %q", k, str[k], want)
		}
	}
	if _, has := num["berthy.operation.duration_ms"]; has {
		t.Error("a start has a duration")
	}
	if started.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_INFO {
		t.Errorf("start severity %v", started.GetSeverityNumber())
	}
	str, num = recordAttrs(ended)
	if str["berthy.operation.state"] != "succeeded" || str["berthy.image.digest"] != "sha256:"+strings.Repeat("a", 64) || str["berthy.version"] != "v1.4.2" || num["berthy.operation.duration_ms"] != 48000 {
		t.Errorf("end attributes %v %v", str, num)
	}
	failedRestart := byBody["restart failed/op456"]
	if failedRestart.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_WARN {
		t.Errorf("failed severity %v", failedRestart.GetSeverityNumber())
	}
	if s, _ := recordAttrs(failedRestart); s["berthy.version"] != "" || s["berthy.image.digest"] != "" {
		t.Errorf("unknown version and digest are not left out: %v", s)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.res["deployment.environment.name"] != "lab" || rc.res["service.name"] != "berthy-agent" {
		t.Errorf("resource %v", rc.res)
	}
}

func attrStr(r *logspb.LogRecord, key string) string {
	s, _ := recordAttrs(r)
	return s[key]
}

func TestReportsSendNoTelemetryWithoutAConfiguration(t *testing.T) {
	rc := newLogsReceiver(t)
	r := NewReports(monitoringConfig(""), alertStore(t, ""), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	r.OperationStarted(ctx, failedDeploy())
	r.OperationEnded(ctx, succeededDeploy())
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if n := len(rc.records()); n != 0 {
		t.Fatalf("%d records", n)
	}
}

func TestReportsTellTelemetryOfAResumedEndButNotTelegram(t *testing.T) {
	rc := newLogsReceiver(t)
	tg := newReportsTelegram(t)
	s := telemetryAlertStore(t, rc.srv.URL)
	r := NewReports(monitoringConfig(tg.srv.URL), s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	e := failedDeploy()
	e.State, e.Resumed = "failed", true
	r.OperationEnded(ctx, e)
	r.Wait()
	waitRecords(t, rc, 1)
	if got := tg.texts(); len(got) != 0 {
		t.Fatalf("messages %q", got)
	}
}
