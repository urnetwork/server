// Linux supplies boot clocks, process generations and cgroup-v2 ownership for
// cross-process SSH reservations. Files are private, bounded and atomically
// replaced under a stable flock; no SSH or production command runs here.
package monitor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type sharedSshAdmission struct {
	store *sshAdmissionStore
	owner sshAdmissionOwner
}

type sshAdmissionStore struct {
	directory string
	bootId    string
	now       func() (int64, error)
	status    func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus
}

// Register before the first probe so a diagnostic need not wait for a cadence
// tick. Replacing a watcher requires its process and all old claims to be gone.
func newSharedSshAdmission(ctx context.Context, directory string, hosts []string) (sshAdmissionBackend, error) {
	owner, err := sshAdmissionCurrentOwner(ctx)
	if err != nil {
		return nil, err
	}
	store, err := newSshAdmissionStore(directory, owner.Identity.BootId)
	if err != nil {
		return nil, err
	}
	err = store.update(ctx, func(state *sshAdmissionState, now int64) error {
		if state.Version != 0 {
			if state.BootId != owner.Identity.BootId {
				*state = sshAdmissionState{}
			} else {
				store.prune(ctx, state, now)
				if state.Watcher != owner && (store.status(ctx, state.Watcher) != sshAdmissionOwnerGone || len(state.Requests) != 0) {
					return errors.New("SSH admission watcher still owns work")
				}
				if state.Watcher == owner {
					if !slices.Equal(state.Hosts, hosts) {
						return errors.New("SSH admission inventory changed")
					}
					return nil
				}
			}
		}
		*state = sshAdmissionState{Version: 1, BootId: owner.Identity.BootId, Watcher: owner, Hosts: slices.Clone(hosts), NextTicket: 1, Turn: sshAdmissionDiagnostic}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &sharedSshAdmission{store: store, owner: owner}, nil
}

// Admission uses the caller's existing budget and never starts a command after
// failed acquisition. Release errors leave capacity reserved and are surfaced.
func (self *sharedSshAdmission) acquire(ctx context.Context, host string) (func() error, error) {
	return self.store.acquire(ctx, self.owner, self.owner.Identity, sshAdmissionWatch, []string{host})
}

func newSshAdmissionStore(directory, bootId string) (*sshAdmissionStore, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("SSH admission directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, errors.New("SSH admission directory unavailable")
	}
	real, err := filepath.EvalSymlinks(directory)
	if err != nil || real != directory {
		return nil, errors.New("SSH admission directory is not canonical")
	}
	file, err := os.Open(directory)
	if err != nil {
		return nil, errors.New("SSH admission directory unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("SSH admission directory is not private")
	}
	var stat unix.Stat_t
	if unix.Fstat(int(file.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("SSH admission directory owner invalid")
	}
	return &sshAdmissionStore{directory: directory, bootId: bootId, now: sshAdmissionBootNanos, status: sshAdmissionNativeStatus}, nil
}

// Bounded polling is only local queue observation. It never retries SSH or
// financial work, and cancellation removes only this unstarted request.
func (self *sshAdmissionStore) acquire(ctx context.Context, owner sshAdmissionOwner, expected SshAdmissionWatcher, class string, hosts []string) (release func() error, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hosts = slices.Clone(hosts)
	slices.Sort(hosts)
	if len(hosts) < 1 || len(hosts) > 2 || (len(hosts) == 2 && hosts[0] == hosts[1]) {
		return nil, errors.New("SSH admission demand invalid")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("SSH admission token unavailable")
	}
	token := hex.EncodeToString(random[:])
	deadline := time.Now().Add(120 * time.Second)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	queued := false
	defer func() {
		if resultErr != nil && queued {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, self.remove(cleanup, token, owner))
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("SSH admission queue deadline")
		}
		granted := false
		err := self.update(ctx, func(state *sshAdmissionState, now int64) error {
			if state.Version != 1 || state.BootId != self.bootId || state.Watcher.Identity != expected || self.status(ctx, state.Watcher) != sshAdmissionOwnerLive {
				return errors.New("SSH admission watcher authority unavailable")
			}
			self.prune(ctx, state, now)
			if !queued {
				if len(state.Requests) >= sshAdmissionQueueLimit || state.NextTicket == ^uint64(0) {
					return errors.New("SSH admission queue full")
				}
				for _, request := range state.Requests {
					if class == sshAdmissionDiagnostic && request.Class == class {
						return errors.New("SSH diagnostic already owns a turn")
					}
				}
				if class == sshAdmissionDiagnostic && (owner.Cgroup == state.Watcher.Cgroup || owner.Unit == state.Watcher.Unit) {
					return errors.New("SSH diagnostic requires a separate owner service")
				}
				for _, host := range hosts {
					if !slices.Contains(state.Hosts, host) {
						return errors.New("SSH admission host outside inventory")
					}
				}
				state.Requests = append(state.Requests, sshAdmissionRequest{Token: token, Ticket: state.NextTicket, Class: class, Owner: owner, Hosts: hosts, ExpiresNanos: now + time.Until(deadline).Nanoseconds()})
				state.NextTicket++
				queued = true
			}
			found := false
			for _, request := range state.Requests {
				if request.Token == token {
					found = true
					if request.Active {
						return errors.New("SSH admission unexpected active request")
					}
				}
			}
			if !found {
				return errors.New("SSH admission queued request expired")
			}
			granted = state.grant(token)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if granted {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return func() error {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				return self.remove(cleanup, token, owner)
			}, nil
		}
		if err := waitSshAdmission(ctx); err != nil {
			return nil, err
		}
	}
}

func (self *sshAdmissionStore) prune(ctx context.Context, state *sshAdmissionState, now int64) {
	statuses := map[sshAdmissionOwner]sshAdmissionOwnerStatus{}
	for _, request := range state.Requests {
		if _, ok := statuses[request.Owner]; !ok {
			statuses[request.Owner] = self.status(ctx, request.Owner)
		}
	}
	state.prune(now, statuses)
}

func (self *sshAdmissionStore) remove(ctx context.Context, token string, owner sshAdmissionOwner) error {
	return self.update(ctx, func(state *sshAdmissionState, _ int64) error {
		for i, request := range state.Requests {
			if request.Token == token {
				if request.Owner != owner {
					return errors.New("SSH admission release owner mismatch")
				}
				state.Requests = slices.Delete(state.Requests, i, i+1)
				break
			}
		}
		return nil
	})
}

// Lock-file identity is stable across state renames. Failed writes cannot
// publish partial capacity, and successful publication is fsynced before use.
func (self *sshAdmissionStore) update(ctx context.Context, change func(*sshAdmissionState, int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := unix.Open(self.directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("SSH admission directory unavailable")
	}
	defer unix.Close(directory)
	lock, err := unix.Openat(directory, "queue.lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return errors.New("SSH admission lock unavailable")
	}
	defer unix.Close(lock)
	if !sshAdmissionPrivateFile(lock) {
		return errors.New("SSH admission lock invalid")
	}
	for {
		err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return errors.New("SSH admission lock failed")
		}
		if err := waitSshAdmission(ctx); err != nil {
			return err
		}
	}
	defer unix.Flock(lock, unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	state := sshAdmissionState{}
	var previous []byte
	fd, err := unix.Openat(directory, "queue.json", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == nil {
		file := os.NewFile(uintptr(fd), "queue.json")
		defer file.Close()
		if !sshAdmissionPrivateFile(fd) {
			return errors.New("SSH admission state permissions invalid")
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, sshAdmissionStateLimit+1))
		if readErr != nil || len(raw) > sshAdmissionStateLimit || sshAdmissionDecode(raw, &state) != nil {
			return errors.New("SSH admission state unreadable")
		}
		if err := state.validate(); err != nil {
			return err
		}
		previous = raw
	} else if err != unix.ENOENT {
		return errors.New("SSH admission state unavailable")
	}
	now, err := self.now()
	if err != nil {
		return errors.New("SSH admission boot clock unavailable")
	}
	if err := change(&state, now); err != nil {
		return err
	}
	if err := state.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > sshAdmissionStateLimit {
		return errors.New("SSH admission state exceeds limit")
	}
	if bytes.Equal(raw, previous) {
		return nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("SSH admission state token unavailable")
	}
	name := ".queue-" + hex.EncodeToString(random[:])
	output, err := unix.Openat(directory, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("SSH admission state write unavailable")
	}
	defer unix.Unlinkat(directory, name, 0)
	file := os.NewFile(uintptr(output), name)
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("SSH admission state write failed")
	}
	if unix.Renameat(directory, name, directory, "queue.json") != nil || unix.Fsync(directory) != nil {
		return errors.New("SSH admission state publication failed")
	}
	return nil
}

func sshAdmissionPrivateFile(fd int) bool {
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0777 == 0600 && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func waitSshAdmission(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func sshAdmissionBootNanos() (int64, error) {
	var value unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &value)
	return value.Nano(), err
}

// Strict decoding rejects duplicate as well as unknown object keys. Corrupt
// coordination state is unavailable capacity, never an empty queue.
func sshAdmissionDecode(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 16 {
			return errors.New("SSH admission JSON depth")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		keys := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("SSH admission duplicate key")
				}
				keys[name] = true
			}
			if err := consume(depth + 1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := consume(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("SSH admission trailing JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// The helper never loads Vault or monitor settings and never executes a target
// command. Its owner must be a dedicated systemd service with group cleanup.
func RunSshAdmissionLease(ctx context.Context, requestPath string, input io.Reader, output io.Writer) error {
	fd, err := unix.Open(requestPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("SSH lease request unavailable")
	}
	file := os.NewFile(uintptr(fd), requestPath)
	defer file.Close()
	if !sshAdmissionPrivateFile(fd) {
		return errors.New("SSH lease request is not private")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	var request SshAdmissionLeaseRequest
	if err != nil || len(raw) > 4096 || sshAdmissionDecode(raw, &request) != nil || request.WaitSeconds < 1 || request.WaitSeconds > 120 {
		return errors.New("SSH lease request invalid")
	}
	owner, err := sshAdmissionCurrentOwner(ctx)
	if err != nil {
		return err
	}
	store, err := newSshAdmissionStore(request.Directory, owner.Identity.BootId)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(request.WaitSeconds)*time.Second)
	release, err := store.acquire(waitCtx, owner, request.Watcher, sshAdmissionDiagnostic, request.Hosts)
	cancel()
	if err != nil {
		return err
	}
	// After a grant, no path except this explicit joined-release message can
	// release. In particular an EOF may mean the owner died with SSH alive.
	if _, err := io.WriteString(output, "{\"granted\":true}\n"); err != nil {
		return errors.New("SSH lease grant delivery failed; reservation retained")
	}
	return awaitSshAdmissionRelease(ctx, input, release)
}

func awaitSshAdmissionRelease(ctx context.Context, input io.Reader, release func() error) error {
	read := make(chan bool, 1)
	go func() {
		var value [8]byte
		_, err := io.ReadFull(input, value[:])
		read <- err == nil && string(value[:]) == "release\n"
	}()
	select {
	case <-ctx.Done():
		return errors.New("SSH lease canceled; reservation retained")
	case valid := <-read:
		if !valid {
			return errors.New("SSH lease release absent; reservation retained")
		}
		return release()
	}
}

func sshAdmissionRead(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("SSH admission native field unavailable")
	}
	return raw, nil
}

func sshAdmissionProcess(pid int, bootId string) (SshAdmissionWatcher, string, error) {
	raw, err := sshAdmissionRead(fmt.Sprintf("/proc/%d/stat", pid), 8192)
	if err != nil {
		return SshAdmissionWatcher{}, "", err
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return SshAdmissionWatcher{}, "", errors.New("SSH admission process malformed")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return SshAdmissionWatcher{}, "", errors.New("SSH admission process incomplete")
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return SshAdmissionWatcher{}, "", err
	}
	cgroup, err := sshAdmissionRead(fmt.Sprintf("/proc/%d/cgroup", pid), 8192)
	if err != nil {
		return SshAdmissionWatcher{}, "", err
	}
	line := strings.TrimSuffix(string(cgroup), "\n")
	if !strings.HasPrefix(line, "0::/") || strings.Contains(line, "\n") {
		return SshAdmissionWatcher{}, "", errors.New("SSH admission requires unified cgroup")
	}
	return SshAdmissionWatcher{Pid: pid, StartTicks: ticks, BootId: bootId}, strings.TrimPrefix(line, "0::"), nil
}

// Query only four secret-free service properties, with a finite native owner.
func sshAdmissionUnit(ctx context.Context, unit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "--user", "show", "--property=InvocationID,ControlGroup,KillMode,LoadState", "--", unit)
	var output sshAdmissionBoundedBuffer
	command.Stdout = &output
	if err := command.Run(); err != nil || output.over {
		return nil, errors.New("SSH admission service authority unavailable")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("SSH admission service malformed")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, errors.New("SSH admission service duplicate")
		}
		values[key] = value
	}
	if len(values) != 4 || values["LoadState"] != "loaded" || values["KillMode"] != "control-group" || !sshAdmissionHex(values["InvocationID"], 32) || values["ControlGroup"] == "" {
		return nil, errors.New("SSH admission service is not an owned group")
	}
	return values, nil
}

type sshAdmissionBoundedBuffer struct {
	bytes.Buffer
	over bool
}

func (self *sshAdmissionBoundedBuffer) Write(value []byte) (int, error) {
	if self.Len()+len(value) > 8192 {
		self.over = true
		return len(value), nil
	}
	return self.Buffer.Write(value)
}

func sshAdmissionCurrentOwner(ctx context.Context) (sshAdmissionOwner, error) {
	boot, err := sshAdmissionRead("/proc/sys/kernel/random/boot_id", 64)
	if err != nil {
		return sshAdmissionOwner{}, errors.New("SSH admission boot identity unavailable")
	}
	identity, cgroup, err := sshAdmissionProcess(os.Getpid(), strings.TrimSpace(string(boot)))
	if err != nil {
		return sshAdmissionOwner{}, errors.New("SSH admission process identity unavailable")
	}
	unit := filepath.Base(cgroup)
	if !strings.HasSuffix(unit, ".service") {
		return sshAdmissionOwner{}, errors.New("SSH admission requires an owned service")
	}
	values, err := sshAdmissionUnit(ctx, unit)
	if err != nil || values["ControlGroup"] != cgroup {
		return sshAdmissionOwner{}, errors.New("SSH admission service identity mismatch")
	}
	var stat unix.Stat_t
	if unix.Stat(filepath.Join("/sys/fs/cgroup", cgroup), &stat) != nil {
		return sshAdmissionOwner{}, errors.New("SSH admission cgroup identity unavailable")
	}
	owner := sshAdmissionOwner{Identity: identity, Unit: unit, InvocationId: values["InvocationID"], Cgroup: cgroup, Device: uint64(stat.Dev), Inode: stat.Ino}
	if !owner.valid() {
		return sshAdmissionOwner{}, errors.New("SSH admission owner invalid")
	}
	return owner, nil
}

// A reused path never releases an older claim. A missing original group can
// only have been removed after becoming empty; otherwise require its exact
// invocation/inode and populated=0. Unknown authority retains the reservation.
func sshAdmissionNativeStatus(ctx context.Context, owner sshAdmissionOwner) sshAdmissionOwnerStatus {
	return sshAdmissionProbeStatus(ctx, owner, sshAdmissionOwnerProbe{
		read: sshAdmissionRead, process: sshAdmissionProcess, stat: unix.Stat, unit: sshAdmissionUnit,
	})
}

// These are native read seams for deterministic process/cgroup race controls.
type sshAdmissionOwnerProbe struct {
	read    func(string, int64) ([]byte, error)
	process func(int, string) (SshAdmissionWatcher, string, error)
	stat    func(string, *unix.Stat_t) error
	unit    func(context.Context, string) (map[string]string, error)
}

func sshAdmissionProbeStatus(ctx context.Context, owner sshAdmissionOwner, probe sshAdmissionOwnerProbe) sshAdmissionOwnerStatus {
	boot, err := probe.read("/proc/sys/kernel/random/boot_id", 64)
	if err != nil || !sshAdmissionBootIdValid(strings.TrimSpace(string(boot))) {
		return sshAdmissionOwnerUnknown
	}
	if strings.TrimSpace(string(boot)) != owner.Identity.BootId {
		return sshAdmissionOwnerGone
	}
	identity, cgroup, err := probe.process(owner.Identity.Pid, owner.Identity.BootId)
	if err == nil && identity == owner.Identity {
		if cgroup == owner.Cgroup {
			return sshAdmissionOwnerLive
		}
		return sshAdmissionOwnerUnknown
	}
	if err != nil && !os.IsNotExist(err) {
		return sshAdmissionOwnerUnknown
	}
	path := filepath.Join("/sys/fs/cgroup", owner.Cgroup)
	var root unix.Stat_t
	if probe.stat("/sys/fs/cgroup", &root) != nil || uint64(root.Dev) != owner.Device {
		return sshAdmissionOwnerUnknown
	}
	var stat unix.Stat_t
	err = probe.stat(path, &stat)
	if errors.Is(err, unix.ENOENT) {
		return sshAdmissionOwnerGone
	}
	if err != nil || uint64(stat.Dev) != owner.Device || stat.Ino != owner.Inode {
		return sshAdmissionOwnerDead
	}
	values, err := probe.unit(ctx, owner.Unit)
	if err != nil || values["InvocationID"] != owner.InvocationId || values["ControlGroup"] != owner.Cgroup {
		return sshAdmissionOwnerDead
	}
	raw, err := probe.read(filepath.Join(path, "cgroup.events"), 4096)
	if err != nil {
		return sshAdmissionOwnerDead
	}
	populated := false
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "populated 0" {
			populated = true
		} else if strings.HasPrefix(line, "populated ") {
			return sshAdmissionOwnerDead
		}
	}
	var after unix.Stat_t
	if !populated || probe.stat(path, &after) != nil || after.Dev != stat.Dev || after.Ino != stat.Ino {
		return sshAdmissionOwnerDead
	}
	return sshAdmissionOwnerGone
}
