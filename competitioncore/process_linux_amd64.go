// Native process classification holds both pidfd and a proc-directory lease.
// The fixed Linux/amd64 syscall numbers are ABI facts, not runtime options.
package competitioncore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// The kernel namespace and directory identity must survive every observation.
type nativeProcess struct {
	root       *directory
	process    *directory
	pidfd      *os.File
	invocation Invocation
	settings   Settings
	directory  fileIdentity
	birth      uint64
	checkPidfd func() error
	closeRoot  bool
}

// A pidfd_open result alone is insufficient: the held proc inode, host PID,
// birth tick, namespace PID and credentials must all agree before and after.
func openNativeProcess(ctx context.Context, invocation Invocation, settings Settings) (*nativeProcess, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	root, err := openDirectory("/proc", false)
	if err != nil {
		return nil, err
	}
	if err := requireFilesystem(root, 0x9fa0); err != nil {
		root.Close()
		return nil, err
	}
	const pidfdOpen = 434
	fd, _, errno := syscall.Syscall(pidfdOpen, uintptr(invocation.HostPid), 0, 0)
	if errno != 0 {
		root.Close()
		return nil, errors.New("crashing process has no kernel pidfd lease")
	}
	pidfd := os.NewFile(fd, "core-crashing-pidfd")
	self, err := newProcessLease(root, invocation, pidfd, settings)
	if err != nil {
		pidfd.Close()
		root.Close()
		return nil, err
	}
	self.closeRoot = true
	self.checkPidfd = func() error {
		content, err := readRelative(root, fmt.Sprintf("%d/fdinfo/%d", os.Getpid(), pidfd.Fd()), settings.ProcBytes, false)
		if err != nil {
			return err
		}
		found := false
		for _, line := range strings.Split(string(content), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok && key == "Pid" {
				pid, err := parseUnsigned(strings.TrimSpace(value))
				if found || err != nil || pid != invocation.HostPid {
					return errors.New("crashing pidfd no longer names the exact host process")
				}
				found = true
			}
		}
		if !found {
			return errors.New("crashing pidfd lacks its positive kernel identity")
		}
		return nil
	}
	return self, nil
}

// Tests supply a private proc tree and a private pidfd-check seam; native
// construction above always supplies actual kernel pidfd/fdinfo observations.
func newProcessLease(root *directory, invocation Invocation, pidfd *os.File, settings Settings) (*nativeProcess, error) {
	process, err := root.OpenDirectory(strconv.FormatUint(invocation.HostPid, 10))
	if err != nil {
		return nil, err
	}
	info, err := process.Stat(".")
	if err != nil {
		process.Close()
		return nil, err
	}
	directory, err := identity(info)
	if err != nil {
		process.Close()
		return nil, err
	}
	content, err := readRelative(process, "stat", settings.ProcBytes, false)
	if err != nil {
		process.Close()
		return nil, err
	}
	birth, err := parseProcessStat(content, invocation.HostPid)
	if err != nil {
		process.Close()
		return nil, err
	}
	return &nativeProcess{root: root, process: process, pidfd: pidfd, invocation: invocation, settings: settings, directory: directory, birth: birth}, nil
}

// No process pathname can be reopened after a birth/inode mismatch.
func (self *nativeProcess) Facts(ctx context.Context) (ProcessFacts, error) {
	if ctx.Err() != nil || self.checkPidfd == nil || self.checkPidfd() != nil {
		return ProcessFacts{}, errors.New("crashing process pidfd ownership changed")
	}
	info, err := self.root.Lstat(strconv.FormatUint(self.invocation.HostPid, 10))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ProcessFacts{}, errors.New("crashing process directory disappeared")
	}
	current, err := identity(info)
	if err != nil || current != self.directory {
		return ProcessFacts{}, errors.New("crashing process directory generation changed")
	}
	read := func(name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return readRelative(self.process, name, self.settings.ProcBytes, false)
	}
	first, err := read("stat")
	if err != nil {
		return ProcessFacts{}, err
	}
	birth, err := parseProcessStat(first, self.invocation.HostPid)
	if err != nil || birth != self.birth {
		return ProcessFacts{}, errors.New("crashing process differs from its held birth generation")
	}
	status, err := read("status")
	if err != nil {
		return ProcessFacts{}, err
	}
	facts, err := parseProcessStatus(status, self.invocation)
	if err != nil {
		return ProcessFacts{}, err
	}
	cgroup, err := read("cgroup")
	if err != nil {
		return ProcessFacts{}, err
	}
	group, err := parseProcessCgroup(cgroup, self.settings)
	if err != nil {
		return ProcessFacts{}, err
	}
	last, err := read("stat")
	if err != nil {
		return ProcessFacts{}, err
	}
	lastBirth, err := parseProcessStat(last, self.invocation.HostPid)
	if err != nil || birth != lastBirth || self.checkPidfd() != nil || ctx.Err() != nil {
		return ProcessFacts{}, errors.New("crashing process birth/pidfd changed during observation")
	}
	facts.Birth, facts.Cgroup = birth, group
	return facts, nil
}

