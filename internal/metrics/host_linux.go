//go:build linux

package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type LinuxHost struct {
	total, idle uint64
	initialized bool
}

func NewHostReader() *LinuxHost { return &LinuxHost{} }
func ptr(v uint64) *uint64      { return &v }
func (h *LinuxHost) Read(ctx context.Context) Host {
	var out Host
	read := func(path string, fn func([]string) error) {
		b, e := os.ReadFile(path)
		if e == nil {
			e = fn(strings.Split(string(b), "\n"))
		}
		if e != nil {
			out.Errors = append(out.Errors, path+": "+e.Error())
		}
	}
	read("/proc/stat", func(lines []string) error {
		if len(lines) == 0 {
			return fmt.Errorf("missing cpu")
		}
		f := strings.Fields(lines[0])
		if len(f) < 5 || f[0] != "cpu" {
			return fmt.Errorf("missing cpu")
		}
		var total, idle uint64
		for i, v := range f[1:] {
			n, e := strconv.ParseUint(v, 10, 64)
			if e != nil {
				return e
			}
			if i < 8 {
				total += n
			}
			if i == 3 || i == 4 {
				idle += n
			}
		}
		if h.initialized && total > h.total && idle >= h.idle && idle-h.idle <= total-h.total {
			v := 100 * (1 - float64(idle-h.idle)/float64(total-h.total))
			out.CPUPercent = &v
		}
		h.total, h.idle, h.initialized = total, idle, true
		return nil
	})
	read("/proc/meminfo", func(lines []string) error {
		m := map[string]uint64{}
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, e := strconv.ParseUint(f[1], 10, 64)
				if e == nil {
					m[strings.TrimSuffix(f[0], ":")] = v * 1024
				}
			}
		}
		for key, dest := range map[string]**uint64{"MemTotal": &out.MemoryTotal, "MemAvailable": &out.MemoryAvailable, "SwapTotal": &out.SwapTotal, "SwapFree": &out.SwapFree} {
			if v, ok := m[key]; ok {
				*dest = ptr(v)
			} else {
				out.Errors = append(out.Errors, "/proc/meminfo: missing "+key)
			}
		}
		return nil
	})
	read("/proc/loadavg", func(lines []string) error {
		f := strings.Fields(lines[0])
		if len(f) < 3 {
			return fmt.Errorf("missing load")
		}
		for _, x := range f[:3] {
			v, e := strconv.ParseFloat(x, 64)
			if e != nil {
				return e
			}
			out.Load = append(out.Load, v)
		}
		return nil
	})
	read("/proc/diskstats", func(lines []string) error {
		rd, wr, err := sumDiskstats(lines, isPartitionDevice)
		if err != nil {
			return err
		}
		out.DiskReadBytes = ptr(rd)
		out.DiskWriteBytes = ptr(wr)
		return nil
	})
	read("/proc/net/dev", func(lines []string) error {
		rx, tx, err := sumNetDevCounters(lines, physicalInterface)
		if err != nil {
			return err
		}
		out.NetworkRXBytes = ptr(rx)
		out.NetworkTXBytes = ptr(tx)
		return nil
	})
	mounts, err := ReadMounts()
	out.Mounts = mounts
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
	}
	return out
}

// sumDiskstats totals sectors read and written (512 bytes each) over whole physical
// disks in /proc/diskstats lines. Virtual devices are skipped because they re-report I/O
// already counted on their backing disks, and partitions are skipped because they
// duplicate their parent's counters.
func sumDiskstats(lines []string, isPartition func(name string) bool) (rd, wr uint64, err error) {
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if skipDiskName(name) || isPartition(name) {
			continue
		}
		a, e := strconv.ParseUint(f[5], 10, 64)
		if e != nil {
			return 0, 0, e
		}
		b, e := strconv.ParseUint(f[9], 10, 64)
		if e != nil {
			return 0, 0, e
		}
		rd += a * 512
		wr += b * 512
	}
	return rd, wr, nil
}

