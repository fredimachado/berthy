package setup

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"berthy/internal/scope"
)

func TestWithoutHostedPreservesEveryOtherByte(t *testing.T) {
	const pre = "{\r\n \"listen\" : \"127.0.0.1:7710\", \"cluster\":{\"state_dir\":\"/var/lib/berthy/cluster/state\",\"key_file\":\"/var/lib/berthy/cluster/key\"}, \"environments\":[\n  "
	const post = "\n ], \"compose_command\" : [\"docker\",\"com\\u0070ose\"]}\n"
	first := removalHostedJSON("alpha", "dev", "/custom/one", "1")
	middle := strings.ReplaceAll(removalHostedJSON("beta", "dev", "/custom/two", "2"), ",", " ,\n   ")
	last := strings.Replace(removalHostedJSON("gamma", "prod", "/custom/three", "3"), "gamma", `gam\u006da`, 1)
	raw := pre + first + " ,\r\n\t" + middle + ",\n   " + last + post
	for _, tt := range []struct {
		id         scope.ID
		dir, peers string
	}{
		{scope.ID{Project: "alpha", Environment: "dev"}, "/custom/one", middle + ",\n   " + last},
		{scope.ID{Project: "beta", Environment: "dev"}, "/custom/two", first + " ,\r\n\t" + last},
		{scope.ID{Project: "gamma", Environment: "prod"}, "/custom/three", first + " ,\r\n\t" + middle},
	} {
		t.Run(tt.id.String(), func(t *testing.T) {
			input := []byte(raw)
			got, target, err := withoutHosted(input, tt.id)
			if err != nil || string(got) != pre+tt.peers+post || string(input) != raw {
				t.Fatalf("replacement = %q, error = %v, mutated input = %t", got, err, string(input) != raw)
			}
			if target.Project != tt.id.Project || target.Environment != tt.id.Environment ||
				target.StateDir != tt.dir+"/state" || target.KeyFile != tt.dir+"/key" {
				t.Fatalf("wrong target: %+v", target)
			}
		})
	}
}

func TestWithoutHostedRefusesUnknownLastAndAmbiguousInput(t *testing.T) {
	const pre = `{"listen":"127.0.0.1:7710","cluster":{"state_dir":"/var/lib/berthy/cluster/state","key_file":"/var/lib/berthy/cluster/key"},"environments":[`
	first := removalHostedJSON("alpha", "dev", "/one", "1")
	middle := removalHostedJSON("beta", "dev", "/two", "2")
	raw := pre + first + "," + middle + "]}"
	for _, tt := range []struct{ name, input, project string }{
		{"unknown", raw, "missing"},
		{"last", pre + first + "]}", "alpha"},
		{"duplicate array", strings.TrimSuffix(raw, "}") + `,"environments":[]}`, "alpha"},
		{"case-variant array", strings.Replace(raw, "environments", "Environments", 1), "alpha"},
		{"duplicate nested key", strings.Replace(raw, `"scope":"admin"`, `"scope":"admin","scope":"admin"`, 1), "alpha"},
		{"case-variant field", strings.Replace(raw, "state_dir", "State_Dir", 1), "alpha"},
		{"malformed", strings.TrimSuffix(raw, "]}"), "alpha"},
		{"old layout", `{"environment":"dev","project":"alpha"}`, "alpha"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := withoutHosted([]byte(tt.input), scope.ID{Project: tt.project, Environment: "dev"})
			if got != nil || err == nil {
				t.Fatalf("got %q, %v", got, err)
			}
			if (tt.name == "unknown" || tt.name == "last") && !strings.Contains(err.Error(), tt.project+"/dev") {
				t.Fatalf("refusal does not name the pair: %v", err)
			}
		})
	}
}

func removalHostedJSON(project, env, dir, digestDigit string) string {
	return `{"project":"` + project + `","environment":"` + env + `","state_dir":"` + dir + `/state","key_file":"` + dir +
		`/key","tokens":[{"digest":"` + strings.Repeat(digestDigit, 64) + `","scope":"admin"}]}`
}

func TestAppliedAppsProbeIsBoundedAndFailsClosed(t *testing.T) {
	id := scope.ID{Project: "alpha", Environment: "dev"}
	for _, tt := range []struct {
		name, reply string
		code        int
		want        string
	}{
		{"empty", "empty\n", 0, ""},
		{"applied", "applied\n", 0, "berthy destroy --project alpha -e dev --confirm alpha/dev --allow-recreate"},
		{"no reply", "", 0, "could not verify applied apps"},
		{"missing newline", "empty", 0, "could not verify applied apps"},
		{"extra bytes", "payload\nempty\n", 0, "could not verify applied apps"},
		{"unexpected reply", "payload\n", 0, "could not verify applied apps"},
		{"failed with a valid reply", "empty\n", 1, "could not verify applied apps"},
		{"older installed binary", "payload\n", 2, "upgrade the server with berthy setup"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			r := funcRunner{run: func(script string, stdin io.Reader) Result {
				calls++
				want := "exec setpriv --reuid=berthy --regid=berthy --clear-groups /usr/local/bin/berthy agent has-applied-apps 'alpha' 'dev' " + shellQuote("/custom/it's here")
				if stdin != nil || !strings.Contains(script, want) {
					t.Fatalf("unexpected probe command: %q", script)
				}
				return Result{Code: tt.code, Stdout: []byte(tt.reply), Stderr: bytes.Repeat([]byte("payload"), 2)}
			}}
			err := requireNoAppliedApps(context.Background(), r, id, "/custom/it's here")
			if calls != 1 || (err == nil) != (tt.want == "") ||
				(err != nil && (!strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "payload"))) {
				t.Fatalf("probe made %d calls and returned %v", calls, err)
			}
		})
	}
}