// Leases never survive a dispatcher return and hold no core stream data.
func (self *nativeProcess) Close() error {
	var result error
	if self.process != nil {
		result = errors.Join(result, self.process.Close())
		self.process = nil
	}
	if self.pidfd != nil {
		result = errors.Join(result, self.pidfd.Close())
		self.pidfd = nil
	}
	if self.closeRoot && self.root != nil {
		result = errors.Join(result, self.root.Close())
		self.root = nil
	}
	return result
}

// /proc stat's comm may contain spaces and parentheses; the final delimiter
// determines field 22, never a whitespace split of the complete line.
func parseProcessStat(content []byte, pid uint64) (uint64, error) {
	value := strings.TrimSuffix(string(content), "\n")
	open, close := strings.Index(value, " ("), strings.LastIndex(value, ") ")
	if open <= 0 || close <= open {
		return 0, errors.New("malformed crashing process stat")
	}
	actual, err := parseUnsigned(value[:open])
	fields := strings.Fields(value[close+2:])
	if err != nil || actual != pid || len(fields) < 20 {
		return 0, errors.New("crashing process stat does not name its original host PID")
	}
	birth, err := parseUnsigned(fields[19])
	if err != nil || birth == 0 {
		return 0, errors.New("crashing process stat has no positive birth identity")
	}
	return birth, nil
}

// Exactly one unified hierarchy is admitted; legacy or duplicate views are
// ambiguous and cannot become positive ordinary-process authority.
func parseProcessCgroup(content []byte, settings Settings) (string, error) {
	value := strings.TrimSuffix(string(content), "\n")
	if !strings.HasPrefix(value, "0::") || strings.ContainsRune(value, '\n') || !validCgroup(strings.TrimPrefix(value, "0::"), settings) {
		return "", errors.New("crashing process has no exact unified cgroup")
	}
	return strings.TrimPrefix(value, "0::"), nil
}

// Positive CoreDumping and kernel-wait semantics are both required. UID/GID
// are host initial-namespace values, not identities supplied by a candidate.
func parseProcessStatus(content []byte, invocation Invocation) (ProcessFacts, error) {
	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "Pid", "Tgid", "NSpid", "Uid", "Gid", "CoreDumping":
			if _, exists := values[key]; exists {
				return ProcessFacts{}, errors.New("ambiguous crashing process status")
			}
			values[key] = strings.TrimSpace(value)
		}
	}
	pid, err := parseUnsigned(values["Pid"])
	tgid, tgidErr := parseUnsigned(values["Tgid"])
	namespace := strings.Fields(values["NSpid"])
	if err != nil || tgidErr != nil || pid != invocation.HostPid || tgid != pid || len(namespace) == 0 || values["CoreDumping"] != "1" {
		return ProcessFacts{}, errors.New("crashing process status lacks exact live core custody")
	}
	for _, part := range namespace {
		if value, err := parseUnsigned(part); err != nil || value == 0 || value > 2147483647 {
			return ProcessFacts{}, errors.New("invalid crashing namespace PID")
		}
	}
	if namespace[0] != strconv.FormatUint(invocation.HostPid, 10) || namespace[len(namespace)-1] != strconv.FormatUint(invocation.NamespacePid, 10) {
		return ProcessFacts{}, errors.New("crashing process namespace generation changed")
	}
	for key, expected := range map[string]uint64{"Uid": invocation.Uid, "Gid": invocation.Gid} {
		fields := strings.Fields(values[key])
		if len(fields) != 4 {
			return ProcessFacts{}, errors.New("crashing process credentials are incomplete")
		}
		for index, value := range fields {
			parsed, err := parseUnsigned(value)
			if err != nil || parsed > 4294967295 || index == 0 && parsed != expected {
				return ProcessFacts{}, errors.New("crashing process initial credentials changed")
			}
		}
	}
	return ProcessFacts{HostPid: pid, NamespacePid: invocation.NamespacePid, Uid: invocation.Uid, Gid: invocation.Gid}, nil
}

// Canonical unsigned decimal rejects signs, whitespace and overflow.
func parseUnsigned(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		return 0, errors.New("noncanonical unsigned core metadata")
	}
	return parsed, nil
}

// An ordinary directory cannot masquerade as procfs or cgroupfs in production.
func requireFilesystem(root *directory, magic int64) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(file.Fd()), &stat); err != nil || int64(stat.Type) != magic {
		return errors.New("native core authority is not its required kernel filesystem")
	}
	return nil
}
