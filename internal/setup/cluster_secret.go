package setup

import (
	"bytes"
	"context"
	"errors"

	"berthy/internal/api"
)

// PreviewClusterSecret checks the server and records the maintenance command without sending it.
func (s *Session) PreviewClusterSecret(ctx context.Context, name string) error {
	script, err := clusterSecretCommand(ctx, s.R, name)
	if err == nil {
		s.Log.Would(script)
	}
	return err
}

// SetClusterSecret sends one credential to the installed writer and verifies that the service starts again.
func SetClusterSecret(ctx context.Context, r Runner, name string, value []byte) error {
	if len(value) == 0 || len(value) > api.MaxSecretValue || bytes.ContainsAny(value, "\r\n\x00") {
		return errors.New("the cluster credential must be a nonempty single line")
	}
	script, err := clusterSecretCommand(ctx, r, name)
	if err != nil {
		return err
	}
	res, err := r.Run(ctx, script, bytes.NewReader(value))
	if err != nil {
		return errors.New("could not confirm the cluster credential write or service startup")
	}
	// The exit status is the only output used from the command that reads the credential.
	switch res.Code {
	case 0:
		return nil
	case 21:
		return errors.New("could not stop berthy.service; the credential was not stored")
	case 22:
		return errors.New("the cluster credential was not stored; berthy.service is running")
	case 23:
		return errors.New("the cluster credential was stored, but berthy.service did not come up")
	case 24:
		return errors.New("the cluster credential was not stored and berthy.service did not come up")
	case 25:
		return errors.New("berthy.service stopped before the cluster credential could be written")
	default:
		return errors.New("could not confirm the cluster credential write or service startup")
	}
}

func clusterSecretCommand(ctx context.Context, r Runner, name string) (string, error) {
	if err := api.CheckSecretName(name); err != nil {
		return "", err
	}
	if _, err := Connect(ctx, r); err != nil {
		return "", err
	}
	const ready = "test -x " + binPath + " && systemctl is-active --quiet berthy </dev/null"
	if res, err := r.Run(ctx, ready, nil); err != nil || res.Code != 0 {
		return "", errors.New("cluster credentials require an installed and running berthy.service")
	}
	res, err := r.Run(ctx, "cat "+configPath, nil)
	if err != nil || res.Code != 0 {
		return "", errors.New("could not read the installed agent configuration")
	}
	f, err := decodeAgentConfig(res.Stdout)
	if err != nil {
		return "", err
	}
	wait := wholeSeconds((Options{}).healthTimeout(len(f.Environments)))
	return clusterSecretScript(ready, f.Listen, name, wait), nil
}

func clusterSecretScript(ready, listen, name string, wait int) string {
	return "# store cluster credential\n" + ready + " || exit 25\n" + `
status=21
restore_service() {
  trap - EXIT
  trap '' HUP INT TERM
  if systemctl start berthy >/dev/null 2>&1 && (
` + waitScriptOnSuccess(binPath, listen, wait, "exit 0") + `
  ); then exit "$status"; fi
  if [ "$status" -eq 0 ]; then exit 23; fi
  exit 24
}
# The remote shell owns the service recovery even if the operator disconnects.
trap 'restore_service </dev/null' EXIT HUP INT TERM
if systemctl stop berthy </dev/null >/dev/null 2>&1; then
  status=22
  if ` + probeAsAgent + binPath + " agent setup-cluster-secret " + shellQuote(name) + ` >/dev/null 2>&1; then
    status=0
  fi
fi
restore_service </dev/null
`
}
