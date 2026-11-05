// Independent post-exit containment cleanup uses only the authenticated run's
// exact Docker labels, finite cgroups and private evidence mount. It never
// prunes daemon resources or discovers authority from an untrusted directory.
package competitionagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Persisted before launch, outside the worker's writable workspace. The build
// identities are exact image-key prefixes, never a shared slice prefix scan.
type cleanupPlan struct {
	Schema            int                `json:"schema"`
	RunId             string             `json:"run_id"`
	JobId             string             `json:"job_id"`
	RoundId           string             `json:"round_id"`
	Attempt           int                `json:"attempt"`
	CgroupNames       []string           `json:"cgroup_names"`
	EvidenceDirectory string             `json:"evidence_directory"`
	EvidenceSource    string             `json:"evidence_source"`
	DockerOwner       *DockerProxyPlan   `json:"docker_owner,omitempty"`
	ManagementCpuSet  string             `json:"management_cpu_set,omitempty"`
	WorkDeviceId      uint64             `json:"work_device_id,omitempty"`
	SliceResources    *runSliceResources `json:"slice_resources,omitempty"`
}

// Only a successful concrete cleanup permits releasing physical host custody.
type runCleaner interface {
	Cleanup(context.Context, cleanupPlan) error
}

// Private seams keep tests off host services while exercising real admission,
// ownership checks, ordering, budgets and resource inventories.
type hostCleaner struct {
	config   Config
	settings AgentSettings
	command  func(context.Context, []string) ([]byte, error)
	cgroups  cleanupCgroups
	mounts   cleanupMounts
}

// Killing and joining are distinct so a hard-exit cleanup cannot acknowledge a
// process tree merely because the kernel accepted a kill request.
type cleanupCgroups interface {
	Kill(context.Context, string) error
	Empty(context.Context, string) (bool, error)
}

// Mount implementations accept only the already validated exact run plan.
type cleanupMounts interface {
	Unmount(context.Context, cleanupPlan) error
}

// This constructor performs no I/O and never touches a daemon or host resource.
func newHostCleaner(config Config, settings AgentSettings) runCleaner {
	cleaner := &hostCleaner{config: config, settings: settings,
		cgroups: &hostCleanupCgroups{rootPath: "/sys/fs/cgroup", maximumBytes: settings.DiagnosticBytes, requireCgroupFs: true},
		mounts:  newHostCleanupMounts(config, settings)}
	cleaner.command = cleaner.runDocker
	return cleaner
}

var cleanupUuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var cleanupBuildSlicePattern = regexp.MustCompile(`^urnetwork-submission-build-[0-9a-f]{32}\.slice$`)
var cleanupEvaluationSlicePattern = regexp.MustCompile(`^urnetwork-evaluation-[0-9a-f]{32}-((baseline|candidate)-(0[1-9]|score)|live-0[1-9]|preflight)\.slice$`)
var errCleanupCgroupAbsent = errors.New("exact cleanup cgroup is absent")

const cleanupJobLabel = "com.urnetwork.competition.job-id"
const cleanupRoundLabel = "com.urnetwork.competition.round-id"
const cleanupRunLabel = "com.urnetwork.competition.run-id"

