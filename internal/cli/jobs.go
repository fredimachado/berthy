package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

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
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTARGET\tSCHEDULE\tNEXT\tLAST\tWHEN")
	for _, r := range rows {
		target := r.App
		if r.Target != "" {
			target += "/" + r.Target
		}
		schedule, next := r.Schedule, "-"
		if schedule == "" {
			schedule = "on demand"
		}
		if r.Next != nil {
			next = r.Next.UTC().Format(time.RFC3339)
		}
		last, when := "-", "-"
		if r.Last != nil {
			last = r.Last.Status
			if r.Last.Detail != "" {
				last += ": " + r.Last.Detail
			}
			t := r.Last.Started
			if r.Last.Finished != nil {
				t = *r.Last.Finished
			}
			when = t.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, target, schedule, next, last, when)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

// runJobCommand runs "job run NAME": it asks the agent to start the job, waits for the run to finish, and prints the
// result. It exits 0 only when the run succeeded.
func runJobCommand(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	if len(f.pos) != 2 || f.pos[0] != "run" || f.pos[1] == "" {
		fmt.Fprintln(errout, "usage: job run NAME")
		return 2
	}
	name := url.PathEscape(f.pos[1])
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	call := agentCaller(ctx, newAgentClient(), base, prefix, token)
	data, status, err := call("POST", "jobs/"+name+"/run", "")
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	var started struct {
		RunID int64 `json:"run_id"`
	}
	if status != http.StatusAccepted {
		fmt.Fprintln(errout, agentError(status, data))
		return 1
	}
	if json.Unmarshal(data, &started) != nil || started.RunID == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	// The wait lasts as long as the job's timeout and a grace for the agent to record the result.
	timeout := 24 * time.Hour
	if data, status, err = call("GET", "jobs", ""); err == nil && status == http.StatusOK {
		var list []api.JobStatus
		if json.Unmarshal(data, &list) == nil {
			for _, j := range list {
				if d, e := time.ParseDuration(j.Timeout); j.Name == f.pos[1] && e == nil {
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
				fmt.Fprintln(errout, "run status unavailable")
				return 1
			}
		case !found:
			fmt.Fprintln(errout, "run record unavailable")
			return 1
		default:
			failures = 0
			if run.Status != "running" {
				return printRun(out, errout, run, f.json)
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(errout, "run %d did not finish in time; see berthy jobs\n", started.RunID)
			return 1
		}
		select {
		case <-time.After(jobPollInterval):
		case <-ctx.Done():
			fmt.Fprintf(errout, "stopped waiting; run %d continues on the agent\n", started.RunID)
			return 1
		}
	}
}

// newAgentClient is the HTTP client of the commands that poll the agent: no proxy, short connect and header limits.
func newAgentClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 20 * time.Second}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// agentCaller returns a function that sends a request without a body to the agent under prefix and reads the answer, at most
// 1 MiB of it.
func agentCaller(ctx context.Context, client *http.Client, base *url.URL, prefix, token string) func(method, path, query string) ([]byte, int, error) {
	return func(method, path, query string) ([]byte, int, error) {
		u := *base
		u.Path, u.RawQuery = prefix+path, query
		resp, err := send(ctx, client, method, u.String(), token, nil)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return nil, 0, fmt.Errorf("invalid agent response")
		}
		return data, resp.StatusCode, nil
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
		fmt.Fprintf(out, "run: %d\nstatus: %s\n", run.ID, run.Status)
		if run.ExitCode != nil {
			fmt.Fprintf(out, "exit_code: %d\n", *run.ExitCode)
		}
		if run.Detail != "" {
			fmt.Fprintf(out, "detail: %s\n", run.Detail)
		}
	}
	if run.Status == "succeeded" {
		return 0
	}
	return 1
}
