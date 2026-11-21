package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
)

// logLimitRunner answers every Compose call with one `compose config` output, or with err.
type logLimitRunner struct {
	config string
	err    error
}

func (r logLimitRunner) Run(context.Context, engine.ComposeRun) ([]byte, []byte, error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	return []byte(r.config), nil, nil
}

// withDaemonConfig makes the doctor read content as the daemon configuration. An empty content leaves the file missing.
// The doctor's daemonConfigPath is restored when the test ends.
func withDaemonConfig(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	saved := daemonConfigPath
	daemonConfigPath = path
	t.Cleanup(func() { daemonConfigPath = saved })
}

// stackConfig is `compose config --format json` of the Compose app "stack": one service that sets no logging and one
// that caps its logs.
const stackConfig = `{"services":{"bare":{"image":"alpine:3.20"},"capped":{"image":"alpine:3.20","logging":{"driver":"json-file","options":{"max-size":"5m"}}}}}`

// stackManifest declares one Compose app, "stack", which runs as the project shop-lab-stack.
func stackManifest() manifest.Manifest {
	return manifest.Manifest{Project: "shop", Network: "overlay", Apps: map[string]manifest.App{"stack": {Kind: "compose", File: "compose.yml"}}}
}

func TestDaemonLogMaxSizeReadsLogOpts(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"missing file", "", ""},
		{"max-size set", `{"log-opts":{"max-size":"20m"}}`, "20m"},
		{"other log-opts only", `{"log-opts":{"max-file":"3"}}`, ""},
		{"no log-opts", `{"storage-driver":"overlay2"}`, ""},
		{"not JSON", `{"log-opts":`, ""},
		{"max-size not a string", `{"log-opts":{"max-size":20}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDaemonConfig(t, tc.content)
			if got := daemonLogMaxSize(); got != tc.want {
				t.Fatalf("max-size %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComposeLogCheckNamesOnlyServicesWithoutALimit(t *testing.T) {
	withDaemonConfig(t, "")
	e, _ := doctorDocker(t, nil)
	d := &deploy.Controller{Environment: "lab", Compose: logLimitRunner{config: stackConfig}, StateDir: t.TempDir()}
	check, ok := composeLogCheck(context.Background(), d, e, stackManifest())
	if !ok || check.Name != "compose_log_limit" || check.OK || check.Critical {
		t.Fatalf("check %+v, reported %v", check, ok)
	}
	if !strings.Contains(check.Detail, "shop-lab-stack/bare") || strings.Contains(check.Detail, "capped") {
		t.Fatalf("detail %q", check.Detail)
	}
}

func TestComposeLogCheckIsSilentWhenNothingIsUnlimited(t *testing.T) {
	e, _ := doctorDocker(t, nil)
	t.Run("the daemon sets a max-size", func(t *testing.T) {
		withDaemonConfig(t, `{"log-opts":{"max-size":"20m"}}`)
		d := &deploy.Controller{Environment: "lab", Compose: logLimitRunner{config: stackConfig}, StateDir: t.TempDir()}
		if check, ok := composeLogCheck(context.Background(), d, e, stackManifest()); ok {
			t.Fatalf("warned although the daemon limits every service: %+v", check)
		}
	})
	t.Run("no Compose app", func(t *testing.T) {
		withDaemonConfig(t, "")
		d := &deploy.Controller{Environment: "lab", Compose: logLimitRunner{config: stackConfig}, StateDir: t.TempDir()}
		image := manifest.Manifest{Project: "shop", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web"}}}
		if check, ok := composeLogCheck(context.Background(), d, e, image); ok {
			t.Fatalf("warned without a Compose app: %+v", check)
		}
	})
	t.Run("the configuration cannot be read", func(t *testing.T) {
		withDaemonConfig(t, "")
		d := &deploy.Controller{Compose: logLimitRunner{err: os.ErrNotExist}, StateDir: t.TempDir()}
		if check, ok := composeLogCheck(context.Background(), d, e, stackManifest()); ok {
			t.Fatalf("reported a project it could not read: %+v", check)
		}
	})
}
