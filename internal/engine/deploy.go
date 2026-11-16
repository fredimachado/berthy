package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxDockerResponse = 8 << 20
const maxCommandOutput = 1 << 20

type RegistryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress,omitempty"`
}
type UpdateStatus struct{ State, Message string }
type ServiceSnapshot struct {
	ID                 string
	Version            uint64
	Spec, PreviousSpec json.RawMessage
	UpdateStatus       UpdateStatus
}
type ServiceTask struct {
	ID, ServiceID, NodeID, ContainerID, State, DesiredState, Image string
	Slot, Version, SpecVersion                                     uint64
	Labels                                                         map[string]string
	// Timestamp is when the task last changed state: Docker's status timestamp, or its last update when that is missing.
	Timestamp time.Time
}
type Network struct {
	ID, Name, Driver, Scope string
	Attachable              bool
}
type Image struct {
	ID          string
	RepoDigests []string
	Healthcheck json.RawMessage
}
type ContainerSnapshot struct {
	ID, ImageID, State, StartedAt string
	Labels                        map[string]string
	Running                       bool
	ExitCode                      int
}
type ExecResult struct {
	Output   []byte
	ExitCode int
}
type InventoryImage struct {
	ID                    string
	RepoTags, RepoDigests []string
}
type InventoryContainer struct {
	ID, Image, ImageID, State string
	Labels                    map[string]string
}
type InventoryService struct {
	ID                 string
	Spec, PreviousSpec json.RawMessage
}

var dockerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func pathID(id string) (string, error) {
	if !dockerID.MatchString(id) {
		return "", fmt.Errorf("invalid Docker identifier")
	}
	return url.PathEscape(id), nil
}
func authHeader(auth *RegistryAuth) (string, error) {
	if auth == nil {
		return "", nil
	}
	b, e := json.Marshal(auth)
	if e != nil {
		return "", e
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
func (e *Engine) request(ctx context.Context, method, path string, body []byte, auth *RegistryAuth, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.44"+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid Docker request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h, err := authHeader(auth)
	if err != nil {
		return nil, err
	}
	if h != "" {
		req.Header.Set("X-Registry-Auth", h)
	}
	client := e.client
	if stream {
		client = &http.Client{Transport: e.client.Transport}
		if _, ok := ctx.Deadline(); !ok {
			return nil, fmt.Errorf("Docker stream requires a deadline")
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Docker unavailable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode}
	}
	return resp, nil
}
func (e *Engine) jsonRequest(ctx context.Context, method, path string, body []byte, auth *RegistryAuth, out any) error {
	resp, err := e.request(ctx, method, path, body, auth, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDockerResponse))
		return err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDockerResponse+1))
	if err != nil {
		return fmt.Errorf("Docker response read failed")
	}
	if len(b) > maxDockerResponse {
		return fmt.Errorf("Docker response too large")
	}
	if err = json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("invalid Docker response")
	}
	return nil
}
func (e *Engine) InspectService(ctx context.Context, id string) (ServiceSnapshot, error) {
	p, err := pathID(id)
	if err != nil {
		return ServiceSnapshot{}, err
	}
	var x struct {
		ID                 string
		Version            struct{ Index uint64 }
		Spec, PreviousSpec json.RawMessage
		UpdateStatus       UpdateStatus
	}
	err = e.jsonRequest(ctx, "GET", "/services/"+p, nil, nil, &x)
	return ServiceSnapshot{ID: x.ID, Version: x.Version.Index, Spec: x.Spec, PreviousSpec: x.PreviousSpec, UpdateStatus: x.UpdateStatus}, err
}
func validSpec(s json.RawMessage) bool {
	return len(s) > 0 && len(s) <= maxDockerResponse && json.Valid(s) && s[0] == '{'
}
func (e *Engine) CreateService(ctx context.Context, spec json.RawMessage, auth *RegistryAuth) (string, error) {
	if !validSpec(spec) {
		return "", fmt.Errorf("invalid service spec")
	}
	var x struct{ ID string }
	err := e.jsonRequest(ctx, "POST", "/services/create", spec, auth, &x)
	return x.ID, err
}
func (e *Engine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *RegistryAuth) error {
	p, err := pathID(id)
	if err != nil {
		return err
	}
	if !validSpec(spec) {
		return fmt.Errorf("invalid service spec")
	}
	return e.jsonRequest(ctx, "POST", "/services/"+p+"/update?version="+strconv.FormatUint(version, 10), spec, auth, nil)
}
func (e *Engine) DeleteService(ctx context.Context, id string) error {
	p, err := pathID(id)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "DELETE", "/services/"+p, nil, nil, nil)
}
func (e *Engine) InspectNetwork(ctx context.Context, name string) (Network, error) {
	p, err := pathID(name)
	if err != nil {
		return Network{}, err
	}
	var n Network
	err = e.jsonRequest(ctx, "GET", "/networks/"+p, nil, nil, &n)
	return n, err
}
func refPath(ref string) (string, error) {
	if ref == "" || len(ref) > 512 || strings.ContainsAny(ref, "?#\x00\r\n") {
		return "", fmt.Errorf("invalid image reference")
	}
	return url.PathEscape(ref), nil
}
func (e *Engine) ResolveImage(ctx context.Context, ref string, auth *RegistryAuth) (string, error) {
	p, err := refPath(ref)
	if err != nil {
		return "", err
	}
	var x struct{ Descriptor struct{ Digest string } }
	if err = e.jsonRequest(ctx, "GET", "/distribution/"+p+"/json", nil, auth, &x); err != nil {
		return "", err
	}
	if x.Descriptor.Digest == "" {
		return "", fmt.Errorf("Docker returned no digest")
	}
	return x.Descriptor.Digest, nil
}
func (e *Engine) PullImage(ctx context.Context, ref string, auth *RegistryAuth) error {
	if _, err := refPath(ref); err != nil {
		return err
	}
	q := url.Values{"fromImage": {ref}}
	resp, err := e.request(ctx, "POST", "/images/create?"+q.Encode(), nil, auth, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited := &io.LimitedReader{R: resp.Body, N: maxDockerResponse + 1}
	d := json.NewDecoder(limited)
	for {
		var x struct {
			Error       string
			ErrorDetail struct{ Message string }
		}
		err = d.Decode(&x)
		if err == io.EOF && limited.N > 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid Docker pull response")
		}
		if x.Error != "" || x.ErrorDetail.Message != "" {
			return fmt.Errorf("Docker image pull failed")
		}
	}
}
func (e *Engine) InspectImage(ctx context.Context, ref string) (Image, error) {
	p, err := refPath(ref)
	if err != nil {
		return Image{}, err
	}
	var x struct {
		ID          string
		RepoDigests []string
		Config      struct{ Healthcheck json.RawMessage }
	}
	err = e.jsonRequest(ctx, "GET", "/images/"+p+"/json", nil, nil, &x)
	return Image{ID: x.ID, RepoDigests: x.RepoDigests, Healthcheck: x.Config.Healthcheck}, err
}
func (e *Engine) ServiceTasks(ctx context.Context, serviceID string) ([]ServiceTask, error) {
	if _, err := pathID(serviceID); err != nil {
		return nil, err
	}
	f, _ := json.Marshal(map[string][]string{"service": {serviceID}})
	q := url.Values{"filters": {string(f)}}
	var all []struct {
		ID, ServiceID, NodeID, DesiredState string
		Slot                                uint64
		UpdatedAt                           time.Time
		Version                             struct{ Index uint64 }
		SpecVersion                         struct{ Index uint64 }
		Spec                                struct {
			ContainerSpec struct {
				Labels map[string]string
				Image  string
			}
		}
		Status struct {
			State           string
			Timestamp       time.Time
			ContainerStatus struct{ ContainerID string }
		}
	}
	if err := e.jsonRequest(ctx, "GET", "/tasks?"+q.Encode(), nil, nil, &all); err != nil {
		return nil, err
	}
	out := make([]ServiceTask, 0, len(all))
	for _, t := range all {
		if t.ServiceID != serviceID {
			continue
		}
		at := t.Status.Timestamp
		if at.IsZero() {
			at = t.UpdatedAt
		}
		out = append(out, ServiceTask{ID: t.ID, ServiceID: t.ServiceID, NodeID: t.NodeID, ContainerID: t.Status.ContainerStatus.ContainerID, State: t.Status.State, DesiredState: t.DesiredState, Slot: t.Slot, Version: t.Version.Index, SpecVersion: t.SpecVersion.Index, Labels: t.Spec.ContainerSpec.Labels, Image: t.Spec.ContainerSpec.Image, Timestamp: at})
	}
	return out, nil
}

