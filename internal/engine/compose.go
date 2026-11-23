package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxComposeOutput = 1 << 20

// composeProject matches the project names the Compose CLI accepts.
var composeProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// Compose runs the Compose CLI against the agent's Docker daemon.
type Compose struct {
	Command []string // e.g. ["docker","compose"]
	Host    string   // DOCKER_HOST, e.g. "unix:///var/run/docker.sock"
}

// ComposeRun is one Compose command for a project whose files are in Dir.
type ComposeRun struct {
	Project, Dir string   // Dir holds compose.yaml and .env, mode 0700
	Args         []string // after the global flags
	DockerConfig string   // optional DOCKER_CONFIG dir with registry auths
	// NoFiles runs the command for the project name alone, with no Compose file or environment file. Compose finds the
	// project's containers, networks and volumes by their labels, which is all that bringing a project down needs. Dir is
	// then only the private working directory and home, and may be empty.
	NoFiles bool
}

// boundedBuffer keeps the first max bytes written to it and discards the rest. It never fails a write, so a chatty
// command is not stopped by its own output.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return len(p), nil
}

// Run executes `<Command> -p <Project> --project-directory <Dir> -f <Dir>/compose.yaml --env-file <Dir>/.env <Args...>`
// with a minimal environment: PATH, HOME set to Dir, DOCKER_HOST, COMPOSE_STATUS_STDOUT and DOCKER_CONFIG when set.
// Each stream keeps at most 1 MiB. Errors never include the environment file or the command output.
// The command runs in its own process group. Cancelling ctx kills the whole group, and on Linux the command also
// dies with the agent, so a Compose CLI never outlives the agent that started it.
func (c Compose) Run(ctx context.Context, r ComposeRun) (stdout, stderr []byte, err error) {
	if len(c.Command) == 0 || c.Command[0] == "" || c.Host == "" {
		return nil, nil, errors.New("compose unavailable")
	}
	if !composeProject.MatchString(r.Project) || r.Dir == "" || len(r.Args) == 0 {
		return nil, nil, errors.New("invalid compose command")
	}
	args := append([]string(nil), c.Command[1:]...)
	args = append(args, "-p", r.Project)
	if !r.NoFiles {
		args = append(args, "--project-directory", r.Dir, "-f", filepath.Join(r.Dir, "compose.yaml"), "--env-file", filepath.Join(r.Dir, ".env"))
	}
	args = append(args, r.Args...)
	cmd := exec.CommandContext(ctx, c.Command[0], args...)
	prepareCommand(cmd)
	cmd.Cancel = func() error { return killCommand(cmd) }
	cmd.Dir = r.Dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.Dir, "DOCKER_HOST=" + c.Host, "COMPOSE_STATUS_STDOUT=1"}
	if r.DockerConfig != "" {
		cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+r.DockerConfig)
	}
	cmd.WaitDelay = 10 * time.Second
	var out, errOut boundedBuffer
	out.max, errOut.max = maxComposeOutput, maxComposeOutput
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.buf.Bytes(), errOut.buf.Bytes(), ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.buf.Bytes(), errOut.buf.Bytes(), fmt.Errorf("compose exited with status %d", exit.ExitCode())
		}
		return out.buf.Bytes(), errOut.buf.Bytes(), errors.New("compose could not run")
	}
	return out.buf.Bytes(), errOut.buf.Bytes(), nil
}

// Version runs the Compose CLI's version command with HOME set to home, as the agent runs it, and returns the first
// line of its output.
func (c Compose) Version(ctx context.Context, home string) (string, error) {
	if len(c.Command) == 0 || c.Command[0] == "" || c.Host == "" || home == "" {
		return "", errors.New("unavailable")
	}
	args := append(append([]string(nil), c.Command[1:]...), "version")
	cmd := exec.CommandContext(ctx, c.Command[0], args...)
	prepareCommand(cmd)
	cmd.Cancel = func() error { return killCommand(cmd) }
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "DOCKER_HOST=" + c.Host}
	cmd.WaitDelay = time.Second
	var out boundedBuffer
	out.max = 4096
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", errors.New("unavailable")
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out.buf.String()), "\n")
	line = strings.TrimSpace(line)
	if line == "" || !utf8.ValidString(line) || strings.IndexFunc(line, func(r rune) bool { return r < ' ' }) >= 0 {
		return "", errors.New("unavailable")
	}
	return line, nil
}

