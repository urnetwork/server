package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// One storage host's exporter answer: its MinIO drives at /srv/minio/d<n> on nvme<n>n1, its boot media on
// nvme9n1 at /, each drive having written a thousand bytes times its number and plus offset.
func fileClusterExporterText(driveCount int, offset uint64) string {
	text := "# HELP node_disk_written_bytes_total The total number of bytes written successfully.\n" +
		"# TYPE node_disk_written_bytes_total counter\n" +
		"node_disk_written_bytes_total{device=\"nvme9n1\"} 5e+09\n" +
		"node_disk_writes_completed_total{device=\"nvme9n1\"} 77\n" +
		"node_disk_reads_completed_total{device=\"nvme9n1\"} 88\n" +
		"node_filesystem_size_bytes{device=\"/dev/nvme9n1p2\",fstype=\"ext4\",mountpoint=\"/\"} 1e+11\n" +
		"node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} 12.5\n"
	for i := range driveCount {
		device := fmt.Sprintf("nvme%dn1", i)
		text += fmt.Sprintf("node_disk_written_bytes_total{device=%q} %d\n", device, uint64(i+1)*1000+offset)
		text += fmt.Sprintf("node_disk_writes_completed_total{device=%q} %d\n", device, uint64(i+1)*10+offset)
		text += fmt.Sprintf("node_disk_reads_completed_total{device=%q} %d\n", device, uint64(i+1)*20+offset)
		text += fmt.Sprintf("node_filesystem_size_bytes{device=\"/dev/%s\",fstype=\"xfs\",mountpoint=\"/srv/minio/d%d\"} 1.92e+12\n", device, i)
	}
	return text
}

// Admin info for hosts of two drives each, the drive at index healing under heal.
func fileClusterCountersAdminInfo(hostNames []string, healing int) *FileClusterAdminInfo {
	info := &FileClusterAdminInfo{ReadTime: time.Unix(0, 0), PoolCount: 1, RawUsageByteCount: 10, RawCapacityByteCount: 100}
	disk := 0
	for _, hostName := range hostNames {
		for i := range 2 {
			info.Drives = append(info.Drives, &FileClusterDriveInfo{
				Endpoint:     fmt.Sprintf("http://%s:9000/srv/minio/d%d", hostName, i),
				Path:         fmt.Sprintf("/srv/minio/d%d", i),
				HostEndpoint: hostName + ":9000",
				Healing:      disk == healing,
				DiskIndex:    disk,
			})
			disk += 1
		}
	}
	return info
}

// Counters over exporters by host name, each a test server.
func newTestFileClusterCounters(t *testing.T, info *FileClusterAdminInfo, hostTexts map[string]string, failingHosts ...string) *FileClusterCounters {
	t.Helper()
	hostUrls := map[string]string{}
	for hostName, text := range hostTexts {
		failing := false
		for _, failingHost := range failingHosts {
			failing = failing || failingHost == hostName
		}
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if failing || request.URL.Path != "/metrics" {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(writer, text)
		}))
		t.Cleanup(server.Close)
		hostUrls[hostName] = server.URL + "/metrics"
	}
	return newFileClusterCounters(DefaultFileClusterCountersSettings(), &http.Client{}, func(clusterId string) *FileClusterAdminInfo {
		if clusterId != "meta-1" {
			return nil
		}
		return info
	}, func(hostName string) string {
		return hostUrls[hostName]
	})
}

// Each drive is found by its mountpoint's device, with its three counters, its heal flag and a host-free id.
func TestFileClusterCountersMapDrivesByMountpoint(t *testing.T) {
	hostNames := []string{"store-a.example", "store-b.example"}
	counters := newTestFileClusterCounters(t, fileClusterCountersAdminInfo(hostNames, 3), map[string]string{
		"store-a.example": fileClusterExporterText(2, 0),
		"store-b.example": fileClusterExporterText(2, 500),
	})
	reading, err := counters.ReadDriveCounters(t.Context(), "meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(reading.Drives) != 4 || reading.RawUsageByteCount != 10 || reading.RawCapacityByteCount != 100 {
		t.Fatalf("reading %+v", reading)
	}
	for i, drive := range reading.Drives {
		offset := uint64(0)
		if 2 <= i {
			offset = 500
		}
		local := uint64(i%2 + 1)
		if drive.Missing || drive.DriveId != fmt.Sprintf("0-0-%d", i) || drive.Healing != (i == 3) {
			t.Errorf("drive %d: %+v", i, drive)
		}
		if drive.WrittenByteCount != local*1000+offset || drive.WritesCompleted != local*10+offset || drive.ReadsCompleted != local*20+offset {
			t.Errorf("drive %d counters: %+v", i, drive)
		}
	}
}

