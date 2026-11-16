package manifest

import (
	"strings"
	"testing"
	"time"
)

// attemptManifest returns a valid JSON manifest with one image app whose deploy, hooks and migrations blocks are the
// given JSON objects ("" leaves a block out).
func attemptManifest(deploy, hooks, migrations string) string {
	extra := ""
	if deploy != "" {
		extra += `,"deploy":` + deploy
	}
	if hooks != "" {
		extra += `,"hooks":` + hooks
	}
	if migrations != "" {
		extra += `,"migrations":` + migrations
	}
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}` + extra + `}}}`
}

func TestStartAttemptsAndCommandSettingsDefaults(t *testing.T) {
	m, err := DecodeJSON([]byte(attemptManifest("", `{"pre_deploy":[{"run":["a"]},{"run":["b"],"timeout":"90s","retries":2}],"post_deploy":[{"run":["c"]}]}`, `{"version":["v"],"apply":["x"]}`)))
	if err != nil {
		t.Fatal(err)
	}
	a := m.Apps["api"]
	if a.Deploy.StartAttempts == nil || *a.Deploy.StartAttempts != 3 {
		t.Fatalf("start_attempts default %v, want 3", a.Deploy.StartAttempts)
	}
	pre := a.Hooks.PreDeploy
	if pre[0].Timeout != "5m" || pre[0].Retries == nil || *pre[0].Retries != 0 {
		t.Fatalf("hook defaults not written: %+v", pre[0])
	}
	if pre[1].Timeout != "90s" || *pre[1].Retries != 2 {
		t.Fatalf("explicit hook settings lost: %+v", pre[1])
	}
	if post := a.Hooks.PostDeploy[0]; post.Timeout != "5m" || *post.Retries != 0 {
		t.Fatalf("post hook defaults not written: %+v", post)
	}
	if x := a.Migrations; x.Timeout != "5m" || x.Retries == nil || *x.Retries != 0 {
		t.Fatalf("migration defaults not written: %+v", x)
	}
	if pre[0].TimeoutDuration() != 5*time.Minute || pre[1].TimeoutDuration() != 90*time.Second || pre[1].RetryCount() != 2 {
		t.Fatalf("hook accessors: %v %v %d", pre[0].TimeoutDuration(), pre[1].TimeoutDuration(), pre[1].RetryCount())
	}
	if a.Migrations.TimeoutDuration() != 5*time.Minute || a.Migrations.RetryCount() != 0 || a.Deploy.StartAttemptLimit() != 3 {
		t.Fatal("accessors do not return the defaults")
	}
}

// A stored release written before these settings existed has none of them; the accessors still answer with the defaults.
func TestSettingsAccessorsFallBackToDefaults(t *testing.T) {
	if (Deploy{}).StartAttemptLimit() != 3 || (Hook{}).TimeoutDuration() != 5*time.Minute || (Hook{}).RetryCount() != 0 || (Migrations{}).TimeoutDuration() != 5*time.Minute || (Migrations{}).RetryCount() != 0 {
		t.Fatal("zero settings do not fall back to the defaults")
	}
}

func TestStartAttemptsAndCommandSettingsValidation(t *testing.T) {
	valid := map[string]string{
		"start_attempts 1":    attemptManifest(`{"start_attempts":1}`, "", ""),
		"start_attempts 10":   attemptManifest(`{"start_attempts":10}`, "", ""),
		"hook timeout 1s":     attemptManifest("", `{"pre_deploy":[{"run":["a"],"timeout":"1s"}]}`, ""),
		"hook timeout 24h":    attemptManifest("", `{"post_deploy":[{"run":["a"],"timeout":"24h"}]}`, ""),
		"hook retries 5":      attemptManifest("", `{"pre_deploy":[{"run":["a"],"retries":5}]}`, ""),
		"migration timeout":   attemptManifest("", "", `{"version":["v"],"apply":["x"],"timeout":"30m"}`),
		"migration retries 5": attemptManifest("", "", `{"version":["v"],"apply":["x"],"retries":5}`),
	}
	for name, input := range valid {
		if _, err := DecodeJSON([]byte(input)); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	invalid := map[string]struct{ input, want string }{
		"start_attempts 0":          {attemptManifest(`{"start_attempts":0}`, "", ""), "app api: invalid start_attempts"},
		"start_attempts 11":         {attemptManifest(`{"start_attempts":11}`, "", ""), "app api: invalid start_attempts"},
		"start_attempts negative":   {attemptManifest(`{"start_attempts":-1}`, "", ""), "app api: invalid start_attempts"},
		"hook timeout 0s":           {attemptManifest("", `{"pre_deploy":[{"run":["a"],"timeout":"0s"}]}`, ""), "app api: pre_deploy hook 1: invalid timeout"},
		"hook timeout 25h":          {attemptManifest("", `{"post_deploy":[{"run":["a"],"timeout":"25h"}]}`, ""), "app api: post_deploy hook 1: invalid timeout"},
		"hook timeout below 1s":     {attemptManifest("", `{"pre_deploy":[{"run":["a"],"timeout":"500ms"}]}`, ""), "app api: pre_deploy hook 1: invalid timeout"},
		"hook timeout not duration": {attemptManifest("", `{"pre_deploy":[{"run":["a"],"timeout":"soon"}]}`, ""), "app api: pre_deploy hook 1: invalid timeout"},
		"hook retries 6":            {attemptManifest("", `{"pre_deploy":[{"run":["a"],"retries":6}]}`, ""), "app api: pre_deploy hook 1: invalid retries"},
		"hook retries negative":     {attemptManifest("", `{"post_deploy":[{"run":["a"],"retries":-1}]}`, ""), "app api: post_deploy hook 1: invalid retries"},
		"backup hook timeout":       {attemptManifest("", `{"pre_deploy":[{"backup":"db","timeout":"1m"}]}`, ""), "app api: pre_deploy hook 1: timeout and retries apply to run hooks only"},
		"backup hook retries":       {attemptManifest("", `{"post_deploy":[{"backup":"db","retries":1}]}`, ""), "app api: post_deploy hook 1: timeout and retries apply to run hooks only"},
		"migration timeout 0s":      {attemptManifest("", "", `{"version":["v"],"apply":["x"],"timeout":"0s"}`), "app api: invalid migrations timeout"},
		"migration timeout 25h":     {attemptManifest("", "", `{"version":["v"],"apply":["x"],"timeout":"25h"}`), "app api: invalid migrations timeout"},
		"migration retries 6":       {attemptManifest("", "", `{"version":["v"],"apply":["x"],"retries":6}`), "app api: invalid migrations retries"},
		"migration retries -1":      {attemptManifest("", "", `{"version":["v"],"apply":["x"],"retries":-1}`), "app api: invalid migrations retries"},
	}
	for name, tc := range invalid {
		_, err := DecodeJSON([]byte(tc.input))
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}
}