// The build cgroup is killed and joined first because BuildKit children need
// not carry job labels. Docker deletion requires an independent inspect proof.
func (self *hostCleaner) Cleanup(parent context.Context, plan cleanupPlan) error {
	if err := self.validate(plan); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupTimeout)
	defer cancel()
	for _, name := range plan.CgroupNames {
		if cleanupBuildSlicePattern.MatchString(name) {
			if err := self.cgroups.Kill(ctx, name); err != nil {
				return err
			}
			if err := self.joinCgroup(ctx, name); err != nil {
				return err
			}
		}
	}
	for _, name := range plan.CgroupNames {
		if !cleanupBuildSlicePattern.MatchString(name) {
			if err := self.cgroups.Kill(ctx, name); err != nil {
				return err
			}
		}
	}
	containers, err := self.list(ctx, plan, false)
	if err != nil {
		return err
	}
	networks, err := self.list(ctx, plan, true)
	if err != nil {
		return err
	}
	if len(containers) > self.settings.CleanupObjectCount-len(networks) {
		return errors.New("run cleanup object count exceeds its total bound")
	}
	allowed := map[string]bool{}
	for _, name := range plan.CgroupNames {
		allowed[name] = true
	}
	for _, id := range containers {
		if err := self.inspect(ctx, plan, id, false, allowed); err != nil {
			return err
		}
	}
	for _, id := range networks {
		if err := self.inspect(ctx, plan, id, true, allowed); err != nil {
			return err
		}
	}
	for _, id := range containers {
		if _, err := self.docker(ctx, []string{"container", "rm", "--force", "--", id}); err != nil {
			return err
		}
	}
	for _, id := range networks {
		if _, err := self.docker(ctx, []string{"network", "rm", "--", id}); err != nil {
			return err
		}
	}
	for _, name := range plan.CgroupNames {
		if err := self.joinCgroup(ctx, name); err != nil {
			return err
		}
	}
	for _, network := range []bool{false, true} {
		remaining, err := self.list(ctx, plan, network)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return errors.New("run cleanup left exact-label Docker resources")
		}
	}
	if err := self.mounts.Unmount(ctx, plan); err != nil {
		return err
	}
	return ctx.Err()
}

// Every accepted cgroup comes from the finite evaluator stage vocabulary. An
// authenticated build identity is allowed only as one exact 32-hex slice name.
func (self *hostCleaner) validate(plan cleanupPlan) error {
	if self.command == nil || self.cgroups == nil || self.mounts == nil || self.settings.CleanupObjectCount <= 0 || self.settings.CleanupTimeout <= 0 ||
		self.settings.CleanupCommandTimeout <= 0 || self.settings.CleanupPollInterval <= 0 || self.settings.DiagnosticBytes <= 0 ||
		len(plan.CgroupNames) > self.settings.CleanupObjectCount {
		return errors.New("invalid finite run cleanup authority or bounds")
	}
	return validateCleanupPlan(plan, self.config)
}

// Admission checks the concrete cleanup scope without any host I/O. Runtime
// cleanup separately checks its configured count, command and lifetime budgets.
func validateCleanupPlan(plan cleanupPlan, config Config) error {
	if plan.Schema != Schema || !digestPattern.MatchString(plan.RunId) || !cleanupUuidPattern.MatchString(plan.JobId) || !cleanupUuidPattern.MatchString(plan.RoundId) || plan.Attempt <= 0 ||
		len(plan.CgroupNames) == 0 || !filepath.IsAbs(config.WorkDirectory) || filepath.Clean(config.WorkDirectory) != config.WorkDirectory || config.WorkDirectory == "/" {
		return errors.New("invalid finite run cleanup identity")
	}
	expectedDirectory := filepath.Join(config.WorkDirectory, plan.RunId, ".evidence-runtime")
	expectedSource := "urnetwork-evidence-" + strings.ReplaceAll(plan.JobId, "-", "") + "-" + strconv.Itoa(plan.Attempt)
	if plan.EvidenceDirectory != expectedDirectory || plan.EvidenceSource != expectedSource {
		return errors.New("run cleanup evidence scope differs from its exact private identity")
	}
	tokenDigest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", plan.JobId, plan.Attempt)))
	token := hex.EncodeToString(tokenDigest[:])[:32]
	allowed := map[string]bool{}
	for index := 1; index <= 9; index++ {
		for _, role := range []string{"baseline", "candidate"} {
			allowed[fmt.Sprintf("urnetwork-evaluation-%s-%s-%02d.slice", token, role, index)] = true
		}
		allowed[fmt.Sprintf("urnetwork-evaluation-%s-live-%02d.slice", strings.ReplaceAll(plan.JobId, "-", ""), index)] = true
	}
	for _, role := range []string{"baseline", "candidate"} {
		allowed["urnetwork-evaluation-"+token+"-"+role+"-score.slice"] = true
	}
	allowed["urnetwork-evaluation-"+token+"-preflight.slice"] = true
	seen := map[string]bool{}
	buildCount := 0
	for _, name := range plan.CgroupNames {
		build := cleanupBuildSlicePattern.MatchString(name)
		if seen[name] || !build && !allowed[name] {
			return errors.New("run cleanup contains an unauthorized cgroup")
		}
		seen[name] = true
		if build {
			buildCount++
		}
	}
	if buildCount != 1 || len(seen) != len(allowed)+1 {
		return errors.New("run cleanup requires its one build and complete full-R9 stage inventory")
	}
	for name := range allowed {
		if !seen[name] {
			return errors.New("run cleanup omitted an exact full-R9 stage cgroup")
		}
	}
	return nil
}