// FailedTasksSince counts the tasks that failed at or after since: tasks Swarm marked failed or rejected. Swarm marks a
// container that exits on its own as failed. A task that was shut down, such as the old tasks of a rolling update, is not
// a failure whatever its exit code. Tasks without a timestamp are not counted.
func FailedTasksSince(tasks []ServiceTask, since time.Time) int {
	n := 0
	for _, t := range tasks {
		if t.Timestamp.IsZero() || t.Timestamp.Before(since) {
			continue
		}
		if t.State == "failed" || t.State == "rejected" {
			n++
		}
	}
	return n
}
func (e *Engine) InspectContainer(ctx context.Context, id string) (ContainerSnapshot, error) {
	p, err := pathID(id)
	if err != nil {
		return ContainerSnapshot{}, err
	}
	var x struct {
		ID, Image string
		Config    struct{ Labels map[string]string }
		State     struct {
			Status, StartedAt string
			Running           bool
			ExitCode          int
		}
	}
	err = e.jsonRequest(ctx, "GET", "/containers/"+p+"/json", nil, nil, &x)
	return ContainerSnapshot{ID: x.ID, ImageID: x.Image, Labels: x.Config.Labels, State: x.State.Status, StartedAt: x.State.StartedAt, Running: x.State.Running, ExitCode: x.State.ExitCode}, err
}
func (e *Engine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	p, err := pathID(id)
	if err != nil {
		return "", err
	}
	var x struct {
		State struct{ Health *struct{ Status string } }
	}
	if err = e.jsonRequest(ctx, "GET", "/containers/"+p+"/json", nil, nil, &x); err != nil {
		return "", err
	}
	if x.State.Health == nil {
		return "none", nil
	}
	return x.State.Health.Status, nil
}
func (e *Engine) CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	if _, err := pathID(name); err != nil {
		return "", err
	}
	if !validSpec(spec) {
		return "", fmt.Errorf("invalid container spec")
	}
	q := url.Values{"name": {name}}
	var x struct{ ID string }
	err := e.jsonRequest(ctx, "POST", "/containers/create?"+q.Encode(), spec, nil, &x)
	return x.ID, err
}
func (e *Engine) StartContainer(ctx context.Context, id string) error {
	p, err := pathID(id)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "POST", "/containers/"+p+"/start", nil, nil, nil)
}
func (e *Engine) StopContainer(ctx context.Context, id string, seconds int) error {
	p, err := pathID(id)
	if err != nil {
		return err
	}
	if seconds < 0 || seconds > 300 {
		return fmt.Errorf("invalid stop timeout")
	}
	return e.jsonRequest(ctx, "POST", "/containers/"+p+"/stop?t="+strconv.Itoa(seconds), nil, nil, nil)
}
func (e *Engine) WaitContainer(ctx context.Context, id string) (int, error) {
	p, err := pathID(id)
	if err != nil {
		return 0, err
	}
	resp, err := e.request(ctx, "POST", "/containers/"+p+"/wait?condition=not-running", nil, nil, true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var x struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxDockerResponse)).Decode(&x); err != nil {
		return 0, fmt.Errorf("invalid Docker wait response")
	}
	if x.Error != nil && x.Error.Message != "" {
		return 0, fmt.Errorf("Docker container wait failed")
	}
	return x.StatusCode, nil
}
func (e *Engine) RemoveContainer(ctx context.Context, id string) error {
	p, err := pathID(id)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "DELETE", "/containers/"+p, nil, nil, nil)
}