// A host whose exporter does not answer reads its drives missing, and the others' stand.
func TestFileClusterCountersHostDownReadsMissing(t *testing.T) {
	hostNames := []string{"store-a.example", "store-b.example"}
	counters := newTestFileClusterCounters(t, fileClusterCountersAdminInfo(hostNames, -1), map[string]string{
		"store-a.example": fileClusterExporterText(2, 0),
		"store-b.example": fileClusterExporterText(2, 0),
	}, "store-b.example")
	reading, err := counters.ReadDriveCounters(t.Context(), "meta-1")
	if err != nil {
		t.Fatal(err)
	}
	for i, drive := range reading.Drives {
		if drive.Missing != (2 <= i) {
			t.Errorf("drive %d missing %v", i, drive.Missing)
		}
	}
}

// A drive whose mountpoint the answer lacks, or whose filesystem is the root's, reads missing.
func TestFileClusterCountersUnmatchedDriveReadsMissing(t *testing.T) {
	info := fileClusterCountersAdminInfo([]string{"store-a.example"}, -1)
	info.Drives[1].Path = "/srv/elsewhere/d1"
	info.Drives[1].Endpoint = "http://store-a.example:9000/srv/elsewhere/d1"
	counters := newTestFileClusterCounters(t, info, map[string]string{"store-a.example": fileClusterExporterText(2, 0)})
	reading, err := counters.ReadDriveCounters(t.Context(), "meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if reading.Drives[0].Missing || !reading.Drives[1].Missing {
		t.Fatalf("missing %v %v", reading.Drives[0].Missing, reading.Drives[1].Missing)
	}
}

// A filesystem on a partition maps to its disk's diskstats series, and a drive's endpoint gives its path when
// admin info names none.
func TestFileClusterCountersPartitionMapsToItsDisk(t *testing.T) {
	admin := fileClusterCountersAdminInfo([]string{"store-a.example"}, -1)
	admin.Drives[0].Path = ""
	text := "node_disk_written_bytes_total{device=\"sdb\"} 4096\n" +
		"node_disk_writes_completed_total{device=\"sdb\"} 8\n" +
		"node_disk_reads_completed_total{device=\"sdb\"} 9\n" +
		"node_filesystem_size_bytes{device=\"/dev/sdb1\",fstype=\"xfs\",mountpoint=\"/srv/minio/d0\"} 1.6e+13\n"
	counters := newTestFileClusterCounters(t, admin, map[string]string{"store-a.example": text})
	reading, err := counters.ReadDriveCounters(t.Context(), "meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if drive := reading.Drives[0]; drive.Missing || drive.WrittenByteCount != 4096 || drive.ReadsCompleted != 9 {
		t.Fatalf("drive %+v", drive)
	}
}

// No admin info, no drive list: an error, which the loop reads as every drive missing.
func TestFileClusterCountersUnreadAdminInfoIsAnError(t *testing.T) {
	counters := newTestFileClusterCounters(t, nil, map[string]string{})
	if _, err := counters.ReadDriveCounters(t.Context(), "meta-1"); !errors.Is(err, ErrFileClusterCountersUnread) {
		t.Fatalf("err %v", err)
	}
}

// The parser takes escaped label values, timestamps and unsorted labels, and skips what it cannot read.
func TestFileClusterCountersParseTheTextFormat(t *testing.T) {
	series, err := parseFileClusterHostSeries("node_disk_written_bytes_total{foo=\"a\\\"b\",device=\"nvme0n1\"} 1.5e+03 1700000000000\n" +
		"node_disk_written_bytes_total{device=\"nvme1n1\"} -4\n" +
		"node_disk_written_bytes_total{device=\"nvme2n1\"} NaN\n" +
		"node_disk_written_bytes_total{device=\"nvme3n1\" 4\n" +
		"node_filesystem_size_bytes{mountpoint=\"/srv/minio/d0/\",device=\"/dev/nvme0n1\"} 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(series.writtenByteCounts) != 1 || series.writtenByteCounts["nvme0n1"] != 1500 {
		t.Fatalf("written %v", series.writtenByteCounts)
	}
	if series.mountpointDevices["/srv/minio/d0"] != "/dev/nvme0n1" {
		t.Fatalf("mountpoints %v", series.mountpointDevices)
	}
}
