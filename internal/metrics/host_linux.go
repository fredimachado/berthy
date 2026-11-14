//go:build linux

package metrics

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
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
		var rd, wr uint64
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) < 14 {
				continue
			} // Count whole devices only; partitions duplicate their parent counters.
			if _, e := os.Stat("/sys/class/block/" + f[2] + "/partition"); e == nil {
				continue
			}
			a, e := strconv.ParseUint(f[5], 10, 64)
			if e != nil {
				return e
			}
			b, e := strconv.ParseUint(f[9], 10, 64)
			if e != nil {
				return e
			}
			rd += a * 512
			wr += b * 512
		}
		out.DiskReadBytes = ptr(rd)
		out.DiskWriteBytes = ptr(wr)
		return nil
	})
	read("/proc/net/dev", func(lines []string) error {
		var rx, tx uint64
		for _, line := range lines {
			_, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			f := strings.Fields(v)
			if len(f) < 16 {
				return fmt.Errorf("invalid interface counters")
			}
			a, e := strconv.ParseUint(f[0], 10, 64)
			if e != nil {
				return e
			}
			b, e := strconv.ParseUint(f[8], 10, 64)
			if e != nil {
				return e
			}
			rx += a
			tx += b
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
func ReadMounts() ([]Mount, error) {
	f, e := os.Open("/proc/self/mountinfo")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var mounts []Mount
	var failures []string
	scan := bufio.NewScanner(f)
	seen := map[string]bool{}
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 5 {
			continue
		}
		path := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\").Replace(fields[4])
		if seen[path] {
			continue
		}
		seen[path] = true
		var st syscall.Statfs_t
		if e := syscall.Statfs(path, &st); e != nil {
			failures = append(failures, "mount "+path+": "+e.Error())
			continue
		}
		mounts = append(mounts, Mount{path, st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)})
	}
	if e := scan.Err(); e != nil {
		failures = append(failures, e.Error())
	}
	if len(failures) > 0 {
		return mounts, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return mounts, nil
}