// Polling owns the same bounded cleanup lifetime; accepting cgroup.kill alone
// cannot free a slot while descendants still retain resources or mount handles.
func (self *hostCleaner) joinCgroup(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		empty, err := self.cgroups.Empty(ctx, name)
		if err != nil {
			return err
		}
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.CleanupPollInterval):
		}
	}
}

// Both labels are mandatory filters. Short ids, duplicates and excess results
// fail before any inspect or deletion, even if a daemon ignores the filters.
func (self *hostCleaner) list(ctx context.Context, plan cleanupPlan, network bool) ([]string, error) {
	args := []string{"container", "ls", "--all", "--quiet", "--no-trunc"}
	if network {
		args = []string{"network", "ls", "--quiet", "--no-trunc"}
	}
	args = append(args, "--filter", "label="+cleanupJobLabel+"="+plan.JobId, "--filter", "label="+cleanupRoundLabel+"="+plan.RoundId)
	content, err := self.docker(ctx, args)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range strings.Fields(string(content)) {
		if !digestPattern.MatchString(id) || seen[id] || len(ids) >= self.settings.CleanupObjectCount {
			return nil, errors.New("Docker cleanup inventory has invalid, duplicate or excess ids")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// Docker's field casing is an external contract. Partial decoding accepts its
// other report fields but never case-aliases the ownership fields being checked.
func (self *hostCleaner) inspect(ctx context.Context, plan cleanupPlan, id string, network bool, allowed map[string]bool) error {
	args := []string{"container", "inspect", "--", id}
	if network {
		args = []string{"network", "inspect", "--", id}
	}
	content, err := self.docker(ctx, args)
	if err != nil {
		return err
	}
	var reports []map[string]json.RawMessage
	if json.Unmarshal(content, &reports) != nil || len(reports) != 1 {
		return errors.New("Docker cleanup inspect report is malformed")
	}
	var inspectedId string
	if json.Unmarshal(reports[0]["Id"], &inspectedId) != nil || inspectedId != id || !digestPattern.MatchString(inspectedId) {
		return errors.New("Docker cleanup inspect identity differs from the exact target")
	}
	labelObject := reports[0]
	if !network {
		if json.Unmarshal(reports[0]["Config"], &labelObject) != nil {
			return errors.New("Docker cleanup has no container labels")
		}
	}
	var labels map[string]string
	if json.Unmarshal(labelObject["Labels"], &labels) != nil || labels[cleanupJobLabel] != plan.JobId || labels[cleanupRoundLabel] != plan.RoundId || labels[cleanupRunLabel] != plan.RunId {
		return errors.New("Docker cleanup target does not carry all three exact execution labels")
	}
	if !network {
		var host map[string]json.RawMessage
		var parent string
		if json.Unmarshal(reports[0]["HostConfig"], &host) != nil || json.Unmarshal(host["CgroupParent"], &parent) != nil || !allowed[parent] {
			return errors.New("Docker cleanup container escaped its exact authorized cgroup")
		}
	}
	return nil
}

// Every daemon operation gets its own timeout inside the original overall one.
func (self *hostCleaner) docker(ctx context.Context, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	commandCtx, cancel := context.WithTimeout(ctx, self.settings.CleanupCommandTimeout)
	defer cancel()
	content, err := self.command(commandCtx, args)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > self.settings.DiagnosticBytes {
		return nil, errors.New("Docker cleanup response exceeds its bound")
	}
	return content, commandCtx.Err()
}

// The pinned binary and explicit Unix socket are the only daemon authority.
// An isolated empty config path and minimal environment exclude ambient Docker
// contexts, credential helpers, registries, plugin paths and DOCKER_* settings.
func (self *hostCleaner) runDocker(ctx context.Context, args []string) ([]byte, error) {
	if err := validatePin(self.config.Docker, true); err != nil {
		return nil, err
	}
	socket := self.config.DockerSocketPath
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || socket == "/" {
		return nil, errors.New("invalid fixed Docker socket")
	}
	if err := protectedRootPath(filepath.Dir(socket), true, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0002 != 0 {
		return nil, errors.New("Docker socket is not the protected fixed daemon")
	}
	emptyConfig := filepath.Join(self.config.StateDirectory, "cleanup-docker-empty")
	if info, err := os.Lstat(emptyConfig); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("cleanup Docker config path is not private")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("cleanup Docker config path is not protected")
		}
		directory, err := os.Open(emptyConfig)
		if err != nil {
			return nil, err
		}
		entries, readErr := directory.Readdirnames(1)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) != 0 {
			return nil, errors.New("cleanup Docker config must be empty")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	arguments := append([]string{"--host", "unix://" + socket, "--config", emptyConfig}, args...)
	command := exec.CommandContext(ctx, self.config.Docker.Path, arguments...)
	command.Env = controlEnvironment()
	output := &boundedOutput{maximum: self.settings.DiagnosticBytes}
	command.Stdout, command.Stderr = output, &discardWriter{}
	if err := command.Run(); err != nil {
		return nil, errors.New("fixed-daemon cleanup command failed")
	}
	if output.exceeded {
		return nil, errors.New("fixed-daemon cleanup output exceeds its bound")
	}
	return append([]byte(nil), output.Bytes()...), nil
}

// The cgroup v2 filesystem does not permit symlinks; protected ancestor checks
// and descriptor confinement also reject synthetic or replaced path components.
type hostCleanupCgroups struct {
	rootPath            string
	maximumBytes        int64
	requireCgroupFs     bool
	limitCommandTimeout time.Duration
	limitCommand        func(context.Context, []string) error
	managerBoundary     Pin
	managerMaximumBytes int64
	limitReadCommand    func(context.Context, []string) ([]byte, error)
}

// A systemd slice expands into exact parent slice components, never a prefix
// glob or recursive enumeration of unrelated jobs under urnetwork.slice.
func cleanupSlicePath(name string) (string, error) {
	if !cleanupBuildSlicePattern.MatchString(name) && !cleanupEvaluationSlicePattern.MatchString(name) {
		return "", errors.New("invalid exact cleanup slice")
	}
	atoms := strings.Split(strings.TrimSuffix(name, ".slice"), "-")
	parts := make([]string, 0, len(atoms))
	for index, atom := range atoms {
		if atom == "" || !namePattern.MatchString(atom) {
			return "", errors.New("invalid cleanup slice component")
		}
		parts = append(parts, strings.Join(atoms[:index+1], "-")+".slice")
	}
	return filepath.Join(parts...), nil
}

// All property access is descriptor-confined to a verified cgroup v2 root.
func (self *hostCleanupCgroups) open(name, property string, flags int) (*os.File, error) {
	admissionLimit := property != "cgroup.kill" && property != "cgroup.events" && property != "cgroup.freeze"
	return self.openWithLimitAuthority(name, property, flags, admissionLimit)
}

// Before manager mutation even the exact census file must prove exclusive
// root authority, without depending on controllers that are not enabled yet.
func (self *hostCleanupCgroups) openWithLimitAuthority(name, property string, flags int, admissionLimit bool) (*os.File, error) {
	if property != "cgroup.kill" && property != "cgroup.events" && property != "cgroup.freeze" &&
		property != "memory.max" && property != "memory.swap.max" && property != "pids.max" && property != "cpu.max" &&
		property != "cpuset.cpus" && property != "cpuset.cpus.effective" &&
		property != "memory.current" && property != "memory.swap.current" && property != "memory.stat" {
		return nil, errors.New("unsupported exact cgroup cleanup property")
	}
	relative, err := cleanupSlicePath(name)
	if err != nil {
		return nil, err
	}
	root, err := openDirectoryNoFollow(self.rootPath)
	if err != nil {
		return nil, errors.New("cleanup cgroup hierarchy is unavailable")
	}
	defer root.Close()
	if self.requireCgroupFs {
		var stat syscall.Statfs_t
		err = syscall.Fstatfs(int(root.Fd()), &stat)
		info, infoErr := root.Stat()
		const cgroup2SuperMagic = 0x63677270
		if err != nil || stat.Type != cgroup2SuperMagic {
			return nil, errors.New("cleanup cgroup root is not cgroup v2")
		}
		if admissionLimit && (infoErr != nil || !rootOwnedContainmentNode(info)) {
			return nil, errors.New("containment limit hierarchy is not exclusively root-owned")
		}
	}
	current := ""
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		directory, err := openRelativeNoFollow(root, current, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, errCleanupCgroupAbsent
			}
			return nil, err
		}
		info, statErr := directory.Stat()
		closeErr := directory.Close()
		if statErr != nil || closeErr != nil || !info.IsDir() {
			return nil, errors.New("cleanup cgroup ancestor is not an exact directory")
		}
		if self.requireCgroupFs && admissionLimit && !rootOwnedContainmentNode(info) {
			return nil, errors.New("containment limit parent is delegated or writable outside root")
		}
	}
	file, err := openRelativeNoFollow(root, filepath.Join(relative, property), flags, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			directory, checkErr := openRelativeNoFollow(root, relative, os.O_RDONLY|syscall.O_DIRECTORY, 0)
			if directory != nil {
				_ = directory.Close()
			}
			if errors.Is(checkErr, os.ErrNotExist) {
				return nil, errCleanupCgroupAbsent
			}
			return nil, errors.New("required cgroup cleanup property is unavailable")
		}
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("cleanup cgroup property is not regular")
	}
	if self.requireCgroupFs && admissionLimit && !rootOwnedContainmentNode(info) {
		file.Close()
		return nil, errors.New("containment limit property is delegated or writable outside root")
	}
	return file, nil
}

