package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/deploy"
)

var (
	// backupPollInterval is how often "backup run" asks whether its run has finished.
	backupPollInterval = 2 * time.Second
	// backupWaitLimit is how long "backup run" waits for a run, which is as long as the agent lets one take: the archive and
	// upload may use up to 24 hours, and a before job its own timeout, so the wait is generous and ends at the run's record.
	backupWaitLimit = 25 * time.Hour
)

// printBackups prints the backups in storage as a table, or as indented JSON when asJSON is set. Every text comes from the
// agent, so it is printed with control characters made visible.
func printBackups(out, errout io.Writer, body []byte, asJSON bool) int {
	if asJSON {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, pretty.String())
		return 0
	}
	var rows []api.BackupEntry
	if json.Unmarshal(body, &rows) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no backups")
		return 0
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKUP\tAPP\tVOLUME\tSIZE\tFINISHED\tSHA256\tKEY")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(r.Backup), terminalSafe(r.App), volumeOrCommand(r.Volume), api.HumanSize(r.Size), r.Finished.UTC().Format(time.RFC3339), terminalSafe(r.SHA256), terminalSafe(r.Key))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

// volumeOrCommand is what a backup names as its source: its volume, or "command" for a backup of the output of a command, which
// has no volume.
func volumeOrCommand(volume string) string {
	if volume == "" {
		return "command"
	}
	return terminalSafe(volume)
}

// sourceOf words the source of a backup: "volume uploads", or "command".
func sourceOf(volume string) string {
	if volume == "" {
		return "command"
	}
	return "volume " + terminalSafe(volume)
}

// printBackupRun prints a finished run and returns 0 when it succeeded.
func printBackupRun(out, errout io.Writer, run api.BackupRun, asJSON bool) int {
	if asJSON {
		pretty, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, string(pretty))
	} else {
		fmt.Fprintf(out, "run: %d\nstatus: %s\nbackup: %s\n", run.ID, styleFor(out, true).Status(run.Status), terminalSafe(run.Backup))
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

// printRestorePlan shows what a restore would read and which volume it would create, or which command it would feed and where
// that runs. Every text comes from the agent.
func printRestorePlan(out, errout io.Writer, data []byte) int {
	var plan struct {
		Restore *deploy.RestorePlan `json:"restore"`
	}
	if json.Unmarshal(data, &plan) != nil || plan.Restore == nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	r := plan.Restore
	fmt.Fprintf(out, "restore %s\n", terminalSafe(r.Key))
	if r.Into != "" {
		fmt.Fprintf(out, "into volume %s (new)\n", terminalSafe(r.Into))
	} else {
		fmt.Fprintf(out, "into the input of a command on %s\n", terminalSafe(r.Target))
		fmt.Fprintf(out, "command: %s\n", terminalSafe(strings.Join(r.Command, " ")))
	}
	fmt.Fprintf(out, "backup: %s (app %s, %s)\n", terminalSafe(r.Backup), terminalSafe(r.App), sourceOf(r.Volume))
	fmt.Fprintf(out, "taken: %s\n", r.Finished.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "size: %s\nsha256: %s\n", api.HumanSize(r.Size), terminalSafe(r.SHA256))
	return 0
}

// runBackupCommand runs "backup list" and "backup run NAME".
func runBackupCommand(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	call := agentclient.Caller(ctx, agentclient.NewHTTPClient(), base, prefix, token)
	switch {
	case len(f.pos) == 1 && f.pos[0] == "list":
		query := ""
		if f.backup != "" {
			query = url.Values{"backup": {f.backup}}.Encode()
		}
		progress := startWait(errout, !f.json, "reading backups")
		data, status, err := call("GET", "backups", query)
		progress.Close()
		if err != nil {
			fmt.Fprintln(errout, "agent connection failed")
			return 1
		}
		if status != http.StatusOK {
			fmt.Fprintln(errout, agentclient.Message(status, data))
			return 1
		}
		return printBackups(out, errout, data, f.json)
	case len(f.pos) == 2 && f.pos[0] == "run" && f.pos[1] != "" && f.backup == "":
		return runBackupRun(ctx, call, f.pos[1], f.json, out, errout)
	}
	fmt.Fprintln(errout, "usage: backup list [--backup NAME] | backup run NAME")
	return 2
}

// runBackupRun asks the agent to start the backup, waits for the run to finish, and prints the result. It exits 0 only when
// the run succeeded.
func runBackupRun(ctx context.Context, call func(method, path, query string) ([]byte, int, error), name string, asJSON bool, out, errout io.Writer) int {
	escaped := url.PathEscape(name)
	data, status, err := call("POST", "backups/"+escaped+"/run", "")
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	if status != http.StatusAccepted {
		fmt.Fprintln(errout, agentclient.Message(status, data))
		return 1
	}
	if code, held := reportHeld(data, out, errout, asJSON); held {
		return code
	}
	var started struct {
		RunID int64 `json:"backup_run_id"`
	}
	if json.Unmarshal(data, &started) != nil || started.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	progress := startWait(errout, !asJSON, "waiting for backup")
	defer progress.Close()
	deadline := time.Now().Add(backupWaitLimit)
	find := func(limit string) (api.BackupRun, bool, bool) {
		data, status, err := call("GET", "backups/"+escaped+"/runs", "limit="+limit)
		var runs []api.BackupRun
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &runs) != nil {
			return api.BackupRun{}, false, false
		}
		for _, r := range runs {
			if r.ID == started.RunID {
				return r, true, true
			}
		}
		return api.BackupRun{}, false, true
	}
	failures := 0
	for {
		run, found, ok := find("1")
		if ok && !found {
			// A newer run has taken the place of ours; look further back.
			run, found, ok = find("100")
		}
		switch {
		case !ok:
			if failures++; failures >= 3 {
				progress.Close()
				fmt.Fprintln(errout, "run status unavailable")
				return 1
			}
		case !found:
			progress.Close()
			fmt.Fprintln(errout, "run record unavailable")
			return 1
		default:
			failures = 0
			if run.Status != "running" {
				progress.Close()
				return printBackupRun(out, errout, run, asJSON)
			}
		}
		if time.Now().After(deadline) {
			progress.Close()
			fmt.Fprintf(errout, "run %d did not finish in time; see berthy backup list\n", started.RunID)
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