// dockerOutput consumes the non-TTY multiplexed stream without exposing framing. It refuses more than maxCommandOutput.
func dockerOutput(r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	if err := dockerStream(r, &out, maxCommandOutput); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// dockerStream copies the payload of the non-TTY multiplexed stream r to w, validating its framing. A frame is copied
// whole, so memory stays bounded by w. limit is the most payload to accept, or a negative number for no limit.
func dockerStream(r io.Reader, w io.Writer, limit int64) error {
	var total int64
	for {
		var hdr [8]byte
		_, err := io.ReadFull(r, hdr[:])
		if err == io.EOF {
			return nil
		}
		if err != nil || (hdr[0] != 1 && hdr[0] != 2) || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return fmt.Errorf("invalid Docker output stream")
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		if limit >= 0 && size > limit-total {
			return fmt.Errorf("Docker output exceeds limit")
		}
		total += size
		if _, err = io.CopyN(w, r, size); err != nil {
			return fmt.Errorf("invalid Docker output stream")
		}
	}
}
func (e *Engine) ContainerLogs(ctx context.Context, id string) ([]byte, error) {
	p, err := pathID(id)
	if err != nil {
		return nil, err
	}
	resp, err := e.request(ctx, "GET", "/containers/"+p+"/logs?stdout=1&stderr=1", nil, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return dockerOutput(resp.Body)
}
func (e *Engine) Exec(ctx context.Context, id string, command []string) (ExecResult, error) {
	var out bytes.Buffer
	code, err := e.exec(ctx, id, command, func(r io.Reader) error {
		o, err := dockerOutput(r)
		out.Write(o)
		return err
	})
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Output: out.Bytes(), ExitCode: code}, nil
}

// ExecDiscard runs a command in a container like Exec, but reads the command's output and throws it away, so a command
// that prints a great deal still returns its exit code. Memory use does not depend on the amount of output. ctx must have
// a deadline.
func (e *Engine) ExecDiscard(ctx context.Context, id string, command []string) (int, error) {
	return e.exec(ctx, id, command, func(r io.Reader) error { return dockerStream(r, io.Discard, -1) })
}

// exec creates an exec in a container, starts it, hands the output stream to consume, and returns the exit code.
func (e *Engine) exec(ctx context.Context, id string, command []string, consume func(io.Reader) error) (int, error) {
	p, err := pathID(id)
	if err != nil {
		return 0, err
	}
	if len(command) == 0 || command[0] == "" {
		return 0, fmt.Errorf("empty command")
	}
	body, err := json.Marshal(struct {
		AttachStdout, AttachStderr bool
		Tty                        bool
		Cmd                        []string
	}{true, true, false, command})
	if err != nil {
		return 0, err
	}
	var x struct{ ID string }
	if err = e.jsonRequest(ctx, "POST", "/containers/"+p+"/exec", body, nil, &x); err != nil {
		return 0, err
	}
	p, err = pathID(x.ID)
	if err != nil {
		return 0, fmt.Errorf("invalid Docker exec response")
	}
	resp, err := e.request(ctx, "POST", "/exec/"+p+"/start", []byte(`{"Detach":false,"Tty":false}`), nil, true)
	if err != nil {
		return 0, err
	}
	err = consume(resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, err
	}
	var status struct {
		Running  bool
		ExitCode *int
	}
	if err = e.jsonRequest(ctx, "GET", "/exec/"+p+"/json", nil, nil, &status); err != nil {
		return 0, err
	}
	if status.Running || status.ExitCode == nil {
		return 0, fmt.Errorf("Docker exec has no exit status")
	}
	return *status.ExitCode, nil
}
func (e *Engine) InventoryImages(ctx context.Context) ([]InventoryImage, error) {
	var x []InventoryImage
	err := e.jsonRequest(ctx, "GET", "/images/json?all=1", nil, nil, &x)
	return x, err
}
func (e *Engine) InventoryContainers(ctx context.Context) ([]InventoryContainer, error) {
	var x []InventoryContainer
	err := e.jsonRequest(ctx, "GET", "/containers/json?all=1", nil, nil, &x)
	return x, err
}
func (e *Engine) InventoryServices(ctx context.Context) ([]InventoryService, error) {
	var x []InventoryService
	err := e.jsonRequest(ctx, "GET", "/services", nil, nil, &x)
	return x, err
}
func (e *Engine) RemoveImageRef(ctx context.Context, ref string) error {
	if !strings.Contains(ref, "@sha256:") {
		return fmt.Errorf("immutable image reference required")
	}
	p, err := refPath(ref)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "DELETE", "/images/"+p, nil, nil, nil)
}

