package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/state"
)

func readComposeDiagnostic(t *testing.T, s *state.Store, id string) Diagnostic {
	t.Helper()
	items, err := s.Diagnostics(context.Background(), id)
	if err != nil || len(items) != 1 {
		t.Fatalf("stored %d diagnostics, want 1: %v", len(items), err)
	}
	var d Diagnostic
	if err := json.Unmarshal(items[0], &d); err != nil {
		t.Fatal(err)
	}
	if d.Kind != "compose" || d.Name != "up" || d.Captured.IsZero() || d.Captured.Location().String() != "UTC" || d.Container != "" || d.TaskID != "" || d.ExitCode != nil {
		t.Fatalf("unexpected Compose diagnostic fields: kind=%q name=%q captured=%v container=%q task=%q exit=%v", d.Kind, d.Name, d.Captured, d.Container, d.TaskID, d.ExitCode)
	}
	return d
}

func TestComposeUpDiagnosticKeepsFinalErrorWithLargeHistoricalTemplate(t *testing.T) {
	const final = "Docker could not start the executable\n"
	runner := &fakeCompose{config: configOne, hashes: hashOne, upErr: errors.New("exit status 1"),
		upStderr: strings.Repeat("!", engine.MaxComposeOutput+1-len(final)) + final}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "HISTORICAL"}, []byte(strings.Repeat("z", api.MaxSecretValue))); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env.Template = map[string]string{"LARGE": strings.Repeat("${HISTORICAL}", 17)}
	m.Apps["db"] = app
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	seed, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "admin", Kind: "deploy", App: "db", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, seed.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Digest: digestOf(composeFileOne), Manifest: b}}); err != nil {
		t.Fatal(err)
	}
	values, err := c.redactionValues(ctx)
	if err != nil || len(values) == 0 || len(values[0]) <= engine.MaxComposeOutput {
		t.Fatalf("fixture did not resolve a historical value larger than the stderr window: %v", err)
	}
	op := admitAndDrain(t, c, composeReq("historical-template-failure", composeFileOne, composeApp(false)))
	d := readComposeDiagnostic(t, s, op.ID)
	if !d.Truncated || !strings.HasSuffix(d.Logs, final) {
		t.Errorf("lost final Docker error: %d bytes and truncated=%t", len(d.Logs), d.Truncated)
	}
}

func TestComposeDiagnosticTailLimits(t *testing.T) {
	const rawLimit = engine.MaxComposeOutput
	const last = "last error\n"
	const tokenHex = "0123456789abcdef"
	const cutToken = "test." + tokenHex + tokenHex + tokenHex + tokenHex
	const marker = "[redacted]"
	lines := strings.Repeat("line\n", 200)
	expanded := strings.Repeat(marker+" ", 6000)
	fillWindow := func(prefix string) string {
		return prefix + strings.Repeat("!", rawLimit+1-len(prefix)-len(last)) + last
	}
	for _, tc := range []struct {
		name, raw, want string
		secrets         []string
		truncated       bool
	}{
		{"200 lines", lines, lines, nil, false},
		{"201 lines", "old\n" + lines, lines, nil, true},
		{"unterminated line", lines + "last", lines[len("line\n"):] + "last", nil, true},
		{"UTF-8 byte cutoff", "界" + strings.Repeat("!", diagnosticLogBytes-1), strings.Repeat("!", diagnosticLogBytes-1), nil, true},
		{"UTF-8 repair", "bad\xffbytes", "badbytes", nil, false},
		{"overlapping literal occurrences", "one ababa two", "one " + marker + " two", []string{"aba"}, false},
		{"expansion after redaction", strings.Repeat("x ", 6000), expanded[len(expanded)-diagnosticLogBytes:], []string{"x"}, true},
		{"literal suffix at raw edge", fillWindow("ret "), marker + " " + marker + last, []string{"secret", "!!"}, true},
		{"token prefix cut at raw edge", fillWindow(cutToken + " "), marker + " " + marker + last, []string{"!!"}, true},
		{"token payload cut at raw edge", fillWindow(tokenHex + " "), marker + " " + marker + last, []string{"!!"}, true},
		{"unrelated raw edge", fillWindow("wholly unrelated "), "holly unrelated " + marker + last, []string{"secret", "!!"}, true},
		{"known left edge", "ret end", "ret end", []string{"secret"}, false},
		{"literal across byte cutoff", "z secret" + strings.Repeat("!", diagnosticLogBytes-2), "d]" + strings.Repeat("!", diagnosticLogBytes-2), []string{"secret"}, true},
		{"raw limit exact", fillWindow("")[1:], marker + last, []string{"!!"}, false},
		{"raw limit exceeded", fillWindow(""), marker + last, []string{"!!"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeCompose{config: configOne, hashes: hashOne, upErr: errors.New("exit status 1"), upStderr: tc.raw}
			c, s := composeController(t, runner, newComposeDocker())
			for i, value := range tc.secrets {
				if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: fmt.Sprintf("VALUE_%d", i)}, []byte(value)); err != nil {
					t.Fatal(err)
				}
			}
			op := admitAndDrain(t, c, composeReq("tail", composeFileOne, composeApp(false)))
			d := readComposeDiagnostic(t, s, op.ID)
			if d.Error != "" || d.Logs != tc.want || d.Truncated != tc.truncated {
				t.Errorf("logs start %q (%d bytes), error %q, truncated=%t; want %q (%d bytes), truncated=%t",
					d.Logs[:min(40, len(d.Logs))], len(d.Logs), d.Error, d.Truncated, tc.want[:min(40, len(tc.want))], len(tc.want), tc.truncated)
			}
		})
	}
}

func TestComposeUpDiagnosticFailsClosed(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, upErr: errors.New("exit status 1"), upStderr: "raw fixture marker"}
	c, s := composeController(t, runner, newComposeDocker())
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "PASSWORD"}, []byte("fixture-value")); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env.Template = map[string]string{"URL": "example:${PASSWORD}"}
	m.Apps["db"] = app
	var removeErr error
	runner.onUp = func(ctx context.Context) {
		op, err := s.Active(ctx)
		if err == nil {
			err = s.DeleteOperationSecret(ctx, op.ID, "PASSWORD")
		}
		removeErr = err
	}
	op := admitAndDrain(t, c, composeReq("redaction-unavailable", composeFileOne, m))
	if runner.upRuns() != 1 || removeErr != nil {
		t.Fatalf("fixture did not remove the template secret during up: %v", removeErr)
	}
	if op.Err != "compose up failed" {
		t.Fatalf("operation error %q", op.Err)
	}
	d := readComposeDiagnostic(t, s, op.ID)
	if d.Error != "logs unavailable" || d.Logs != "" {
		t.Errorf("diagnostic error %q, logs %q", d.Error, d.Logs)
	}
}
