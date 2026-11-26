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
	"strconv"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
)

var (
	// jobPollInterval is how often "job run" asks whether its run has finished.
	jobPollInterval = 2 * time.Second
	// jobWaitGrace is how long past the job's timeout "job run" keeps waiting for the agent to record the result.
	jobWaitGrace = 30 * time.Second
)

// printJobs prints the jobs of the environment as a table, or as indented JSON when asJSON is set.
func printJobs(out, errout io.Writer, body []byte, asJSON bool) int {
	if asJSON {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, pretty.String())
		return 0
	}
	var rows []api.JobStatus
	if json.Unmarshal(body, &rows) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no jobs")
		return 0
	}
	s := styleFor(out, true)
	var table cliTable
	table.header(s, "NAME", "TARGET", "SCHEDULE", "NEXT", "LAST", "WHEN")
	for _, r := range rows {
		target := r.App
		if r.Target != "" {
			target += "/" + r.Target
		}
		schedule, next := r.Schedule, nextCell(r)
		if schedule == "" {
			schedule = "on demand"
		}
		last, when := "-", "-"
		if r.Last != nil {
			last = s.Status(r.Last.Status)
			if r.Last.Detail != "" {
				last += ": " + terminalSafe(r.Last.Detail)
			}
			t := r.Last.Started
			if r.Last.Finished != nil {
				t = *r.Last.Finished
			}
			when = t.UTC().Format(time.RFC3339)
		}
		table.row(terminalSafe(r.Name), terminalSafe(target), terminalSafe(schedule), s.paint(toneMuted, next), last, s.paint(toneMuted, when))
	}
	if err := table.write(out, s); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

// nextCell is the next occurrence of a job as the NEXT column shows it: on the clock of the manifest's zone with the zone's
// abbreviation, then the same instant in UTC, as in "2026-11-20 02:00 JST (2026-11-19 17:00Z)". The UTC part leaves the date
// out when it is the same day, and the whole of it when the zone is UTC. A job without an occurrence shows "-".
func nextCell(j api.JobStatus) string {
	local, ok := j.NextClock()
	if !ok {
		return "-"
	}
	text := local.Format("2006-01-02 15:04 MST")
	if local.Location() == time.UTC {
		return text
	}
	utc := local.UTC()
	layout := "15:04Z"
	if y, m, d := local.Date(); y != utc.Year() || m != utc.Month() || d != utc.Day() {
		layout = "2006-01-02 15:04Z"
	}
	return text + " (" + utc.Format(layout) + ")"
}

// runJobLogs runs "job logs NAME [--run ID]": it prints the output kept of a failed or timed-out run, the newest one that kept
// any unless a run is named. The run's line goes to stderr, so the output can be piped.
func runJobLogs(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	query := ""
	if f.run != "" {
		if n, err := strconv.ParseInt(f.run, 10, 64); err != nil || n < 1 {
			fmt.Fprintln(errout, "--run needs a run id")
			return 2
		}
		query = "run=" + url.QueryEscape(f.run)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	call := agentclient.Caller(ctx, agentclient.NewHTTPClient(), base, prefix, token)
	data, status, err := call("GET", "jobs/"+url.PathEscape(f.pos[1])+"/logs", query)
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(errout, "no output kept for %s: only a failed or timed-out run keeps its output\n", terminalSafe(f.pos[1]))
		return 1
	}
	var logs api.JobLogs
	if status != http.StatusOK {
		printRefusal(errout, safeSecretText(agentclient.Message(status, data), token), !f.json)
		return 1
	}
	if json.Unmarshal(data, &logs) != nil || logs.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if f.json {
		pretty, err := json.MarshalIndent(logs, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, string(pretty))
		return 0
	}
	line := fmt.Sprintf("run %d of %s %s", logs.RunID, terminalSafe(logs.Job), terminalSafe(logs.Status))
	if logs.Detail != "" {
		line += ": " + terminalSafe(logs.Detail)
	}
	if logs.Finished != nil {
		line += " at " + logs.Finished.UTC().Format(time.RFC3339)
	}
	fmt.Fprintln(errout, line)
	if logs.Output == "" {
		fmt.Fprintln(errout, "this run kept no output")
		return 0
	}
	// The output is the job's own: no control character of it reaches the terminal.
	for _, l := range strings.Split(strings.TrimSuffix(logs.Output, "\n"), "\n") {
		fmt.Fprintln(out, terminalSafe(l))
	}
	return 0
}