var virtualDiskPrefixes = []string{"dm-", "md", "loop", "ram", "zram", "nbd", "sr", "fd"}

func skipDiskName(name string) bool {
	for _, p := range virtualDiskPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isPartitionDevice reports whether a /proc/diskstats device is a partition according to sysfs.
func isPartitionDevice(name string) bool {
	_, err := os.Stat(sysBlockPartitionPath(name))
	return err == nil
}

// sysBlockPartitionPath maps a /proc/diskstats name to its sysfs partition marker. The
// kernel writes '/' as '!' in sysfs names, so cciss/c0d0 becomes cciss!c0d0.
func sysBlockPartitionPath(name string) string {
	return "/sys/class/block/" + strings.ReplaceAll(name, "/", "!") + "/partition"
}

// sumNetDev totals received and transmitted bytes over the interfaces for which physical
// reports true, and returns how many interfaces it counted. lines are the lines of
// /proc/net/dev; header lines have no "name:" prefix and are skipped.
func sumNetDev(lines []string, physical func(iface string) bool) (rx, tx uint64, included int, err error) {
	for _, line := range lines {
		name, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) < 16 {
			return 0, 0, 0, fmt.Errorf("invalid interface counters")
		}
		if !physical(strings.TrimSpace(name)) {
			continue
		}
		a, e := strconv.ParseUint(f[0], 10, 64)
		if e != nil {
			return 0, 0, 0, e
		}
		b, e := strconv.ParseUint(f[8], 10, 64)
		if e != nil {
			return 0, 0, 0, e
		}
		rx += a
		tx += b
		included++
	}
	return rx, tx, included, nil
}

// sumNetDevCounters counts the interfaces that physical accepts. Containers and some VMs
// expose NICs with no sysfs device link, so when that accepts none, it counts by name.
func sumNetDevCounters(lines []string, physical func(iface string) bool) (rx, tx uint64, err error) {
	rx, tx, n, err := sumNetDev(lines, physical)
	if err != nil || n > 0 {
		return rx, tx, err
	}
	rx, tx, _, err = sumNetDev(lines, func(iface string) bool { return !virtualInterfaceName(iface) })
	return rx, tx, err
}

// physicalInterface reports whether a network interface is backed by hardware, which shows as
// a device link in sysfs. Hosts without that link are handled by the name-rule fallback in
// sumNetDevCounters, not here.
func physicalInterface(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name + "/device")
	return err == nil
}

var virtualInterfacePrefixes = []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "vxlan", "tun", "tap", "wg"}

func virtualInterfaceName(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// mountEntry is the part of a /proc/self/mountinfo line that the filters need.
type mountEntry struct{ Path, FSType string }

var mountPathEscapes = strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\")

// parseMountinfo reads /proc/self/mountinfo. The mount point is field 4. The filesystem
// type is the field after the "-" separator, which is searched for from field 6 because
// the number of optional fields varies. Malformed lines are skipped. Repeated paths are
// all returned, in mountinfo order, for selectMounts to resolve.
func parseMountinfo(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	scan := bufio.NewScanner(r)
	for scan.Scan() {
		f := strings.Fields(scan.Text())
		if len(f) < 5 {
			continue
		}
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(f) {
			continue
		}
		out = append(out, mountEntry{Path: mountPathEscapes.Replace(f[4]), FSType: f[sep+1]})
	}
	return out, scan.Err()
}

// networkFSTypes can block indefinitely when their server is unreachable. They are skipped
// everywhere, including the root mount.
var networkFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
	"sshfs": true, "9p": true, "ceph": true, "glusterfs": true, "fuse": true,
}

func networkFSType(fstype string) bool {
	return networkFSTypes[fstype] || strings.HasPrefix(fstype, "fuse.")
}

// skipFSTypes are pseudo and virtual filesystems. Their statfs results are meaningless for
// disk usage.
var skipFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "devtmpfs": true,
	"mqueue": true, "debugfs": true, "tracefs": true, "securityfs": true, "pstore": true, "bpf": true,
	"configfs": true, "fusectl": true, "hugetlbfs": true, "autofs": true, "binfmt_misc": true,
	"rpc_pipefs": true, "nsfs": true, "overlay": true, "squashfs": true, "tmpfs": true, "ramfs": true,
	"efivarfs": true,
}