// ComposeContainer is one container of a Compose project, as its labels describe it.
type ComposeContainer struct {
	ID, Service, ConfigHash string
	OneOff                  bool
	// State is Docker's container state, such as running or exited. Number is the replica number Compose gave the
	// container, or 0 when it gave none.
	State  string
	Number int
}

// ComposeContainers lists every container of a Compose project, stopped ones included.
func (e *Engine) ComposeContainers(ctx context.Context, project string) ([]ComposeContainer, error) {
	if !composeProject.MatchString(project) {
		return nil, fmt.Errorf("invalid Compose project")
	}
	f, err := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	if err != nil {
		return nil, err
	}
	q := url.Values{"all": {"1"}, "filters": {string(f)}}
	var raw []struct {
		Id     string
		State  string
		Labels map[string]string
	}
	if err = e.jsonRequest(ctx, "GET", "/containers/json?"+q.Encode(), nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]ComposeContainer, 0, len(raw))
	for _, c := range raw {
		if _, err = pathID(c.Id); err != nil {
			return nil, err
		}
		number, _ := strconv.Atoi(c.Labels["com.docker.compose.container-number"])
		out = append(out, ComposeContainer{
			State:      c.State,
			Number:     number,
			ID:         c.Id,
			Service:    c.Labels["com.docker.compose.service"],
			ConfigHash: c.Labels["com.docker.compose.config-hash"],
			OneOff:     strings.EqualFold(c.Labels["com.docker.compose.oneoff"], "true"),
		})
	}
	return out, nil
}

// ProjectVolumes lists the names of the volumes Compose created for a project, found by the project label, in name order.
// A volume the Compose file declares external carries no such label, so it is never listed.
func (e *Engine) ProjectVolumes(ctx context.Context, project string) ([]string, error) {
	if !composeProject.MatchString(project) {
		return nil, fmt.Errorf("invalid Compose project")
	}
	f, err := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	if err != nil {
		return nil, err
	}
	var raw struct {
		Volumes  []struct{ Name string }
		Warnings []string
	}
	if err = e.jsonRequest(ctx, "GET", "/volumes?"+url.Values{"filters": {string(f)}}.Encode(), nil, nil, &raw); err != nil {
		return nil, err
	}
	if len(raw.Warnings) > 0 {
		return nil, errors.New("volume inventory unavailable")
	}
	out := make([]string, 0, len(raw.Volumes))
	for _, v := range raw.Volumes {
		if _, err = pathID(v.Name); err != nil {
			return nil, err
		}
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out, nil
}

// VolumeLabels returns the labels of the named volume, and false when Docker holds no such volume.
func (e *Engine) VolumeLabels(ctx context.Context, name string) (map[string]string, bool, error) {
	p, err := pathID(name)
	if err != nil {
		return nil, false, err
	}
	var v struct{ Labels map[string]string }
	err = e.jsonRequest(ctx, "GET", "/volumes/"+p, nil, nil, &v)
	var status *StatusError
	switch {
	case err == nil:
		return v.Labels, true, nil
	case errors.As(err, &status) && status.Code == 404:
		return nil, false, nil
	}
	return nil, false, err
}

// InspectVolume returns the driver and options of a named volume.
func (e *Engine) InspectVolume(ctx context.Context, name string) (string, map[string]string, error) {
	p, err := pathID(name)
	if err != nil {
		return "", nil, err
	}
	var v struct {
		Driver  string
		Options map[string]string
	}
	err = e.jsonRequest(ctx, "GET", "/volumes/"+p, nil, nil, &v)
	return v.Driver, v.Options, err
}

// VolumeExists reports whether Docker holds a volume with this name. Docker answers 404 for a volume it does not hold.
func (e *Engine) VolumeExists(ctx context.Context, name string) (bool, error) {
	p, err := pathID(name)
	if err != nil {
		return false, err
	}
	err = e.jsonRequest(ctx, "GET", "/volumes/"+p, nil, nil, nil)
	if err == nil {
		return true, nil
	}
	var status *StatusError
	if errors.As(err, &status) && status.Code == 404 {
		return false, nil
	}
	return false, err
}
