package main

// What §22.8's nvme row records per drive beyond the exporter's diskstats (§19.6's counters row serves only those):
// each drive's XFS log and file bytes and its SMART log before and after. The storage host answers them itself:
//
//	filecluster-bench hoststats <drive path>...
//
// run on the host, finds each path's filesystem in /proc/self/mountinfo, reads /sys/fs/xfs/<device>/stats/stats (the
// log line's blocks, 512 B each, and the xpc line's bytes written) and runs nvme smart-log -o json on the device, and
// prints one JSON answer. It reads only, and names a drive by its path and kernel device, never a serial number.
//
// The harness reaches each host through -host_stats_command, a command template the operator gives, such as an ssh
// invocation of the above: {host} is the host's name from admin info and {paths} its drives' paths, each quoted. No
// host name is in this code; the operator's template and the cluster's own admin info supply them.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/urnetwork/server"
)

// One drive in a hoststats answer.
type HostStatsDrive struct {
	Path   string `json:"path"`
	Device string `json:"device"`
	// Absent where the host could not read them.
	XfsLogByteCount  *uint64     `json:"xfs_log_byte_count,omitempty"`
	XfsFileByteCount *uint64     `json:"xfs_file_byte_count,omitempty"`
	Smart            *DriveSmart `json:"smart,omitempty"`
	Errors           []string    `json:"errors,omitempty"`
}

// A host's answer.
type HostStatsAnswer struct {
	Drives []*HostStatsDrive `json:"drives"`
}

// Where a host's files are and how it runs nvme-cli, so a test can stand in for both.
type hostStatsSource struct {
	// "/" on a host
	root     string
	runSmart func(ctx context.Context, device string) ([]byte, error)
}

func runHostStatsMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	flags := flag.NewFlagSet("filecluster-bench hoststats", flag.ContinueOnError)
	root := flags.String("root", "/", "the filesystem root /proc and /sys are under")
	nvmeCommand := flags.String("nvme", "nvme", "nvme-cli's command")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() == 0 {
		return false, refuseStart("hoststats needs the drives' paths")
	}
	source := &hostStatsSource{
		root: *root,
		runSmart: func(ctx context.Context, device string) ([]byte, error) {
			return execCommand(ctx, "/", *nvmeCommand, "smart-log", "-o", "json", "/dev/"+device)
		},
	}
	answer := source.read(ctx, flags.Args())
	data, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		return false, err
	}
	_, err = stdout.Write(append(data, '\n'))
	return err == nil, err
}

// Reads each path's drive: see the file header. A drive it cannot read carries the reasons, never a guess.
func (self *hostStatsSource) read(ctx context.Context, paths []string) *HostStatsAnswer {
	answer := &HostStatsAnswer{Drives: []*HostStatsDrive{}}
	mounts, mountsErr := self.mounts()
	for _, path := range paths {
		drive := &HostStatsDrive{Path: path}
		answer.Drives = append(answer.Drives, drive)
		if mountsErr != nil {
			drive.Errors = append(drive.Errors, mountsErr.Error())
			continue
		}
		device, ok := deviceOfPath(mounts, path)
		if !ok {
			drive.Errors = append(drive.Errors, "no filesystem holds it")
			continue
		}
		drive.Device = device
		if logByteCount, fileByteCount, err := self.xfsStats(device); err != nil {
			drive.Errors = append(drive.Errors, err.Error())
		} else {
			drive.XfsLogByteCount = &logByteCount
			drive.XfsFileByteCount = &fileByteCount
		}
		if smart, err := self.smart(ctx, device); err != nil {
			drive.Errors = append(drive.Errors, err.Error())
		} else {
			drive.Smart = smart
		}
	}
	return answer
}

// Each mountpoint's device, from mountinfo: the source after the separator, symlinks resolved to the kernel name.
func (self *hostStatsSource) mounts() (map[string]string, error) {
	file, err := os.Open(filepath.Join(self.root, "proc/self/mountinfo"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	mounts := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 5 || len(fields) < separator+3 {
			continue
		}
		mountpoint := unescapeMountinfo(fields[4])
		source := fields[separator+2]
		if !strings.HasPrefix(source, "/dev/") {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(filepath.Join(self.root, source)); err == nil {
			source = resolved
		}
		mounts[mountpoint] = filepath.Base(source)
	}
	return mounts, scanner.Err()
}

// mountinfo's octal escapes of space, tab, newline and backslash.
var mountinfoEscapePattern = regexp.MustCompile(`\\[0-7]{3}`)

func unescapeMountinfo(field string) string {
	return mountinfoEscapePattern.ReplaceAllStringFunc(field, func(escape string) string {
		value, err := strconv.ParseUint(escape[1:], 8, 8)
		if err != nil {
			return escape
		}
		return string(rune(value))
	})
}

// The device of the longest mountpoint holding path.
func deviceOfPath(mounts map[string]string, path string) (string, bool) {
	clean := filepath.Clean(path)
	best := ""
	device := ""
	for mountpoint, mountDevice := range mounts {
		if clean == mountpoint || strings.HasPrefix(clean, strings.TrimSuffix(mountpoint, "/")+"/") {
			if len(best) <= len(mountpoint) {
				best = mountpoint
				device = mountDevice
			}
		}
	}
	return device, device != ""
}

// XFS's log bytes (the log line's blocks, 512 B each) and file bytes written (the xpc line's second field) for one
// filesystem.
func (self *hostStatsSource) xfsStats(device string) (uint64, uint64, error) {
	data, err := os.ReadFile(filepath.Join(self.root, "sys/fs/xfs", device, "stats/stats"))
	if err != nil {
		return 0, 0, err
	}
	var logBlocks, writeBytes uint64
	logOk, xpcOk := false, false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "log":
			if 2 < len(fields) {
				logBlocks, err = strconv.ParseUint(fields[2], 10, 64)
				logOk = err == nil
			}
		case "xpc":
			if 2 < len(fields) {
				writeBytes, err = strconv.ParseUint(fields[2], 10, 64)
				xpcOk = err == nil
			}
		}
	}
	if !logOk || !xpcOk {
		return 0, 0, fmt.Errorf("%s's XFS stats lack the log or xpc line", device)
	}
	return logBlocks * 512, writeBytes, nil
}