// RemoveUnusedImage removes ref, an image ID or a name:tag. It does not force the removal, so Docker refuses an image
// that a container uses.
func (e *Engine) RemoveUnusedImage(ctx context.Context, ref string) error {
	p, err := refPath(ref)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "DELETE", "/images/"+p, nil, nil, nil)
}

// ImportImage creates the image repo:tag from archive, a tar of its root filesystem, with the Dockerfile instructions in
// changes applied. Docker reports a failed import in its progress stream, which is read to the end.
func (e *Engine) ImportImage(ctx context.Context, repo, tag string, changes []string, archive []byte) error {
	if _, err := refPath(repo + ":" + tag); err != nil {
		return err
	}
	q := url.Values{"fromSrc": {"-"}, "repo": {repo}, "tag": {tag}, "changes": changes}
	resp, err := e.request(ctx, "POST", "/images/create?"+q.Encode(), archive, nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited := &io.LimitedReader{R: resp.Body, N: maxDockerResponse + 1}
	d := json.NewDecoder(limited)
	for {
		var x struct {
			Error       string
			ErrorDetail struct{ Message string }
		}
		err = d.Decode(&x)
		if err == io.EOF && limited.N > 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid Docker import response")
		}
		if x.Error != "" || x.ErrorDetail.Message != "" {
			return fmt.Errorf("Docker image import failed")
		}
	}
}

// PortBinding is a host address and port that a container port is published on.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// ContainerDetail is the part of a container's inspect output that the agent checks its own containers against.
type ContainerDetail struct {
	ID, ImageID, User, RestartPolicy string
	Running, ReadonlyRootfs          bool
	CapDrop, SecurityOpt             []string
	Memory                           int64
	Labels                           map[string]string
	// PortBindings are the publications asked for, and Ports the ones Docker made.
	PortBindings map[string][]PortBinding
	Ports        map[string][]PortBinding
	// Networks maps each network the container is attached to by name to its ID.
	Networks map[string]string
}

// InspectContainerDetail returns the settings of a container that the agent's own containers are checked against.
func (e *Engine) InspectContainerDetail(ctx context.Context, id string) (ContainerDetail, error) {
	p, err := pathID(id)
	if err != nil {
		return ContainerDetail{}, err
	}
	var x struct {
		ID, Image string
		Config    struct {
			User   string
			Labels map[string]string
		}
		State      struct{ Running bool }
		HostConfig struct {
			ReadonlyRootfs       bool
			CapDrop, SecurityOpt []string
			Memory               int64
			RestartPolicy        struct{ Name string }
			PortBindings         map[string][]PortBinding
		}
		NetworkSettings struct {
			Ports    map[string][]PortBinding
			Networks map[string]struct{ NetworkID string }
		}
	}
	if err = e.jsonRequest(ctx, "GET", "/containers/"+p+"/json", nil, nil, &x); err != nil {
		return ContainerDetail{}, err
	}
	networks := make(map[string]string, len(x.NetworkSettings.Networks))
	for name, n := range x.NetworkSettings.Networks {
		networks[name] = n.NetworkID
	}
	return ContainerDetail{
		ID: x.ID, ImageID: x.Image, User: x.Config.User, Labels: x.Config.Labels, Running: x.State.Running,
		ReadonlyRootfs: x.HostConfig.ReadonlyRootfs, CapDrop: x.HostConfig.CapDrop, SecurityOpt: x.HostConfig.SecurityOpt,
		Memory: x.HostConfig.Memory, RestartPolicy: x.HostConfig.RestartPolicy.Name, PortBindings: x.HostConfig.PortBindings,
		Ports: x.NetworkSettings.Ports, Networks: networks,
	}, nil
}
