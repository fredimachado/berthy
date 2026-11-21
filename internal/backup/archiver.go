// Package backup backs up named Docker volumes to object storage and restores them. The Archiver in this file is the part
// that touches Docker: it reads a volume into an archive stream and fills a new volume from one.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"berthy/internal/engine"
	"berthy/internal/scope"
	"berthy/internal/volarchive"
)

const (
	// helperMemory is the memory limit of an archive or restore container.
	helperMemory = 256 << 20
	// maxRun bounds one archive or restore, the helper's whole life. The engine's streaming calls need a deadline.
	maxRun = 24 * time.Hour
	// cleanupTimeout bounds the removal of a container or a volume after a run, which must not depend on the run's context:
	// that is often the very reason for the removal.
	cleanupTimeout = 30 * time.Second
	// drainTimeout is how long a restore waits for the helper's last words after it exited.
	drainTimeout = 5 * time.Second
)

// ArchiverEngine is the part of the Docker API the archiver uses. *engine.Engine implements it.
type ArchiverEngine interface {
	CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error)
	Attach(ctx context.Context, id string, stdin bool) (io.ReadCloser, io.WriteCloser, *engine.BoundedBuffer, error)
	StartContainer(ctx context.Context, id string) error
	WaitContainer(ctx context.Context, id string) (int, error)
	RemoveContainer(ctx context.Context, id string) error
	InventoryContainers(ctx context.Context) ([]engine.InventoryContainer, error)
	VolumeExists(ctx context.Context, name string) (bool, error)
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
}

var _ ArchiverEngine = (*engine.Engine)(nil)

// Archiver reads a volume into a compressed archive and extracts an archive into a new volume. Each run is one throwaway
// container made from the agent's own helper image, with no network, a read-only root and only the capabilities its work
// needs; the archive passes through the container's stdout and stdin, so nothing is staged on disk.
type Archiver struct {
	Engine ArchiverEngine
	// Image returns the helper image: the agent's static binary as a one-file image.
	Image        func(ctx context.Context) (string, error)
	Project, Env string
	// Log receives what a helper said on stderr when a run fails, and a container that could not be removed. It may be nil.
	// The text can name files, so it goes to the agent's log and never into an error that API callers see.
	Log func(string)
}

func (a *Archiver) log(format string, args ...any) {
	if a.Log != nil {
		a.Log(fmt.Sprintf(format, args...))
	}
}

// Archive streams the volume's tar.gz to w and returns the bytes written. A volume that does not exist is an error: Docker
// would create it empty for the mount. A helper that exits with an error, a failed stream or write, or the end of ctx, is
// an error too. The container is always removed.
func (a *Archiver) Archive(ctx context.Context, volume string, w io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, maxRun)
	defer cancel()
	exists, err := a.Engine.VolumeExists(ctx, volume)
	if err != nil {
		return 0, fmt.Errorf("volume lookup failed: %w", err)
	}
	if !exists {
		return 0, fmt.Errorf("volume %s not found", volume)
	}
	id, err := a.create(ctx, "archive", volume)
	if err != nil {
		return 0, err
	}
	defer a.removeContainer(ctx, id)
	out, _, stderr, err := a.Engine.Attach(ctx, id, false)
	if err != nil {
		return 0, fmt.Errorf("helper attach failed: %w", err)
	}
	defer out.Close()
	if err = a.Engine.StartContainer(ctx, id); err != nil {
		return 0, fmt.Errorf("helper start failed: %w", err)
	}
	n, err := io.Copy(w, out)
	if err != nil {
		a.log("archive of %s stopped: %v: helper said %q", volume, err, stderr.String())
		return n, fmt.Errorf("volume archive stream failed: %w", err)
	}
	code, err := a.Engine.WaitContainer(ctx, id)
	if err != nil {
		return n, fmt.Errorf("helper wait failed: %w", err)
	}
	if code != 0 {
		a.log("archive of %s: helper exited %d: %q", volume, code, stderr.String())
		return n, fmt.Errorf("volume archive failed (helper exit %d)", code)
	}
	return n, nil
}

