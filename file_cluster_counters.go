package server

// §19.6's counters row on the storage hosts (EVSB-M2-07, plan-EVSB-counter-transport): each storage host runs a
// second fluent-bit pipeline whose prometheus_exporter output serves node_exporter's diskstats and filesystem
// series every 5 s on its storage-LAN address, port 2021, reachable only from taskworker hosts. One read of a
// cluster GETs each of its hosts' /metrics, bounded by ReadTimeout, and maps admin info's drives onto the
// series:
//
//   - a drive's path (admin info's path, else its endpoint's) lies under the longest filesystem mountpoint
//     holding it, whose device names the drive's diskstats series, a partition's taken back to its disk;
//   - node_disk_written_bytes_total is field 7 in bytes, node_disk_writes_completed_total field 5 and
//     node_disk_reads_completed_total field 1 of /sys/block/<dev>/stat;
//   - a drive whose host did not answer, or whose device the answer lacks, reads missing, so the budget loop
//     publishes nothing rather than guess.
//
// A drive's id is its pool, set and disk index from admin info: stable across reboots, and naming no host.
// A cluster whose admin info is unread or stale is an error, since then no drive list exists to read.
// taskworker adapts what this returns to evsservice.DriveCounterSource, so server's root links no engine.
// Safe for concurrent use.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The series a read takes.
const (
	fileClusterDiskWrittenBytesSeries    = "node_disk_written_bytes_total"
	fileClusterDiskWritesCompletedSeries = "node_disk_writes_completed_total"
	fileClusterDiskReadsCompletedSeries  = "node_disk_reads_completed_total"
	fileClusterFilesystemSizeSeries      = "node_filesystem_size_bytes"
)

// A partition's name: the disk's name and its partition number, nvme0n1p2 or sdb1.
var fileClusterPartitionPattern = regexp.MustCompile(`^(nvme[0-9]+n[0-9]+)p[0-9]+$|^((?:s|v|xv|h)d[a-z]+)[0-9]+$`)

// A cluster whose admin info is unread or stale: no drive list to read against.
var ErrFileClusterCountersUnread = errors.New("file cluster admin info unread or stale")

type FileClusterCountersSettings struct {
	// The exporter's port on each storage host: 2021 (plan-EVSB-counter-transport).
	Port int
	// How long one host's read may take: 2 s, inside the loop's 5 s interval.
	ReadTimeout time.Duration
	// The most of one answer read, past which the host reads missing: 4 MiB, a host's two families being a
	// few KiB.
	MaxByteCount int64
}

func DefaultFileClusterCountersSettings() *FileClusterCountersSettings {
	return &FileClusterCountersSettings{
		Port:         2021,
		ReadTimeout:  2 * time.Second,
		MaxByteCount: 4 * 1024 * 1024,
	}
}

// One drive's counters as a read found them.
type FileClusterDriveCounters struct {
	// pool-set-disk, from admin info.
	DriveId          string
	WrittenByteCount uint64
	WritesCompleted  uint64
	ReadsCompleted   uint64
	// Admin info shows it under heal.
	Healing bool
	// Its host did not answer, or listed no series for it.
	Missing bool
}

// One read of a cluster: every drive admin info lists, and admin info's use for §19.9's line.
type FileClusterCounterReading struct {
	Drives               []*FileClusterDriveCounters
	RawUsageByteCount    uint64
	RawCapacityByteCount uint64
	UsedInodeCount       uint64
	FreeInodeCount       uint64
}

// See the file header.
type FileClusterCounters struct {
	settings *FileClusterCountersSettings
	client   *http.Client
	// the cluster's newest admin info, nil when unread or stale
	adminInfo func(clusterId string) *FileClusterAdminInfo
	// a host's exporter url
	exporterUrl func(hostName string) string
}

// Reads the counters of clusters' hosts. settings may be nil for the defaults.
func NewFileClusterCounters(clusters *FileClusters, settings *FileClusterCountersSettings) *FileClusterCounters {
	if settings == nil {
		settings = DefaultFileClusterCountersSettings()
	}
	adminInfo := func(clusterId string) *FileClusterAdminInfo {
		cluster, ok := clusters.clusterIdClusters[clusterId]
		if !ok {
			return nil
		}
		return cluster.LastAdminInfo()
	}
	exporterUrl := func(hostName string) string {
		return (&url.URL{Scheme: "http", Host: net.JoinHostPort(hostName, strconv.Itoa(settings.Port)), Path: "/metrics"}).String()
	}
	return newFileClusterCounters(settings, &http.Client{}, adminInfo, exporterUrl)
}