// An absent exact cgroup is already empty; an unavailable hierarchy is not.
func (self *hostCleanupCgroups) Kill(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := self.open(name, "cgroup.kill", os.O_WRONLY)
	if errors.Is(err, errCleanupCgroupAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	count, err := file.Write([]byte("1\n"))
	if err != nil {
		return err
	}
	if count != 2 {
		return io.ErrShortWrite
	}
	return nil
}

// Prepared parent slices remain frozen until the independent Docker owner has
// acknowledged quiescence. An absent parent cannot contain a late daemon start.
func (self *hostCleanupCgroups) Freeze(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := self.open(name, "cgroup.freeze", os.O_WRONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	count, err := file.Write([]byte("1\n"))
	if err != nil {
		return err
	}
	if count != 2 {
		return io.ErrShortWrite
	}
	return nil
}

// Writing freeze requests suspension; only the kernel's finite frozen reply
// acknowledges it. Unlike Empty, a missing slice never counts as frozen.
func (self *hostCleanupCgroups) Frozen(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	file, err := self.open(name, "cgroup.events", os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if self.maximumBytes <= 0 {
		return false, errors.New("invalid frozen cgroup read bound")
	}
	content, err := io.ReadAll(io.LimitReader(file, self.maximumBytes+1))
	if err != nil || int64(len(content)) > self.maximumBytes {
		return false, errors.New("frozen cgroup state exceeds its bound")
	}
	seen, frozen := false, false
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return false, errors.New("malformed frozen cgroup state")
		}
		if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
			return false, errors.New("nonfinite frozen cgroup counter")
		}
		if fields[0] == "frozen" {
			if seen || fields[1] != "0" && fields[1] != "1" {
				return false, errors.New("invalid cgroup frozen value")
			}
			seen, frozen = true, fields[1] == "1"
		}
	}
	if !seen {
		return false, errors.New("cgroup state omitted its frozen acknowledgment")
	}
	return frozen, nil
}

