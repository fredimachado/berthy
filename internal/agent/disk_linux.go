//go:build linux

package agent

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

func hostDisk() (any, error) {
	f, e := os.Open("/proc/self/mountinfo")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	type mount struct {
		Path      string `json:"path"`
		Total     uint64 `json:"total"`
		Available uint64 `json:"available"`
	}
	out := []mount{}
	seen := map[string]bool{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Fields(s.Text())
		if len(p) < 5 {
			continue
		}
		path := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\").Replace(p[4])
		if seen[path] {
			continue
		}
		seen[path] = true
		var st syscall.Statfs_t
		if syscall.Statfs(path, &st) == nil {
			out = append(out, mount{path, st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)})
		}
	}
	return out, s.Err()
}