var skipMountPrefixes = []string{"/var/lib/docker/", "/run/docker/", "/proc/", "/sys/", "/dev/"}

var skipMountExact = map[string]bool{"/proc": true, "/sys": true, "/dev": true}

// skipMount reports whether a mount should be left out of the disk list. The root mount is
// kept whatever its type, because a container or live host often reports "/" as overlay or
// tmpfs, and the root disk check depends on it. Only network types are skipped there.
func skipMount(m mountEntry) bool {
	if m.Path == "/" {
		return networkFSType(m.FSType)
	}
	if networkFSType(m.FSType) || skipFSTypes[m.FSType] {
		return true
	}
	if skipMountExact[m.Path] {
		return true
	}
	for _, p := range skipMountPrefixes {
		if strings.HasPrefix(m.Path, p) {
			return true
		}
	}
	return false
}

// selectMounts keeps, for each path, only the last entry in mountinfo order, since later
// entries are mounted on top and are what statfs reports. If that top mount is a skipped
// type, the path is dropped entirely.
func selectMounts(entries []mountEntry) []mountEntry {
	last := make(map[string]int, len(entries))
	for i, m := range entries {
		last[m.Path] = i
	}
	var out []mountEntry
	for i, m := range entries {
		if last[m.Path] == i && !skipMount(m) {
			out = append(out, m)
		}
	}
	return out
}

const mountStatfsDeadline = 2 * time.Second

// statfsFunc is the statfs call used by statfsTimeout. Tests replace it.
var statfsFunc = syscall.Statfs

// statfsCall is one statfs in progress. Concurrent callers for the same path wait on it
// instead of starting another, so a hung mount holds at most one goroutine.
type statfsCall struct {
	done chan struct{}
	st   syscall.Statfs_t
	err  error
}

var (
	statfsMu    sync.Mutex
	statfsCalls = map[string]*statfsCall{}
)

// statfsTimeout runs statfs in a goroutine so a hung mount cannot block the caller. A caller
// for a path that already has a statfs in progress waits on that call rather than starting
// another, so a hung mount holds at most one goroutine in total. A caller that reaches its
// deadline returns "timeout" and leaves the call running for later callers to share.
func statfsTimeout(path string, d time.Duration) (syscall.Statfs_t, error) {
	statfsMu.Lock()
	c, ok := statfsCalls[path]
	if !ok {
		c = &statfsCall{done: make(chan struct{})}
		statfsCalls[path] = c
		fn := statfsFunc
		go func() {
			c.err = fn(path, &c.st)
			statfsMu.Lock()
			delete(statfsCalls, path)
			statfsMu.Unlock()
			close(c.done)
		}()
	}
	statfsMu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.done:
		return c.st, c.err
	case <-timer.C:
		return syscall.Statfs_t{}, fmt.Errorf("timeout")
	}
}

// statMounts runs statfs on every entry concurrently, so the whole call takes at most about one deadline.
func statMounts(entries []mountEntry, d time.Duration) (mounts []Mount, failures []string) {
	results := make([]struct {
		m   Mount
		err error
	}, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := statfsTimeout(e.Path, d)
			results[i].err = err
			results[i].m = Mount{e.Path, st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)}
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			failures = append(failures, "mount "+entries[i].Path+": "+r.err.Error())
			continue
		}
		mounts = append(mounts, r.m)
	}
	return mounts, failures
}

func ReadMounts() ([]Mount, error) {
	f, e := os.Open("/proc/self/mountinfo")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	entries, scanErr := parseMountinfo(f)
	mounts, failures := statMounts(selectMounts(entries), mountStatfsDeadline)
	if scanErr != nil {
		failures = append(failures, scanErr.Error())
	}
	if len(failures) > 0 {
		return mounts, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return mounts, nil
}