// ErrRestoreVolumeRemoved marks a failed restore whose newly created volume was confirmed gone.
var ErrRestoreVolumeRemoved = errors.New("restore-created volume removed")

type removedRestoreVolume struct{ err error }

func (e removedRestoreVolume) Error() string        { return e.err.Error() }
func (e removedRestoreVolume) Unwrap() error        { return e.err }
func (e removedRestoreVolume) Is(target error) bool { return target == ErrRestoreVolumeRemoved }

// Restore creates the new volume and extracts the archive in r into it, then calls verify, the caller's check of the
// checksum of what it fed into r. A volume that exists is refused, so a restore never writes into someone's data. A
// failure at any step removes the container and the volume that this call created.
func (a *Archiver) Restore(ctx context.Context, volume string, r io.Reader, labels map[string]string, verify func() error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, maxRun)
	defer cancel()
	exists, err := a.Engine.VolumeExists(ctx, volume)
	if err != nil {
		return fmt.Errorf("volume lookup failed: %w", err)
	}
	if exists {
		return fmt.Errorf("volume %s already exists; choose an unused name", volume)
	}
	if err = a.Engine.CreateVolume(ctx, volume, labels); err != nil {
		return fmt.Errorf("volume create failed: %w", err)
	}
	// From here the volume is this call's, and goes if the restore does not complete. The container goes first: Docker does
	// not remove a volume that a container still mounts.
	defer func() {
		if err != nil && a.removeVolume(ctx, volume) {
			err = removedRestoreVolume{err}
		}
	}()
	return a.extract(ctx, volume, r, verify)
}

func (a *Archiver) extract(ctx context.Context, volume string, r io.Reader, verify func() error) error {
	id, err := a.create(ctx, "restore", volume)
	if err != nil {
		return err
	}
	defer a.removeContainer(ctx, id)
	out, in, stderr, err := a.Engine.Attach(ctx, id, true)
	if err != nil {
		return fmt.Errorf("helper attach failed: %w", err)
	}
	// The helper's stdout is empty, but reading it to the end is what delivers its stderr.
	drained := make(chan struct{})
	go func() {
		io.Copy(io.Discard, out)
		close(drained)
	}()
	defer func() {
		out.Close()
		<-drained
	}()
	if err = a.Engine.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("helper start failed: %w", err)
	}
	_, err = io.Copy(in, r)
	if cerr := in.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		a.log("restore into %s stopped: %v: helper said %q", volume, err, stderr.String())
		return fmt.Errorf("volume restore stream failed: %w", err)
	}
	code, err := a.Engine.WaitContainer(ctx, id)
	if err != nil {
		return fmt.Errorf("helper wait failed: %w", err)
	}
	select {
	case <-drained:
	case <-time.After(drainTimeout):
	}
	if code != 0 {
		a.log("restore into %s: helper exited %d: %q", volume, code, stderr.String())
		return fmt.Errorf("volume restore failed (helper exit %d)", code)
	}
	if err = verify(); err != nil {
		return fmt.Errorf("volume restore verification failed: %w", err)
	}
	return nil
}

// create makes the helper container of the given kind, "archive" or "restore", for the volume and returns its ID.
func (a *Archiver) create(ctx context.Context, kind, volume string) (string, error) {
	image, err := a.Image(ctx)
	if err != nil {
		return "", fmt.Errorf("helper image unavailable: %w", err)
	}
	id := scope.ID{Project: a.Project, Environment: a.Env}
	spec, err := helperSpec(kind, id, image, volume)
	if err != nil {
		return "", err
	}
	suffix := make([]byte, 4)
	if _, err = rand.Read(suffix); err != nil {
		return "", err
	}
	name := "berthy-" + kind + "-" + id.Slug() + "-" + hex.EncodeToString(suffix)
	containerID, err := a.Engine.CreateContainer(ctx, name, spec)
	if err != nil {
		return "", fmt.Errorf("helper create failed: %w", err)
	}
	if containerID == "" {
		return "", errors.New("helper create failed: no container ID")
	}
	return containerID, nil
}

