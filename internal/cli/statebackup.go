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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/scope"
	"berthy/internal/state"
	"berthy/internal/statebackup"
)

// runStateRestore runs "state-restore": it rebuilds an agent's state from a snapshot in object storage, with the saved key. It
// needs no agent, so it works when the server is gone. The credentials come from BERTHY_S3_ACCESS_KEY and BERTHY_S3_SECRET_KEY
// and never from a flag, so they stay out of shell history and the process list.
func runStateRestore(args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("state-restore", flag.ContinueOnError)
	fs.SetOutput(errout)
	projectFlag := fs.String("project", "", "project the snapshot belongs to")
	manifestDir := fs.String("manifest-dir", manifest.DefaultDir(), "directory of the manifest, used when --project is omitted")
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
		fmt.Fprintln(errout, "usage: berthy state-restore [--project PROJECT] [--manifest-dir DIR] --endpoint URL --bucket NAME [--region R] --object KEY --out DIR --key-file PATH")
		return 2
	}
	if *projectFlag != "" && !scope.ValidProject(*projectFlag) {
		fmt.Fprintln(errout, "valid --project required")
		return 2
	}
	access, secret := os.Getenv("BERTHY_S3_ACCESS_KEY"), os.Getenv("BERTHY_S3_SECRET_KEY")
	if access == "" || secret == "" {
		fmt.Fprintln(errout, "set BERTHY_S3_ACCESS_KEY and BERTHY_S3_SECRET_KEY")
		return 2
	}
	parts := strings.Split(*object, "/")
	if len(parts) < 3 || !scope.ValidEnvironment(parts[1]) {
		fmt.Fprintln(errout, "object key must start with PROJECT/ENVIRONMENT/")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	store, err := objectstore.New(objectstore.Config{Endpoint: *endpoint, Region: *region, Bucket: *bucket, AccessKey: access, SecretKey: secret})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	progress := startWait(errout, true, "finding project")
	project, err := resolveStateRestoreProject(ctx, *projectFlag, *manifestDir, parts[1], store)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, err)
		var disc *projectDiscoveryError
		if errors.As(err, &disc) {
			return 1
		}
		return 2
	}
	if parts[0] != project {
		fmt.Fprintln(errout, "object key must start with PROJECT/ENVIRONMENT/")
		return 2
	}
	progress = startWait(errout, true, "restoring state")
	rep, err := statebackup.Restore(ctx, statebackup.RestoreOptions{Endpoint: *endpoint, Region: *region, Bucket: *bucket, Object: *object,
		Out: *dir, KeyFile: *keyFile, Project: project, Environment: parts[1], AccessKey: access, SecretKey: secret})
	progress.Close()
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

// projectDiscoveryError is a storage failure while finding the project from object keys. Passing --project skips that listing.
type projectDiscoveryError struct{ err error }

func (e *projectDiscoveryError) Error() string {
	return "cannot list the bucket to find the project; pass --project to skip discovery: " + e.err.Error()
}

func (e *projectDiscoveryError) Unwrap() error { return e.err }

// resolveStateRestoreProject chooses the project for an offline restore: the flag, else the manifest in dir, else the only
// project that has environment among the bucket's scoped object keys.
func resolveStateRestoreProject(ctx context.Context, project, dir, environment string, store objectstore.Store) (string, error) {
	if project != "" {
		return project, nil
	}
	if name, ok, err := manifest.ProjectName(dir); err != nil {
		return "", fmt.Errorf("cannot read the project of %s: give --project", filepath.Join(dir, "berthy.yml"))
	} else if ok {
		return name, nil
	}
	have, err := projectsWithEnvironment(ctx, store, environment)
	if err != nil {
		return "", &projectDiscoveryError{err: err}
	}
	switch len(have) {
	case 0:
		return "", fmt.Errorf("no project has environment %s in the bucket: give --project", environment)
	case 1:
		return have[0], nil
	default:
		return "", fmt.Errorf("environment %s exists in several projects (%s): give --project, or run the command in the project's directory", environment, strings.Join(have, ", "))
	}
}

// projectsWithEnvironment lists the distinct projects that have at least one object under project/environment/ in store,
// using listed key names only.
func projectsWithEnvironment(ctx context.Context, store objectstore.Store, environment string) ([]string, error) {
	list, err := store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var have []string
	for _, o := range list {
		parts := strings.Split(o.Key, "/")
		if len(parts) < 3 || parts[1] != environment || !scope.ValidProject(parts[0]) {
			continue
		}
		if _, ok := seen[parts[0]]; ok {
			continue
		}
		seen[parts[0]] = struct{}{}
		have = append(have, parts[0])
	}
	slices.Sort(have)
	return have, nil
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
	return runStateBackup(ctx, agentclient.NewHTTPClient(), base, prefix, token, f.json, out, errout)
}

func runStateBackup(ctx context.Context, client *http.Client, base *url.URL, prefix, token string, asJSON bool, out, errout io.Writer) int {
	call := agentclient.Caller(ctx, client, base, prefix, token)
	data, status, err := call("POST", "state-backup", "")
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	if status != http.StatusAccepted {
		printRefusal(errout, safeSecretText(agentclient.Message(status, data), token), !asJSON)
		return 1
	}
	if code, held := reportHeldMarkers(data, out, errout, asJSON, nil, token); held {
		return code
	}
	var started struct {
		RunID int64 `json:"state_backup_run_id"`
	}
	if json.Unmarshal(data, &started) != nil || started.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	progress := startWait(errout, !asJSON, "waiting for state backup")
	defer progress.Close()
	deadline := time.Now().Add(backupWaitLimit)
	failures := 0
	for {
		data, status, err := call("GET", "state-backup/runs", "limit=100")
		var runs []api.StateBackupRun
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &runs) != nil {
			if failures++; failures >= 3 {
				progress.Close()
				fmt.Fprintln(errout, "run status unavailable")
				return 1
			}
		} else {
			failures = 0
			for _, run := range runs {
				if run.ID == started.RunID && run.Status != "running" {
					progress.Close()
					return printStateBackupRun(out, errout, run, asJSON)
				}
			}
		}
		if time.Now().After(deadline) {
			progress.Close()
			fmt.Fprintf(errout, "run %d did not finish in time\n", started.RunID)
			return 1
		}
		select {
		case <-time.After(backupPollInterval):
		case <-ctx.Done():
			progress.Close()
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
		fmt.Fprintf(out, "run: %d\nstatus: %s\n", run.ID, styleFor(out, true).Status(run.Status))
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
