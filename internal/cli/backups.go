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
	"text/tabwriter"
	"time"

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

// humanSize writes a byte count with a binary unit, as `backup list` shows sizes.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(r.Backup), terminalSafe(r.App), terminalSafe(r.Volume), humanSize(r.Size), r.Finished.UTC().Format(time.RFC3339), terminalSafe(r.SHA256), terminalSafe(r.Key))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
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
		fmt.Fprintf(out, "run: %d\nstatus: %s\nbackup: %s\n", run.ID, terminalSafe(run.Status), terminalSafe(run.Backup))
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

// printRestorePlan shows what a restore would read and which volume it would create. Every text comes from the agent.
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
	fmt.Fprintf(out, "into volume %s (new)\n", terminalSafe(r.Into))
	fmt.Fprintf(out, "backup: %s (app %s, volume %s)\n", terminalSafe(r.Backup), terminalSafe(r.App), terminalSafe(r.Volume))
	fmt.Fprintf(out, "taken: %s\n", r.Finished.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "size: %s\nsha256: %s\n", humanSize(r.Size), terminalSafe(r.SHA256))
	return 0
}

// runBackupCommand runs "backup list" and "backup run NAME".
func runBackupCommand(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	call := agentCaller(ctx, newAgentClient(), base, prefix, token)
	switch {
	case len(f.pos) == 1 && f.pos[0] == "list":
		query := ""
		if f.backup != "" {
			query = url.Values{"backup": {f.backup}}.Encode()
		}
		data, status, err := call("GET", "backups", query)
		if err != nil {
			fmt.Fprintln(errout, "agent connection failed")
			return 1
		}
		if status != http.StatusOK {
			fmt.Fprintln(errout, agentError(status, data))
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
		fmt.Fprintln(errout, agentError(status, data))
		return 1
	}
	var started struct {
		RunID int64 `json:"backup_run_id"`
	}
	if json.Unmarshal(data, &started) != nil || started.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
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
				fmt.Fprintln(errout, "run status unavailable")
				return 1
			}
		case !found:
			fmt.Fprintln(errout, "run record unavailable")
			return 1
		default:
			failures = 0
			if run.Status != "running" {
				return printBackupRun(out, errout, run, asJSON)
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(errout, "run %d did not finish in time; see berthy backup list\n", started.RunID)
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