// RemoveLeftovers removes the archive and restore helper containers of this environment that exist, and returns how many it
// removed. It is for an agent's start: a helper outlives an agent that was killed, and no run of the new agent has started
// one yet, so every such container is a leftover. A container that cannot be removed is logged and does not stop the rest.
func (a *Archiver) RemoveLeftovers(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	containers, err := a.Engine.InventoryContainers(ctx)
	if err != nil {
		return 0, fmt.Errorf("container inventory failed: %w", err)
	}
	removed := 0
	for _, c := range containers {
		if !(scope.ID{Project: a.Project, Environment: a.Env}).Owns(c.Labels) || (c.Labels["berthy.helper"] != "archive" && c.Labels["berthy.helper"] != "restore") {
			continue
		}
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if err := a.Engine.RemoveContainer(ctx, c.ID); err != nil {
			var status *engine.StatusError
			if !errors.As(err, &status) || status.Code != 404 {
				a.log("leftover helper container %s was not removed: %v", name, err)
			}
			continue
		}
		a.log("removed leftover helper container %s (%s, %s)", name, c.Labels["berthy.helper"], c.State)
		removed++
	}
	return removed, nil
}

// removeContainer removes the helper container, running or not, on a context that the end of the run does not cancel.
func (a *Archiver) removeContainer(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := a.Engine.RemoveContainer(ctx, id); err != nil {
		var status *engine.StatusError
		if !errors.As(err, &status) || status.Code != 404 {
			a.log("helper container %s was not removed: %v", id, err)
		}
	}
}

func (a *Archiver) removeVolume(ctx context.Context, volume string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := a.Engine.RemoveVolume(ctx, volume); err != nil {
		var status *engine.StatusError
		if !errors.As(err, &status) || status.Code != 404 {
			a.log("volume %s was not removed: %v", volume, err)
			return false
		}
	}
	exists, err := a.Engine.VolumeExists(ctx, volume)
	return err == nil && !exists
}

// helperSpec is the helper container of a run. Both kinds run as root without network and with a read-only root, drop every
// capability and add back only what the work needs: reading any file of the volume, or creating, owning and moding files
// in it. The volume is mounted without copying the image's content into it.
func helperSpec(kind string, id scope.ID, image, volume string) (json.RawMessage, error) {
	labels := id.Labels()
	labels["berthy.helper"] = kind
	mount := map[string]any{
		"Type":          "volume",
		"Source":        volume,
		"Target":        volarchive.SourceDir,
		"ReadOnly":      true,
		"VolumeOptions": map[string]any{"NoCopy": true},
	}
	spec := map[string]any{
		"Image":        image,
		"Entrypoint":   []string{"/berthy", "volume-archive"},
		"User":         "0:0",
		"AttachStdout": true,
		"AttachStderr": true,
		"Labels":       labels,
	}
	host := map[string]any{
		"NetworkMode":    "none",
		"ReadonlyRootfs": true,
		"CapDrop":        []string{"ALL"},
		"CapAdd":         []string{"DAC_READ_SEARCH"},
		"SecurityOpt":    []string{"no-new-privileges"},
		"Memory":         helperMemory,
		"Mounts":         []any{mount},
	}
	spec["HostConfig"] = host
	switch kind {
	case "archive":
	case "restore":
		spec["Entrypoint"] = []string{"/berthy", "volume-extract"}
		spec["AttachStdin"], spec["OpenStdin"], spec["StdinOnce"] = true, true, true
		host["CapAdd"] = []string{"CHOWN", "FOWNER", "DAC_OVERRIDE", "FSETID"}
		mount["Target"], mount["ReadOnly"] = volarchive.TargetDir, false
	default:
		return nil, fmt.Errorf("unknown helper kind %q", kind)
	}
	return json.Marshal(spec)
}
