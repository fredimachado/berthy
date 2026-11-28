//go:build linux

package metrics

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestLinuxHostReportsAvailableValues(t *testing.T) {
	h := NewHostReader()
	s := h.Read(context.Background())
	if s.MemoryTotal == nil || *s.MemoryTotal == 0 || len(s.Load) != 3 || s.NetworkRXBytes == nil || len(s.Mounts) == 0 {
		t.Fatalf("incomplete host: %+v", s)
	}
}

func TestParseMountinfoAndSkip(t *testing.T) {
	const info = `22 28 0:21 / /sys rw,nosuid,nodev shared:7 - sysfs sysfs rw
29 28 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro
30 29 0:45 / /var/lib/docker/overlay2/x/merged rw,relatime - overlay overlay rw
31 29 8:17 / /mnt/my\040disk rw,relatime - ext4 /dev/sdb1 rw
32 29 0:50 / /mnt/nas rw,relatime shared:9 master:3 - nfs4 server:/export rw
33 29 0:51 / /mnt/ssh rw,nosuid - fuse.sshfs user@host:/ rw
34 29 0:60 / /data rw,nosuid,nodev - tmpfs tmpfs rw
35 29 8:33 / /data rw,relatime shared:4 - ext4 /dev/sdc1 rw
36 29 8:34 / /mnt/hidden rw,relatime - ext4 /dev/sdd1 rw
37 29 0:61 / /mnt/hidden rw,nosuid,nodev - tmpfs tmpfs rw
38 29 0:62 / /mnt/nosep rw
this line is malformed
`
	entries, err := parseMountinfo(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	// Every well-formed line is returned in order, including repeated paths.
	want := []mountEntry{
		{"/sys", "sysfs"},
		{"/", "ext4"},
		{"/var/lib/docker/overlay2/x/merged", "overlay"},
		{"/mnt/my disk", "ext4"},
		{"/mnt/nas", "nfs4"},
		{"/mnt/ssh", "fuse.sshfs"},
		{"/data", "tmpfs"},
		{"/data", "ext4"},
		{"/mnt/hidden", "ext4"},
		{"/mnt/hidden", "tmpfs"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("parseMountinfo:\n got %+v\nwant %+v", entries, want)
	}
	// /data: tmpfs is mounted first and the ext4 mount on top is what statfs reports, so ext4 is kept.
	// /mnt/hidden: the tmpfs mount is on top and is skipped, which hides the path entirely.
	wantSelected := []mountEntry{
		{"/", "ext4"},
		{"/mnt/my disk", "ext4"},
		{"/data", "ext4"},
	}
	if got := selectMounts(entries); !reflect.DeepEqual(got, wantSelected) {
		t.Fatalf("selectMounts:\n got %+v\nwant %+v", got, wantSelected)
	}
}

func TestSkipMountRules(t *testing.T) {
	for _, tc := range []struct {
		m    mountEntry
		skip bool
	}{
		{mountEntry{"/", "ext4"}, false},
		{mountEntry{"/", "overlay"}, false},
		{mountEntry{"/", "tmpfs"}, false},
		{mountEntry{"/", "nfs4"}, true},
		{mountEntry{"/", "fuse.sshfs"}, true},
		{mountEntry{"/data", "xfs"}, false},
		{mountEntry{"/mnt/ntfs", "fuseblk"}, false},
		{mountEntry{"/mnt/nas", "nfs4"}, true},
		{mountEntry{"/var/lib/docker/volumes/db", "ext4"}, true},
		{mountEntry{"/var/lib/dockerish", "ext4"}, false},
		{mountEntry{"/run/docker/netns/abc", "ext4"}, true},
		{mountEntry{"/proc", "ext4"}, true},
		{mountEntry{"/sys", "ext4"}, true},
		{mountEntry{"/dev", "ext4"}, true},
		{mountEntry{"/mnt/fuse", "fuse"}, true},
		{mountEntry{"/mnt/smb", "smb3"}, true},
		{mountEntry{"/mnt/ceph", "ceph"}, true},
		{mountEntry{"/snap/core/1", "squashfs"}, true},
	} {
		if got := skipMount(tc.m); got != tc.skip {
			t.Errorf("skipMount(%+v) = %v, want %v", tc.m, got, tc.skip)
		}
	}
}

func TestStatfsTimeoutReturnsSuccessfulResult(t *testing.T) {
	st, err := statfsTimeout("/", mountStatfsDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bsize == 0 {
		t.Fatalf("statfs(/) returned zero block size: %+v", st)
	}
}

func TestStatfsTimeoutSharesInFlightCall(t *testing.T) {
	const path = "/berthy-test/shared"
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	orig := statfsFunc
	t.Cleanup(func() { statfsFunc = orig })
	t.Cleanup(unblock)
	var calls atomic.Int32
	statfsFunc = func(_ string, st *syscall.Statfs_t) error {
		calls.Add(1)
		<-release
		st.Bsize = 4096
		return nil
	}

	type result struct {
		st  syscall.Statfs_t
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			st, err := statfsTimeout(path, 5*time.Second)
			results <- result{st, err}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("statfs never started; calls = %d", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the second caller join the call
	unblock()
	for range 2 {
		r := <-results
		if r.err != nil || r.st.Bsize != 4096 {
			t.Errorf("caller got st.Bsize=%d err=%v, want 4096 and nil", r.st.Bsize, r.err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("statfs called %d times, want 1", got)
	}
}

func TestStatfsTimeoutHungPathStartsOneCall(t *testing.T) {
	const path = "/berthy-test/hung-mount"
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	orig := statfsFunc
	t.Cleanup(func() { statfsFunc = orig })
	t.Cleanup(unblock)
	var calls atomic.Int32
	statfsFunc = func(string, *syscall.Statfs_t) error {
		calls.Add(1)
		<-release
		return nil
	}

	for i := range 2 {
		if _, err := statfsTimeout(path, 50*time.Millisecond); err == nil || err.Error() != "timeout" {
			t.Fatalf("call %d: err = %v, want timeout", i+1, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("statfs called %d times while hung, want 1", got)
	}

	unblock()
	// Wait for the hung call to leave the in-flight table first. Otherwise the next caller
	// could join the old call, which returns nil without starting a new statfs.
	deadline := time.Now().Add(2 * time.Second)
	for {
		statfsMu.Lock()
		_, pending := statfsCalls[path]
		statfsMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hung statfs still pending after release")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := statfsTimeout(path, time.Second); err != nil {
		t.Fatalf("after release: err = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("statfs called %d times after release, want 2", got)
	}
}

func TestStatMountsReportsInodeCounts(t *testing.T) {
	orig := statfsFunc
	t.Cleanup(func() { statfsFunc = orig })
	statfsFunc = func(path string, st *syscall.Statfs_t) error {
		st.Bsize, st.Blocks, st.Bavail = 4096, 10, 4
		if path == "/counted" {
			st.Files, st.Ffree = 123, 7
		}
		return nil
	}
	synctest.Test(t, func(t *testing.T) {
		mounts, failures := statMounts([]mountEntry{{"/counted", "ext4"}, {"/zero", "ext4"}}, time.Second)
		if len(failures) != 0 || len(mounts) != 2 {
			t.Fatalf("mounts = %+v, failures = %v", mounts, failures)
		}
		for i, want := range []string{
			`{"path":"/counted","total":40960,"available":16384,"inodes_total":123,"inodes_free":7}`,
			`{"path":"/zero","total":40960,"available":16384,"inodes_total":0,"inodes_free":0}`,
		} {
			got, err := json.Marshal(mounts[i])
			if err != nil || string(got) != want {
				t.Errorf("mount JSON = %s, err = %v; want %s", got, err, want)
			}
		}
	})
}

func TestStatMountsRunsConcurrently(t *testing.T) {
	orig := statfsFunc
	t.Cleanup(func() { statfsFunc = orig })
	statfsFunc = func(path string, st *syscall.Statfs_t) error {
		if path == "/bad" {
			return syscall.EIO
		}
		time.Sleep(200 * time.Millisecond)
		st.Bsize = 4096
		st.Blocks = 10
		st.Bavail = 4
		return nil
	}
	entries := []mountEntry{{"/a", "ext4"}, {"/bad", "ext4"}, {"/b", "ext4"}, {"/c", "ext4"}}

	start := time.Now()
	mounts, failures := statMounts(entries, time.Second)
	// Run serially, the three 200ms calls would take at least 600ms.
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("statMounts took %v; statfs calls ran serially", elapsed)
	}
	wantMounts := []Mount{
		{Path: "/a", Total: 10 * 4096, Available: 4 * 4096},
		{Path: "/b", Total: 10 * 4096, Available: 4 * 4096},
		{Path: "/c", Total: 10 * 4096, Available: 4 * 4096},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Fatalf("mounts:\n got %+v\nwant %+v", mounts, wantMounts)
	}
	wantFailures := []string{"mount /bad: " + syscall.EIO.Error()}
	if !reflect.DeepEqual(failures, wantFailures) {
		t.Fatalf("failures = %q, want %q", failures, wantFailures)
	}
}

// netDevSample has lo, eth0, docker0 and veth123. Each data line has 16 counters.
const netDevSample = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 100 1 0 0 0 0 0 0 100 1 0 0 0 0 0 0
  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0
docker0: 500 5 0 0 0 0 0 0 700 7 0 0 0 0 0 0
 veth123: 300 3 0 0 0 0 0 0 400 4 0 0 0 0 0 0
`

func TestSumNetDev(t *testing.T) {
	rx, tx, n, err := sumNetDev(strings.Split(netDevSample, "\n"), func(iface string) bool { return iface == "eth0" })
	if err != nil {
		t.Fatal(err)
	}
	if rx != 1000 || tx != 2000 || n != 1 {
		t.Fatalf("eth0 only: rx=%d tx=%d included=%d, want rx=1000 tx=2000 included=1", rx, tx, n)
	}
}

func TestSumNetDevFallbackWhenNothingPhysical(t *testing.T) {
	// No interface passes the physical check, so the name rules count eth0 and exclude lo, docker0 and veth123.
	rx, tx, err := sumNetDevCounters(strings.Split(netDevSample, "\n"), func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if rx != 1000 || tx != 2000 {
		t.Fatalf("fallback: rx=%d tx=%d, want eth0 only: rx=1000 tx=2000", rx, tx)
	}
}

func TestVirtualInterfaceNameRules(t *testing.T) {
	for _, name := range []string{"lo", "veth123", "docker0", "br-1a2b", "virbr0", "cni0", "flannel.1", "cali1", "vxlan0", "tun0", "tap0", "wg0"} {
		if !virtualInterfaceName(name) {
			t.Errorf("%s should be virtual", name)
		}
	}
	for _, name := range []string{"eth0", "ens3", "wlan0", "enp0s31f6"} {
		if virtualInterfaceName(name) {
			t.Errorf("%s should be physical", name)
		}
	}
}

func TestSumDiskstats(t *testing.T) {
	const stats = `   8       0 sda 100 0 2048 0 50 0 4096 0 0 0 0 0 0 0 0 0 0
   8       1 sda1 10 0 512 0 5 0 1024 0 0 0 0 0 0 0 0 0 0
 253       0 dm-0 100 0 4096 0 50 0 4096 0 0 0 0 0 0 0 0 0 0
   7       0 loop0 1 0 8 0 0 0 0 0 0 0 0 0 0 0 0 0 0
 259       0 nvme0n1 10 0 100 0 5 0 200 0 0 0 0 0 0 0 0 0 0
 259       1 nvme0n1p1 5 0 50 0 2 0 100 0 0 0 0 0 0 0 0 0 0
 104       0 cciss/c0d0 10 0 10 0 5 0 20 0 0 0 0 0 0 0 0 0 0
`
	partitions := map[string]bool{"sda1": true, "nvme0n1p1": true}
	rd, wr, err := sumDiskstats(strings.Split(stats, "\n"), func(name string) bool { return partitions[name] })
	if err != nil {
		t.Fatal(err)
	}
	// sda (2048 read, 4096 written), nvme0n1 (100, 200) and cciss/c0d0 (10, 20), in 512-byte sectors.
	if rd != (2048+100+10)*512 {
		t.Errorf("read bytes = %d, want %d", rd, (2048+100+10)*512)
	}
	if wr != (4096+200+20)*512 {
		t.Errorf("write bytes = %d, want %d", wr, (4096+200+20)*512)
	}
}

func TestSysBlockPartitionPath(t *testing.T) {
	if got, want := sysBlockPartitionPath("cciss/c0d0p1"), "/sys/class/block/cciss!c0d0p1/partition"; got != want {
		t.Fatalf("sysBlockPartitionPath = %q, want %q", got, want)
	}
}