// runJobCommand runs "job run NAME": it asks the agent to start the job, waits for the run to finish, and prints the
// result. It exits 0 only when the run succeeded. "job logs NAME" shows what a failed run printed.
func runJobCommand(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	if len(f.pos) == 2 && f.pos[0] == "logs" && f.pos[1] != "" {
		return runJobLogs(f, base, prefix, token, out, errout)
	}
	if len(f.pos) == 2 && f.pos[0] == "cancel" && f.pos[1] != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return runJobCancel(ctx, f, base, prefix, token, out, errout)
	}
	if len(f.pos) != 2 || f.pos[0] != "run" || f.pos[1] == "" {
		fmt.Fprintln(errout, "usage: job run NAME | job logs NAME [--run ID] | job cancel NAME")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runJobRun(ctx, agentclient.NewHTTPClient(), base, prefix, token, f.pos[1], f.json, out, errout)
}

func runJobRun(ctx context.Context, client *http.Client, base *url.URL, prefix, token, name string, asJSON bool, out, errout io.Writer) int {
	jobName := name
	name = url.PathEscape(name)
	call := agentclient.Caller(ctx, client, base, prefix, token)
	data, status, err := call("POST", "jobs/"+name+"/run", "")
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	var started struct {
		RunID int64 `json:"run_id"`
	}
	if status != http.StatusAccepted {
		printRefusal(errout, safeSecretText(agentclient.Message(status, data), token), !asJSON)
		return 1
	}
	if code, held := reportHeld(data, out, errout, asJSON); held {
		return code
	}
	if json.Unmarshal(data, &started) != nil || started.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	progress := startWait(errout, !asJSON, "waiting for job")
	defer progress.Close()
	// The wait lasts as long as the job's timeout and a grace for the agent to record the result.
	timeout := 24 * time.Hour
	if data, status, err = call("GET", "jobs", ""); err == nil && status == http.StatusOK {
		var list []api.JobStatus
		if json.Unmarshal(data, &list) == nil {
			for _, j := range list {
				if d, e := time.ParseDuration(j.Timeout); j.Name == jobName && e == nil {
					timeout = d
				}
			}
		}
	}
	deadline := time.Now().Add(timeout + jobWaitGrace)
	find := func(limit string) (api.JobRun, bool, bool) {
		data, status, err := call("GET", "jobs/"+name+"/runs", "limit="+limit)
		var runs []api.JobRun
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &runs) != nil {
			return api.JobRun{}, false, false
		}
		for _, r := range runs {
			if r.ID == started.RunID {
				return r, true, true
			}
		}
		return api.JobRun{}, false, true
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
				return printRun(out, errout, run, asJSON)
			}
		}
		if time.Now().After(deadline) {
			progress.Close()
			fmt.Fprintf(errout, "run %d did not finish in time; see berthy jobs\n", started.RunID)
			return 1
		}
		select {
		case <-time.After(jobPollInterval):
		case <-ctx.Done():
			progress.Close()
			fmt.Fprintf(errout, "stopped waiting; run %d continues on the agent\n", started.RunID)
			return 1
		}
	}
}

// printRun prints a finished run and returns 0 when it succeeded.
func printRun(out, errout io.Writer, run api.JobRun, asJSON bool) int {
	if asJSON {
		pretty, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, string(pretty))
	} else {
		fmt.Fprintf(out, "run: %d\nstatus: %s\n", run.ID, styleFor(out, true).Status(run.Status))
		if run.ExitCode != nil {
			fmt.Fprintf(out, "exit_code: %d\n", *run.ExitCode)
		}
		if run.Detail != "" {
			fmt.Fprintf(out, "detail: %s\n", terminalSafe(run.Detail))
		}
		if run.HasOutput {
			fmt.Fprintf(out, "output: berthy job logs %s\n", terminalSafe(run.Job))
		}
	}
	if run.Status == "succeeded" {
		return 0
	}
	return 1
}
