package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A host's files under a test directory: two XFS drives' mounts and stats, and a third path no filesystem holds.
func writeHostStatsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path string, content string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/self/mountinfo", strings.Join([]string{
		"22 1 259:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw",
		"40 22 259:2 / /srv/file-cluster/meta-1/d1 rw,noatime shared:20 - xfs /dev/nvme0n1 rw,logbsize=256k",
		"41 22 259:3 / /srv/file-cluster/meta-1/d\\0402 rw,noatime shared:21 - xfs /dev/nvme1n1 rw,logbsize=256k",
		"42 22 0:5 / /proc rw - proc proc rw",
	}, "\n")+"\n")
	write("sys/fs/xfs/nvme0n1/stats/stats", "extent_alloc 1 2 3 4\nlog 100 2000 0 5 6\nxpc 111 222 333\n")
	write("sys/fs/xfs/nvme1n1/stats/stats", "log 7 8 0 0 0\nxpc 9 10 11\n")
	return root
}

func TestHostStatsReadsEachDrivesXfsAndSmart(t *testing.T) {
	root := writeHostStatsRoot(t)
	source := &hostStatsSource{root: root, runSmart: func(ctx context.Context, device string) ([]byte, error) {
		switch device {
		case "nvme0n1":
			return []byte(`{"critical_warning": 0, "data_units_written": 12345, "percent_used": 2, "avail_spare": 100, "media_errors": 0}`), nil
		case "nvme1n1":
			// a newer nvme-cli's names
			return []byte(`{"data_units_written": 777, "percentage_used": 3, "available_spare": 99, "media_errors": 1}`), nil
		}
		return nil, errors.New("no such device")
	}}
	answer := source.read(context.Background(), []string{"/srv/file-cluster/meta-1/d1", "/srv/file-cluster/meta-1/d 2", "/srv/elsewhere"})
	if len(answer.Drives) != 3 {
		t.Fatalf("%d drives", len(answer.Drives))
	}
	first := answer.Drives[0]
	if first.Device != "nvme0n1" || *first.XfsLogByteCount != 2000*512 || *first.XfsFileByteCount != 222 || first.Smart.DataUnitsWritten != 12345 || first.Smart.PercentageUsed != 2 || 0 < len(first.Errors) {
		t.Fatalf("first drive %+v", first)
	}
	second := answer.Drives[1]
	if second.Device != "nvme1n1" || *second.XfsLogByteCount != 8*512 || second.Smart.AvailableSpare != 99 || second.Smart.MediaErrors != 1 {
		t.Fatalf("second drive %+v %+v", second, second.Smart)
	}
	// the root filesystem holds the third path, and its device has no XFS stats or SMART log: reasons, no guess
	third := answer.Drives[2]
	if third.XfsLogByteCount != nil || third.Smart != nil || len(third.Errors) != 2 {
		t.Fatalf("third drive %+v", third)
	}
}

func TestHostStatsSmartLackingAFieldReadsNothing(t *testing.T) {
	source := &hostStatsSource{root: writeHostStatsRoot(t), runSmart: func(ctx context.Context, device string) ([]byte, error) {
		return []byte(`{"data_units_written": 1, "percent_used": 2}`), nil
	}}
	answer := source.read(context.Background(), []string{"/srv/file-cluster/meta-1/d1"})
	if answer.Drives[0].Smart != nil || len(answer.Drives[0].Errors) != 1 {
		t.Fatalf("read %+v", answer.Drives[0])
	}
}

func TestCommandHostStatsMapsEachHostsPathsToDrives(t *testing.T) {
	info := &server.FileClusterAdminInfo{Drives: []*server.FileClusterDriveInfo{
		{HostEndpoint: "http://store-1.example:9000", Path: "/srv/file-cluster/meta-1/d1", SetIndex: 0, DiskIndex: 0},
		{HostEndpoint: "http://store-1.example:9000", Path: "/srv/file-cluster/meta-1/it's", SetIndex: 0, DiskIndex: 1},
		{HostEndpoint: "http://store-2.example:9000", Path: "/srv/file-cluster/meta-1/d1", SetIndex: 0, DiskIndex: 2},
		// a host name the shell must never see
		{HostEndpoint: "http://bad;host:9000", Path: "/srv/x", SetIndex: 0, DiskIndex: 3},
	}}
	commands := []string{}
	reader := newCommandHostStats("ssh {host} filecluster-bench hoststats {paths}", func(clusterId string) *server.FileClusterAdminInfo { return info },
		func(ctx context.Context, command string) ([]byte, error) {
			commands = append(commands, command)
			log := uint64(1000)
			file := uint64(2000)
			answer := &HostStatsAnswer{}
			for _, path := range []string{"/srv/file-cluster/meta-1/d1", "/srv/file-cluster/meta-1/it's"} {
				answer.Drives = append(answer.Drives, &HostStatsDrive{Path: path, Device: "nvme0n1", XfsLogByteCount: &log, XfsFileByteCount: &file, Smart: &DriveSmart{DataUnitsWritten: 5}})
			}
			return json.Marshal(answer)
		})
	stats, err := reader.ReadHostStats(context.Background(), "meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 {
		t.Fatalf("ran %v", commands)
	}
	if commands[0] != `ssh store-1.example filecluster-bench hoststats '/srv/file-cluster/meta-1/d1' '/srv/file-cluster/meta-1/it'\''s'` {
		t.Fatalf("command %q", commands[0])
	}
	if len(stats) != 3 || *stats["0-0-0"].XfsLogByteCount != 1000 || stats["0-0-1"].Smart.DataUnitsWritten != 5 || stats["0-0-2"] == nil || stats["0-0-3"] != nil {
		t.Fatalf("stats %v", stats)
	}
}
