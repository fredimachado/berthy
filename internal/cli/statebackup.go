package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/state"
	"berthy/internal/statebackup"
)

// runStateRestore runs "state-restore": it rebuilds an agent's state from a snapshot in object storage, with the saved key. It
// needs no agent, so it works when the server is gone. The credentials come from BERTHY_S3_ACCESS_KEY and BERTHY_S3_SECRET_KEY
// and never from a flag, so they stay out of shell history and the process list.
func runStateRestore(args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("state-restore", flag.ContinueOnError)
	fs.SetOutput(errout)
	endpoint := fs.String("endpoint", "", "S3-compatible endpoint URL")
	region := fs.String("region", "", "bucket region, when the storage needs one")
	bucket := fs.String("bucket", "", "bucket name")
	object := fs.String("object", "", "key of the snapshot, which ends in .db")
	dir := fs.String("out", "", "directory to write the state into: created when missing, otherwise empty")
	keyFile := fs.String("key-file", "", "the saved encryption key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *endpoint == "" || *bucket == "" || *object == "" || *dir == "" || *keyFile == "" {
		fmt.Fprintln(errout, "usage: berthy state-restore --endpoint URL --bucket NAME [--region R] --object KEY --out DIR --key-file PATH")
		return 2
	}
	access, secret := os.Getenv("BERTHY_S3_ACCESS_KEY"), os.Getenv("BERTHY_S3_SECRET_KEY")
	if access == "" || secret == "" {
		fmt.Fprintln(errout, "set BERTHY_S3_ACCESS_KEY and BERTHY_S3_SECRET_KEY")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep, err := statebackup.Restore(ctx, statebackup.RestoreOptions{Endpoint: *endpoint, Region: *region, Bucket: *bucket, Object: *object,
		Out: *dir, KeyFile: *keyFile, AccessKey: access, SecretKey: secret})
	var mismatch *statebackup.KeyMismatchError
	switch {
	case errors.As(err, &mismatch):
		// It names the key the snapshot needs, so the person knows which saved key to look for.
		fmt.Fprintln(errout, terminalSafe(mismatch.Error()))
		return 1
	case errors.Is(err, state.ErrWrongKey):
		fmt.Fprintln(errout, state.ErrWrongKey)
		return 1
	case err != nil:
		fmt.Fprintln(errout, err)
		return 1
	}
	fmt.Fprintf(out, "restored: %s\nenvironment: %s\nschema: %d\nsize: %s\nsha256: %s\nsecrets: %d\n",
		terminalSafe(rep.Path), terminalSafe(rep.Environment), rep.SchemaVersion, api.HumanSize(rep.Size), rep.SHA256, len(rep.Secrets))
	for _, name := range rep.Secrets {
		fmt.Fprintf(out, "  %s\n", terminalSafe(name))
	}
	return 0
}

// runStateBackupCommand runs "state-backup": it asks the agent for a state snapshot now, waits for the run to finish, and prints
// it. It exits 0 only when the snapshot succeeded.
func runStateBackupCommand(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	if len(f.pos) != 0 {
		fmt.Fprintln(errout, "state-backup takes no arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	call := agentclient.Caller(ctx, agentclient.NewHTTPClient(), base, prefix, token)
	data, status, err := call("POST", "state-backup", "")
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	if status != http.StatusAccepted {
		fmt.Fprintln(errout, agentclient.Message(status, data))
		return 1
	}
	if code, held := reportHeld(data, out, errout, f.json); held {
		return code
	}
	var started struct {
		RunID int64 `json:"state_backup_run_id"`
	}
	if json.Unmarshal(data, &started) != nil || started.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	deadline := time.Now().Add(backupWaitLimit)
	failures := 0
	for {
		data, status, err := call("GET", "state-backup/runs", "limit=100")
		var runs []api.StateBackupRun
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &runs) != nil {
			if failures++; failures >= 3 {
				fmt.Fprintln(errout, "run status unavailable")
				return 1
			}
		} else {
			failures = 0
			for _, run := range runs {
				if run.ID == started.RunID && run.Status != "running" {
					return printStateBackupRun(out, errout, run, f.json)
				}
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(errout, "run %d did not finish in time\n", started.RunID)
			return 1
		}
		select {
		case <-time.After(backupPollInterval):
		case <-ctx.Done():
			fmt.Fprintf(errout, "stopped waiting; run %d continues on the agent\n", started.RunID)
			return 1
		}
	}
}

// printStateBackupRun prints a finished state snapshot and returns 0 when it succeeded.
func printStateBackupRun(out, errout io.Writer, run api.StateBackupRun, asJSON bool) int {
	if asJSON {
		pretty, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, string(pretty))
	} else {
		fmt.Fprintf(out, "run: %d\nstatus: %s\n", run.ID, terminalSafe(run.Status))
		if run.Status == "succeeded" {
			fmt.Fprintf(out, "key: %s\nsize: %d\nsha256: %s\n", terminalSafe(run.Key), run.Size, terminalSafe(run.SHA256))
		}
		if run.Detail != "" {
			fmt.Fprintf(out, "detail: %s\n", terminalSafe(run.Detail))
		}
	}
	if run.Status == "succeeded" {
		return 0
	}
	return 1
}