// One device's SMART log, by nvme-cli's JSON, whose field names differ across its versions.
func (self *hostStatsSource) smart(ctx context.Context, device string) (*DriveSmart, error) {
	out, err := self.runSmart(ctx, device)
	if err != nil {
		return nil, err
	}
	raw := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s's smart log: %v", device, err)
	}
	fields := map[string]json.Number{}
	for key, value := range raw {
		if number, ok := value.(json.Number); ok {
			fields[key] = number
		}
	}
	pick := func(names ...string) (uint64, bool) {
		for _, name := range names {
			if number, ok := fields[name]; ok {
				if value, err := strconv.ParseUint(number.String(), 10, 64); err == nil {
					return value, true
				}
			}
		}
		return 0, false
	}
	smart := &DriveSmart{}
	ok := true
	var found bool
	if smart.DataUnitsWritten, found = pick("data_units_written"); !found {
		ok = false
	}
	if smart.PercentageUsed, found = pick("percent_used", "percentage_used"); !found {
		ok = false
	}
	if smart.AvailableSpare, found = pick("avail_spare", "available_spare"); !found {
		ok = false
	}
	if smart.MediaErrors, found = pick("media_errors"); !found {
		ok = false
	}
	if !ok {
		return nil, fmt.Errorf("%s's smart log lacks a field D-OPS-19 reads", device)
	}
	return smart, nil
}

// The harness's side: runs the operator's template per host and maps each answer onto admin info's drives.
type commandHostStats struct {
	template  string
	adminInfo func(clusterId string) *server.FileClusterAdminInfo
	run       func(ctx context.Context, command string) ([]byte, error)
}

func newCommandHostStats(template string, adminInfo func(clusterId string) *server.FileClusterAdminInfo, run func(ctx context.Context, command string) ([]byte, error)) *commandHostStats {
	if run == nil {
		run = func(ctx context.Context, command string) ([]byte, error) {
			return execCommand(ctx, "/", "/bin/sh", "-c", command)
		}
	}
	return &commandHostStats{template: template, adminInfo: adminInfo, run: run}
}

// A host name as admin info gives it, refused unless it is plain, so nothing it holds reaches the shell.
var hostStatsHostPattern = regexp.MustCompile(`^[A-Za-z0-9.\-]+$`)

// A path quoted for the shell.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func (self *commandHostStats) ReadHostStats(ctx context.Context, clusterId string) (map[string]*DriveHostStats, error) {
	info := self.adminInfo(clusterId)
	if info == nil {
		return nil, errors.New("admin info unread")
	}
	hostPaths := map[string][]string{}
	pathDriveIds := map[string]string{}
	hosts := []string{}
	for _, drive := range info.Drives {
		endpoint := drive.HostEndpoint
		if endpoint == "" {
			endpoint = drive.Endpoint
		}
		host, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://"))
		if err != nil {
			host = endpoint
		}
		if !hostStatsHostPattern.MatchString(host) || drive.Path == "" {
			continue
		}
		if _, ok := hostPaths[host]; !ok {
			hosts = append(hosts, host)
		}
		hostPaths[host] = append(hostPaths[host], drive.Path)
		pathDriveIds[host+"\x00"+drive.Path] = fmt.Sprintf("%d-%d-%d", drive.PoolIndex, drive.SetIndex, drive.DiskIndex)
	}
	stats := map[string]*DriveHostStats{}
	errs := []error{}
	for _, host := range hosts {
		quoted := []string{}
		for _, path := range hostPaths[host] {
			quoted = append(quoted, shellQuote(path))
		}
		command := strings.ReplaceAll(strings.ReplaceAll(self.template, "{host}", host), "{paths}", strings.Join(quoted, " "))
		out, err := self.run(ctx, command)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			continue
		}
		answer := &HostStatsAnswer{}
		if err := json.Unmarshal(out, answer); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			continue
		}
		for _, drive := range answer.Drives {
			driveId, ok := pathDriveIds[host+"\x00"+drive.Path]
			if !ok || drive.XfsLogByteCount == nil || drive.XfsFileByteCount == nil {
				continue
			}
			stats[driveId] = &DriveHostStats{XfsLogByteCount: *drive.XfsLogByteCount, XfsFileByteCount: *drive.XfsFileByteCount, Smart: drive.Smart}
		}
	}
	if len(stats) == 0 && 0 < len(errs) {
		return nil, errors.Join(errs...)
	}
	return stats, nil
}