func newFileClusterCounters(
	settings *FileClusterCountersSettings,
	client *http.Client,
	adminInfo func(clusterId string) *FileClusterAdminInfo,
	exporterUrl func(hostName string) string,
) *FileClusterCounters {
	return &FileClusterCounters{
		settings:    settings,
		client:      client,
		adminInfo:   adminInfo,
		exporterUrl: exporterUrl,
	}
}

// One host's answer: each disk's three counters by device, and each mountpoint's device.
type fileClusterHostSeries struct {
	writtenByteCounts map[string]uint64
	writesCompleted   map[string]uint64
	readsCompleted    map[string]uint64
	mountpointDevices map[string]string
}

// Reads every drive of the cluster, each host at once. See the file header.
func (self *FileClusterCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*FileClusterCounterReading, error) {
	info := self.adminInfo(clusterId)
	if info == nil {
		return nil, fmt.Errorf("%w: %s", ErrFileClusterCountersUnread, clusterId)
	}
	hostNames := []string{}
	for _, drive := range info.Drives {
		if hostName := fileClusterHostName(drive); hostName != "" && !slices.Contains(hostNames, hostName) {
			hostNames = append(hostNames, hostName)
		}
	}
	hostSeries := make([]*fileClusterHostSeries, len(hostNames))
	var wait sync.WaitGroup
	for i, hostName := range hostNames {
		wait.Go(func() {
			series, err := self.readHost(ctx, hostName)
			if err == nil {
				hostSeries[i] = series
			}
		})
	}
	wait.Wait()

	reading := &FileClusterCounterReading{
		RawUsageByteCount:    info.RawUsageByteCount,
		RawCapacityByteCount: info.RawCapacityByteCount,
		UsedInodeCount:       info.UsedInodeCount,
		FreeInodeCount:       info.FreeInodeCount,
	}
	for _, drive := range info.Drives {
		counters := &FileClusterDriveCounters{
			DriveId: fmt.Sprintf("%d-%d-%d", drive.PoolIndex, drive.SetIndex, drive.DiskIndex),
			Healing: drive.Healing,
			Missing: true,
		}
		reading.Drives = append(reading.Drives, counters)
		hostIndex := slices.Index(hostNames, fileClusterHostName(drive))
		if hostIndex < 0 || hostSeries[hostIndex] == nil {
			continue
		}
		series := hostSeries[hostIndex]
		device, ok := series.device(fileClusterDrivePath(drive))
		if !ok {
			continue
		}
		writtenByteCount, writtenOk := series.writtenByteCounts[device]
		writesCompleted, writesOk := series.writesCompleted[device]
		readsCompleted, readsOk := series.readsCompleted[device]
		if !writtenOk || !writesOk || !readsOk {
			continue
		}
		counters.WrittenByteCount = writtenByteCount
		counters.WritesCompleted = writesCompleted
		counters.ReadsCompleted = readsCompleted
		counters.Missing = false
	}
	return reading, nil
}

// The host a drive is on: the name in its host's endpoint, else in its own.
func fileClusterHostName(drive *FileClusterDriveInfo) string {
	for _, endpoint := range []string{drive.HostEndpoint, drive.Endpoint} {
		if endpoint == "" {
			continue
		}
		hostPort := endpoint
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
			hostPort = parsed.Host
		}
		if hostName, _, err := net.SplitHostPort(hostPort); err == nil {
			return hostName
		}
		if !strings.ContainsAny(hostPort, "/") {
			return hostPort
		}
	}
	return ""
}

// Where the drive is mounted: admin info's path, else its endpoint's path.
func fileClusterDrivePath(drive *FileClusterDriveInfo) string {
	if drive.Path != "" {
		return path.Clean(drive.Path)
	}
	if parsed, err := url.Parse(drive.Endpoint); err == nil && parsed.Path != "" {
		return path.Clean(parsed.Path)
	}
	return ""
}

