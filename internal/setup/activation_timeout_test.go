package setup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type slowStartRunner struct {
	*agentFake
	startupFixture
	t *testing.T
}

func (r *slowStartRunner) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	res, err := r.agentFake.Run(ctx, script, stdin)
	if strings.Contains(script, "systemctl restart berthy") {
		code, out, _, _ := r.run(r.t, script)
		res.Code, res.Stdout = code, []byte(out)
	}
	return res, err
}

func TestServiceKeepsAgentThatAnswersAfter45Seconds(t *testing.T) {
	record := startupRecord(newInvocation, "shop/p5\tstarted\t\n", "1")
	fixture := newStartupFixture(t, record, "", "0AFA1FAC:1E1E") // listenIP:7710 in /proc/net/tcp
	shellTool(t, fixture.tools, "berthy.setup-probe", `
n=$(date +%s)
[ "$n" -ge 45 ] || exit 1
echo "agent answered at second $n" >> "$CALLS"`)
	f := freshAgentServer(t)
	r := &slowStartRunner{agentFake: f, startupFixture: fixture, t: t}
	s := agentSession(t, r, elfFile(t, emX86), false, nil)
	steps, ok := runSteps(s)
	service := stepOf(t, steps, "service")
	word, _ := os.ReadFile(fixture.state)
	clock, _ := os.ReadFile(filepath.Join(fixture.dir, "clock"))
	calls, _ := os.ReadFile(fixture.calls)
	recovered := slices.ContainsFunc(f.scripts(), func(script string) bool {
		return strings.HasPrefix(script, "/bin/sh "+guardPath+" ") || script == "systemctl stop berthy"
	}) || strings.Contains(string(calls), "stop berthy\n")
	if !ok || service.Status != StatusChanged || string(word) != "confirmed\n" || string(clock) != "45\n" ||
		!strings.Contains(string(calls), "agent answered at second 45") || recovered {
		t.Fatalf("slow start: service=%+v, clock=%q, guard=%q, recovery=%t", service, clock, word, recovered)
	}
}

func TestServiceUsesDesiredHostedCount(t *testing.T) {
	for _, tc := range []struct {
		name, mode  string
		override    time.Duration
		wait, timer int
	}{
		{name: "first install", mode: "fresh", wait: 90, timer: 240},
		{name: "rerun existing environment", mode: "unchanged", wait: 150},
		{name: "add second environment", mode: "add", wait: 150, timer: 360},
		{name: "permissions only", mode: "permissions", wait: 150},
		{name: "positive override", mode: "add", override: 7 * time.Second, wait: 7, timer: 74},
	} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dry=%t", tc.name, dry), func(t *testing.T) {
				f := freshAgentServer(t)
				if tc.mode != "fresh" {
					other := s15File(t, "shop", "prod", "shop-prod")
					other.Environments[0].Tokens[0].Digest = strings.Repeat("b", 64)
					file := other
					if tc.mode != "add" {
						file = s15File(t, "shop", "p5", "p5net")
						file.Environments = append(file.Environments, other.Environments[0])
					}
					f = s15Server(t, file)
				}
				if tc.mode == "permissions" {
					f.probe = with(without(f.probe, "agent_config_stat="), "agent_config_stat=root:root 644")
				}
				s := agentSession(t, f, elfFile(t, emX86), dry, func(o *Options) { o.HealthTimeout = tc.override })
				if dry {
					s.Log = NewCommandLog(io.Discard)
				}
				var results []StepResult
				selected := []Step{{"token", tokenStep}, {"config", configStep}, {"service", serviceStep}}
				if !Run(t.Context(), s, selected, func(r StepResult) { results = append(results, r) }) {
					t.Fatalf("setup: %+v", results)
				}
				config := stepOf(t, results, "config")
				if tc.mode == "unchanged" && config.Status != StatusOK ||
					tc.mode == "permissions" && !strings.Contains(config.Detail, "owner and mode") {
					t.Fatalf("config did not take the expected path: %+v", config)
				}
				scripts := f.scripts()
				if dry {
					scripts = *stepOf(t, results, "service").Commands
					if f.ran("systemctl restart berthy") || f.ran("systemd-run") {
						t.Fatal("dry run sent activation scripts to the server")
					}
				}
				var wait, timer string
				for _, script := range scripts {
					for _, line := range strings.Split(script, "\n") {
						if strings.HasPrefix(line, "end=$((") {
							wait = line
						}
						if strings.HasPrefix(line, "systemd-run ") {
							timer = line
						}
					}
				}
				wantWait := fmt.Sprintf("end=$(( $(date +%%s) + %d ))", tc.wait)
				wantTimer := fmt.Sprintf("--on-active=%ds ", tc.timer)
				if wait != wantWait || tc.timer == 0 && timer != "" || tc.timer != 0 && !strings.Contains(timer, wantTimer) {
					t.Fatalf("activation: wait=%q, timer=%q, want %ds start and %ds guard", wait, timer, tc.wait, tc.timer)
				}
			})
		}
	}
}