// Populated is parsed exactly once and must be a finite boolean kernel value.
func (self *hostCleanupCgroups) Empty(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	file, err := self.open(name, "cgroup.events", os.O_RDONLY)
	if errors.Is(err, errCleanupCgroupAbsent) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if self.maximumBytes <= 0 {
		return false, errors.New("invalid cgroup cleanup read bound")
	}
	content, err := io.ReadAll(io.LimitReader(file, self.maximumBytes+1))
	if err != nil || int64(len(content)) > self.maximumBytes {
		return false, errors.New("cgroup cleanup state exceeds its bound")
	}
	seen := false
	empty := false
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return false, errors.New("malformed cgroup cleanup state")
		}
		if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
			return false, errors.New("nonfinite cgroup cleanup counter")
		}
		if fields[0] == "populated" {
			if seen || fields[1] != "0" && fields[1] != "1" {
				return false, errors.New("invalid cgroup population value")
			}
			seen, empty = true, fields[1] == "0"
		}
	}
	if !seen {
		return false, errors.New("cgroup cleanup omitted population state")
	}
	return empty, nil
}

// Mount identity comes from procfs, then from the opened descriptor's mount id.
// The unmount target is that pinned descriptor, not a raceable path string.
type hostCleanupMounts struct {
	config           Config
	settings         AgentSettings
	readMounts       func(context.Context) ([]byte, error)
	openParent       func(cleanupPlan) (*os.File, error)
	beforeParentOpen func(cleanupPlan)
	mountId          func(*os.File) (uint64, error)
	unmount          func(context.Context, string, int) error
}