// The diskstats device of the longest mountpoint holding drivePath, a partition taken back to its disk when
// the answer has no series for it.
func (self *fileClusterHostSeries) device(drivePath string) (string, bool) {
	if drivePath == "" || !strings.HasPrefix(drivePath, "/") {
		return "", false
	}
	bestMountpoint := ""
	bestDevice := ""
	for mountpoint, device := range self.mountpointDevices {
		holds := mountpoint == "/" || drivePath == mountpoint || strings.HasPrefix(drivePath, mountpoint+"/")
		if holds && len(bestMountpoint) < len(mountpoint) {
			bestMountpoint = mountpoint
			bestDevice = device
		}
	}
	// a drive on the root filesystem is the boot media, which no MinIO drive is
	if bestMountpoint == "" || bestMountpoint == "/" {
		return "", false
	}
	device := strings.TrimPrefix(bestDevice, "/dev/")
	if _, ok := self.writtenByteCounts[device]; ok {
		return device, true
	}
	if match := fileClusterPartitionPattern.FindStringSubmatch(device); match != nil {
		disk := match[1] + match[2]
		if _, ok := self.writtenByteCounts[disk]; ok {
			return disk, true
		}
	}
	return "", false
}

// One host's exporter, within ReadTimeout.
func (self *FileClusterCounters) readHost(ctx context.Context, hostName string) (*fileClusterHostSeries, error) {
	readCtx, cancel := context.WithTimeout(ctx, self.settings.ReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(readCtx, http.MethodGet, self.exporterUrl(hostName), nil)
	if err != nil {
		return nil, err
	}
	response, err := self.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exporter answered %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaxByteCount+1))
	if err != nil {
		return nil, err
	}
	if self.settings.MaxByteCount < int64(len(body)) {
		return nil, fmt.Errorf("exporter answer past %d bytes", self.settings.MaxByteCount)
	}
	return parseFileClusterHostSeries(string(body))
}

// The four series from Prometheus's text format. Lines of other series, comments and samples that do not
// parse are skipped; a counter that is not a whole number at or above zero is too.
func parseFileClusterHostSeries(text string) (*fileClusterHostSeries, error) {
	series := &fileClusterHostSeries{
		writtenByteCounts: map[string]uint64{},
		writesCompleted:   map[string]uint64{},
		readsCompleted:    map[string]uint64{},
		mountpointDevices: map[string]string{},
	}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, valueText, ok := parseFileClusterSample(line)
		if !ok {
			continue
		}
		switch name {
		case fileClusterFilesystemSizeSeries:
			if device, mountpoint := labels["device"], labels["mountpoint"]; device != "" && mountpoint != "" {
				series.mountpointDevices[path.Clean(mountpoint)] = device
			}
		case fileClusterDiskWrittenBytesSeries, fileClusterDiskWritesCompletedSeries, fileClusterDiskReadsCompletedSeries:
			device := labels["device"]
			value, err := strconv.ParseFloat(valueText, 64)
			if device == "" || err != nil || math.IsNaN(value) || value < 0 || float64(math.MaxInt64) <= value {
				continue
			}
			counter := uint64(value)
			switch name {
			case fileClusterDiskWrittenBytesSeries:
				series.writtenByteCounts[device] = counter
			case fileClusterDiskWritesCompletedSeries:
				series.writesCompleted[device] = counter
			case fileClusterDiskReadsCompletedSeries:
				series.readsCompleted[device] = counter
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return series, nil
}

// One sample line: name{label="value",...} value [timestamp]. ok is false for a line of another shape.
func parseFileClusterSample(line string) (name string, labels map[string]string, value string, ok bool) {
	labels = map[string]string{}
	rest := line
	if open := strings.IndexByte(rest, '{'); 0 <= open {
		name = rest[:open]
		rest = rest[open+1:]
		for {
			rest = strings.TrimLeft(rest, " ,")
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			equals := strings.IndexByte(rest, '=')
			if equals <= 0 || len(rest) < equals+2 || rest[equals+1] != '"' {
				return "", nil, "", false
			}
			labelName := strings.TrimSpace(rest[:equals])
			rest = rest[equals+2:]
			var labelValue strings.Builder
			closed := false
			for i := 0; i < len(rest); i += 1 {
				c := rest[i]
				if c == '\\' && i+1 < len(rest) {
					i += 1
					switch rest[i] {
					case 'n':
						labelValue.WriteByte('\n')
					default:
						labelValue.WriteByte(rest[i])
					}
					continue
				}
				if c == '"' {
					rest = rest[i+1:]
					closed = true
					break
				}
				labelValue.WriteByte(c)
			}
			if !closed {
				return "", nil, "", false
			}
			labels[labelName] = labelValue.String()
		}
	} else {
		space := strings.IndexAny(rest, " \t")
		if space <= 0 {
			return "", nil, "", false
		}
		name = rest[:space]
		rest = rest[space:]
	}
	fields := strings.Fields(rest)
	if name == "" || len(fields) < 1 {
		return "", nil, "", false
	}
	return name, labels, fields[0], true
}