// No mount or filesystem mutation occurs during construction.
func newHostCleanupMounts(config Config, settings AgentSettings) *hostCleanupMounts {
	self := &hostCleanupMounts{config: config, settings: settings}
	self.readMounts = func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return readBounded("/proc/self/mountinfo", settings.DiagnosticBytes)
	}
	self.openParent = func(plan cleanupPlan) (*os.File, error) {
		root, err := openDirectoryNoFollow(config.WorkDirectory)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		original, err := os.Lstat(fmt.Sprintf("/proc/self/fd/%d/%s", root.Fd(), plan.RunId))
		if err != nil || !original.IsDir() || original.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("evidence cleanup parent is not an exact directory")
		}
		if self.beforeParentOpen != nil {
			self.beforeParentOpen(plan)
		}
		parent, err := openRelativeNoFollow(root, plan.RunId, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
		child, err := openRelativeNoFollow(parent, ".evidence-runtime", os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			_ = parent.Close()
			return nil, err
		}
		if err := child.Close(); err != nil {
			_ = parent.Close()
			return nil, err
		}
		info, err := parent.Stat()
		if err != nil || !os.SameFile(original, info) {
			parent.Close()
			return nil, errors.New("evidence cleanup parent changed during descriptor capture")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || plan.WorkDeviceId == 0 || uint64(stat.Dev) != plan.WorkDeviceId {
			parent.Close()
			return nil, errors.New("evidence parent moved off the pinned data filesystem")
		}
		return parent, nil
	}
	self.mountId = func(file *os.File) (uint64, error) {
		content, err := readBounded(fmt.Sprintf("/proc/self/fdinfo/%d", file.Fd()), settings.DiagnosticBytes)
		if err != nil {
			return 0, err
		}
		var id uint64
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "mnt_id:" {
				if id != 0 {
					return 0, errors.New("evidence descriptor has ambiguous mount id")
				}
				id, err = strconv.ParseUint(fields[1], 10, 64)
				if err != nil || id == 0 {
					return 0, errors.New("evidence descriptor has invalid mount id")
				}
			}
		}
		if id == 0 {
			return 0, errors.New("evidence descriptor omitted mount id")
		}
		return id, nil
	}
	self.unmount = func(ctx context.Context, target string, flags int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return syscall.Unmount(target, flags)
	}
	return self
}

// A parent descriptor pins confinement on the data filesystem. A short-lived
// child descriptor proves mount id, then closes before non-lazy unmount so our
// own descriptor cannot keep the tmpfs busy. Only the fixed leaf is resolved.
func (self *hostCleanupMounts) Unmount(ctx context.Context, plan cleanupPlan) error {
	if err := validateCleanupPlan(plan, self.config); err != nil {
		return err
	}
	entry, err := self.exactMount(ctx, plan)
	if err != nil || entry == nil {
		return err
	}
	parent, err := self.openParent(plan)
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := syscall.Openat(int(parent.Fd()), ".evidence-runtime", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	child := os.NewFile(uintptr(fd), "cleanup-evidence")
	id, err := self.mountId(child)
	closeErr := child.Close()
	if err != nil || id != entry.id {
		return errors.New("evidence mount changed before descriptor capture")
	}
	if closeErr != nil {
		return closeErr
	}
	current, err := self.exactMount(ctx, plan)
	if err != nil || current == nil || current.id != entry.id {
		return errors.New("evidence mount changed before unmount")
	}
	const umountNoFollow = 8
	if err := self.unmount(ctx, fmt.Sprintf("/proc/self/fd/%d/.evidence-runtime", parent.Fd()), umountNoFollow); err != nil {
		return err
	}
	remaining, err := self.exactMount(ctx, plan)
	if err != nil {
		return err
	}
	if remaining != nil {
		return errors.New("evidence mount remains after non-lazy unmount")
	}
	return nil
}

// Minimal mountinfo fields needed for exact scope; numeric ids are finite.
type cleanupMountEntry struct {
	id                         uint64
	target, filesystem, source string
}

// The global mount inventory is byte-bounded. A nested mount always refuses
// cleanup, even when there is no matching parent tmpfs at the requested target.
func (self *hostCleanupMounts) exactMount(ctx context.Context, plan cleanupPlan) (*cleanupMountEntry, error) {
	content, err := self.readMounts(ctx)
	if err != nil {
		return nil, err
	}
	if self.settings.DiagnosticBytes <= 0 || int64(len(content)) > self.settings.DiagnosticBytes {
		return nil, errors.New("mount inventory exceeds its bound")
	}
	reader := bufio.NewScanner(bytes.NewReader(content))
	reader.Buffer(make([]byte, len(content)+1), len(content)+1)
	var found *cleanupMountEntry
	for reader.Scan() {
		fields := strings.Fields(reader.Text())
		if len(fields) < 10 {
			return nil, errors.New("malformed cleanup mount inventory")
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || len(fields) < separator+4 {
			return nil, errors.New("malformed cleanup mount filesystem identity")
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid cleanup mount id")
		}
		target, err := decodeMountField(fields[4])
		if err != nil || !filepath.IsAbs(target) || filepath.Clean(target) != target {
			return nil, errors.New("invalid cleanup mount target")
		}
		if target != plan.EvidenceDirectory && pathWithin(plan.EvidenceDirectory, target) {
			return nil, errors.New("foreign nested evidence mount prevents cleanup")
		}
		if target != plan.EvidenceDirectory {
			continue
		}
		source, err := decodeMountField(fields[separator+2])
		if err != nil || found != nil || fields[separator+1] != "tmpfs" || source != plan.EvidenceSource || fields[3] != "/" {
			return nil, errors.New("evidence cleanup mount is not the exact owned tmpfs")
		}
		found = &cleanupMountEntry{id: id, target: target, filesystem: fields[separator+1], source: source}
	}
	if err := reader.Err(); err != nil {
		return nil, errors.New("mount inventory line exceeds its bound")
	}
	return found, nil
}

// Only Linux mountinfo's four documented path escapes are accepted.
func decodeMountField(value string) (string, error) {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			result.WriteByte(value[index])
			continue
		}
		if len(value)-index < 4 {
			return "", errors.New("truncated mount path escape")
		}
		switch value[index : index+4] {
		case `\040`:
			result.WriteByte(' ')
		case `\011`:
			result.WriteByte('\t')
		case `\012`:
			result.WriteByte('\n')
		case `\134`:
			result.WriteByte('\\')
		default:
			return "", errors.New("unsupported mount path escape")
		}
		index += 3
	}
	return result.String(), nil
}
