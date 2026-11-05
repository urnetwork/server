//go:build linux

// Forced SSH transports bytes only into a PID1-owned bounded control service.
// Its temporary session clock is unrelated to a signed evaluation's original
// attempt/deadline. Canceling transport never stops the durable evaluator.
package competitionagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// An unacknowledged PID1 start/stop is not an inactive service. Its closed
// admission receipt remains inspectable and is never replayed or silently gc'd.
var ErrManagedSshUnknown = errors.New("managed SSH control-service outcome remains unknown")

// All session budgets are owned by one wrapper. Child caps are inside the
// existing private aggregate, never another allowance outside the reserve.
type ManagementSshSettings struct {
	Agent           AgentSettings
	MemoryBytes     int64         // Per-control child maximum; default 256 MiB requires separate native peak proof.
	TasksMaximum    int64         // Per-control tasks; default 128 includes bounded native control helpers.
	MaximumControls int           // Retained unknown sessions; a full registry refuses new admission.
	MaximumRuntime  time.Duration // Full healthy RPC allowance, not a shorter framing or evaluation clock.
	JoinTimeout     time.Duration // Finite stop/manager/client join owner after transport withdrawal.
	PollInterval    time.Duration // Positive rate limit while monitoring the exact ephemeral unit.
}

// Framing/transfer/cleanup each retain their full allowance in addition to the
// largest signed job lifetime. These defaults do not assert native readiness.
func DefaultManagementSshSettings() ManagementSshSettings {
	agent := DefaultAgentSettings()
	maximum := DefaultLimits().MaximumJobDuration + agent.ControlTimeout + agent.SelfCheckTimeout + agent.InputTransferTimeout + agent.OutputTransferTimeout + agent.CleanupTimeout + agent.CancelGrace
	return ManagementSshSettings{Agent: agent, MemoryBytes: 256 * 1024 * 1024, TasksMaximum: 128, MaximumControls: 128,
		MaximumRuntime: maximum, JoinTimeout: agent.ControlTimeout + agent.ControlTimeout + agent.CancelGrace, PollInterval: agent.OwnerPollInterval}
}

// No peer can choose the unit, marker, executable, properties or clock.
type managementSshPlan struct {
	Schema           int           `json:"schema"`
	Id               string        `json:"id"`
	UnitName         string        `json:"unit_name"`
	ConfigSha256     string        `json:"config_sha256"`
	Agent            Pin           `json:"agent"`
	CreatedAt        time.Time     `json:"created_at"`
	ExpiresAt        time.Time     `json:"expires_at"`
	RuntimeMaximum   time.Duration `json:"runtime_maximum"`
	MemoryBytes      int64         `json:"memory_bytes"`
	TasksMaximum     int64         `json:"tasks_maximum"`
	ManagementCpuSet string        `json:"management_cpuset"`
}

// These are audit facts, not cached permission for another manager operation.
type managementSshClosed struct {
	Schema       int    `json:"schema"`
	Id           string `json:"id"`
	PlanSha256   string `json:"plan_sha256"`
	Outcome      string `json:"outcome"`
	InvocationId string `json:"invocation_id"`
}

// The process owns only the pinned systemd-run monitor and inherited copies
// of the SSH descriptors, never the independently launched evaluator tree.
type managementSshProcess interface {
	Wait() error
	Kill() error
}

// Native implementations are installed at construction. Private tests replace
// these owners without granting an alternate native root or runtime waiver.
type managementSshHost struct {
	settings           ManagementSshSettings
	protected          bool
	load               func(string) (*Config, error)
	admit              func(context.Context, *Config) (*ManagementResourcePolicy, Pin, error)
	start              func(context.Context, *Config, managementSshPlan, string, []*os.File) (managementSshProcess, error)
	observe            func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error)
	stop               func(context.Context, Pin, string) error
	empty              func(context.Context, string) (bool, error)
	newId              func() (string, error)
	joinContext        func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	beforeJoinedReturn func()
}

// The public entrypoint accepts only physical inherited descriptors. Nothing
// outside the placed child reads a request or starts a Go stream-copy worker.
func ServeManagedSsh(ctx context.Context, configPath string, input io.Reader, output, diagnostic io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("managed SSH requires the fixed root wrapper")
	}
	return newManagementSshHost(DefaultManagementSshSettings()).serve(ctx, configPath, input, output, diagnostic)
}

// Direct --serve is a PID1 child role, not a bypass around the forced wrapper.
// Full agent/worker/engine admission happens only after this narrow proof.
func VerifyManagedControlPlacement(ctx context.Context, configPath string) error {
	if os.Geteuid() != 0 {
		return errors.New("managed control requires the fixed root service")
	}
	self := newManagementSshHost(DefaultManagementSshSettings())
	config, err := self.load(configPath)
	if err != nil {
		return err
	}
	policy, manager, err := self.admit(ctx, config)
	if err != nil {
		return err
	}
	return verifyManagementSshCurrent(ctx, config, policy, manager, self.settings)
}

// The lifecycle is externally driven so the SSH parent joins all of its own
// process/descriptor work before returning, including cancellation orderings.
func (self *managementSshHost) serve(parent context.Context, configPath string, input io.Reader, output, diagnostic io.Writer) error {
	if self == nil || self.settings.validate() != nil || self.load == nil || self.admit == nil || self.start == nil || self.observe == nil || self.stop == nil || self.empty == nil || self.newId == nil || self.joinContext == nil {
		return errors.New("managed SSH has no finite concrete owner")
	}
	files, err := captureManagementSshFiles(input, output, diagnostic)
	if err != nil {
		return err
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	if err := parent.Err(); err != nil {
		return err
	}
	config, err := self.load(configPath)
	if err != nil {
		return err
	}
	policy, manager, err := self.admit(parent, config)
	if err != nil {
		return err
	}
	id, err := self.newId()
	if err != nil || !digestPattern.MatchString(id) {
		return errors.New("managed SSH could not allocate its private control identity")
	}
	now := self.settings.Agent.Now().UTC()
	plan := managementSshPlan{Schema: Schema, Id: id, UnitName: managementSshUnitName(id), ConfigSha256: config.configSha256, Agent: config.Agent,
		CreatedAt: now, ExpiresAt: now.Add(self.settings.MaximumRuntime), RuntimeMaximum: self.settings.MaximumRuntime,
		MemoryBytes: self.settings.MemoryBytes, TasksMaximum: self.settings.TasksMaximum, ManagementCpuSet: policy.ManagementCpuSet}
	if err := plan.validate(*config, *policy, self.settings); err != nil {
		return err
	}
	registry, err := openManagementSshRegistry(*config, self.settings, self.protected)
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := registry.create(parent, plan); err != nil {
		return err
	}
	prior, priorErr := self.observe(parent, config, policy, manager, plan)
	if priorErr != nil || prior.exists {
		withdrawErr := registry.withdraw(plan.Id)
		retireErr := registry.retire(plan.Id)
		return errors.Join(errors.New("managed SSH nonce already has manager custody or cannot be checked"), withdrawErr, retireErr)
	}
	marker := filepath.Join(registry.path, id+".admit")
	monitorCtx, monitorCancel := context.WithDeadline(context.WithoutCancel(parent), plan.ExpiresAt)
	defer monitorCancel()
	process, err := self.start(monitorCtx, config, plan, marker, files)
	if err != nil {
		// Start failure means no child executable was issued. A started
		// monitor's lost acknowledgement instead arrives through Wait.
		withdrawErr := registry.withdraw(plan.Id)
		retireErr := registry.retire(plan.Id)
		return errors.Join(err, withdrawErr, retireErr)
	}
	if nilControlStream(process) {
		withdrawErr := registry.withdraw(plan.Id)
		closeErr := registry.close(plan, "unknown", "")
		return errors.Join(ErrManagedSshUnknown, errors.New("managed SSH lost its issued monitor owner"), withdrawErr, closeErr)
	}
	waited := make(chan error, 1)
	go func() { waited <- process.Wait() }()
	var waitErr error
	waitJoined := false
	seen, invocation, stopping, stopAcknowledged := false, "", false, false
	generationUnknown := false
	var outcomeErr error
	ctx := parent
	var finish context.CancelFunc
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	beginStop := func(cause error) {
		if stopping {
			if cause != nil && !errors.Is(outcomeErr, cause) {
				outcomeErr = errors.Join(outcomeErr, cause)
			}
			return
		}
		stopping, outcomeErr = true, cause
		if err := registry.withdraw(plan.Id); err != nil {
			outcomeErr = errors.Join(outcomeErr, err)
		}
		ctx, finish = self.joinContext(context.WithoutCancel(parent), self.settings.JoinTimeout)
	}
	for {
		if err := parent.Err(); err != nil {
			beginStop(err)
		}
		if monitorCtx.Err() != nil {
			beginStop(monitorCtx.Err())
		}
		unit, observeErr := self.observe(ctx, config, policy, manager, plan)
		if observeErr == nil && unit.exists {
			if invocation != "" && unit.invocationId != invocation {
				if !generationUnknown {
					generationUnknown = true
					beginStop(errors.New("managed control invocation changed"))
				}
			} else {
				seen, invocation = true, unit.invocationId
				if unit.completed {
					beginStop(nil)
				}
			}
		}
		if stopping {
			if !stopAcknowledged {
				stopAcknowledged = self.stop(ctx, manager, plan.UnitName) == nil
			}
			if seen && stopAcknowledged {
				after, afterErr := self.observe(ctx, config, policy, manager, plan)
				empty, emptyErr := self.empty(ctx, plan.UnitName)
				if afterErr == nil && (!after.exists || after.inactive && after.invocationId == invocation) && emptyErr == nil && empty {
					// Never wait for --wait before issuing the stop which
					// lets its RemainAfterExit service become inactive.
					if !waitJoined {
						select {
						case waitErr = <-waited:
							waitJoined = true
						case <-ctx.Done():
						}
					}
					if waitJoined {
						if self.beforeJoinedReturn != nil {
							self.beforeJoinedReturn()
						}
						if generationUnknown {
							closeErr := registry.close(plan, "unknown", invocation)
							return errors.Join(ErrManagedSshUnknown, outcomeErr, waitErr, closeErr)
						}
						if err := registry.close(plan, "joined", invocation); err != nil {
							return err
						}
						if err := registry.retire(plan.Id); err != nil {
							return err
						}
						return errors.Join(outcomeErr, waitErr)
					}
				}
			}
		}
		if stopping && ctx.Err() != nil {
			if !waitJoined {
				_ = process.Kill()
				waitErr = <-waited
			}
			closeErr := registry.close(plan, "unknown", invocation)
			return errors.Join(ErrManagedSshUnknown, outcomeErr, waitErr, ctx.Err(), closeErr)
		}
		if waitJoined {
			beginStop(errors.New("control monitor ended without a confirmed unit join"))
		} else if stopping {
			select {
			case waitErr = <-waited:
				waitJoined = true
			case <-ctx.Done():
			case <-time.After(self.settings.PollInterval):
			}
		} else {
			select {
			case waitErr = <-waited:
				waitJoined = true
				beginStop(waitErr)
			case <-parent.Done():
				beginStop(parent.Err())
			case <-monitorCtx.Done():
				beginStop(monitorCtx.Err())
			case <-time.After(self.settings.PollInterval):
			}
		}
		if waitJoined {
			select {
			case <-ctx.Done():
			case <-time.After(self.settings.PollInterval):
			}
		}
	}
}

// Only the exact nonce-derived service is ever managed by this owner.
func managementSshUnitName(id string) string { return "urnetwork-subeval-control-" + id + ".service" }

// Positive values cannot exceed their existing private aggregate or truncate
// the configured healthy request owners. Integer overflow refuses admission.
func (self ManagementSshSettings) validate() error {
	profile := DefaultManagementBoundarySettings()
	minimum := DefaultLimits().MaximumJobDuration
	for _, allowance := range []time.Duration{self.Agent.ControlTimeout, self.Agent.SelfCheckTimeout, self.Agent.InputTransferTimeout, self.Agent.OutputTransferTimeout, self.Agent.CleanupTimeout, self.Agent.CancelGrace} {
		if allowance <= 0 || minimum+allowance <= minimum {
			return errors.New("managed SSH request lifetime is unbounded or overflowed")
		}
		minimum += allowance
	}
	if self.MemoryBytes <= 0 || self.MemoryBytes > profile.PrivateMemoryBytes || self.TasksMaximum <= 0 || self.TasksMaximum > profile.PrivateTasksMaximum ||
		self.MaximumControls <= 0 || self.MaximumControls > (int(^uint(0)>>1)-1)/4 || self.MaximumRuntime < minimum || self.JoinTimeout <= self.Agent.CancelGrace ||
		self.PollInterval <= 0 || self.Agent.Now == nil || self.Agent.ConfigBytes <= 0 || self.Agent.DiagnosticBytes <= 0 || self.Agent.ManagementCpuQuotaPercent <= 0 ||
		self.Agent.ConfigBytes == math.MaxInt64 || int64(self.Agent.ManagementCpuQuotaPercent) > math.MaxInt64/1000000 ||
		self.Agent.ContainmentCpuPeriodMicros < 1000 || self.Agent.ContainmentCpuPeriodMicros > 1000000 || self.Agent.CleanupObjectCount <= 0 || self.Agent.MaximumClockSkew <= 0 {
		return errors.New("managed SSH has invalid child caps or finite owner settings")
	}
	return nil
}

// Cleanup can validate an expired session; expiry never proves a process join.
func (self managementSshPlan) validate(config Config, policy ManagementResourcePolicy, settings ManagementSshSettings) error {
	if settings.validate() != nil || ValidateManagementResourcePolicy(policy, DefaultManagementBoundarySettings()) != nil || self.Schema != Schema || !digestPattern.MatchString(self.Id) ||
		self.UnitName != managementSshUnitName(self.Id) || !digestPattern.MatchString(self.ConfigSha256) || self.ConfigSha256 != config.configSha256 || self.Agent != config.Agent || self.CreatedAt.IsZero() ||
		self.ExpiresAt.Sub(self.CreatedAt) != self.RuntimeMaximum || self.RuntimeMaximum != settings.MaximumRuntime || self.MemoryBytes != settings.MemoryBytes || self.TasksMaximum != settings.TasksMaximum ||
		validateRuntimeBoundaryManagementCpuSet(self.ManagementCpuSet, policy.ManagementCpuSet) != nil {
		return errors.New("managed SSH receipt differs from its protected role or original session lifetime")
	}
	return nil
}

// No goroutine copies data outside the placed unit. Duplicating a pipe/socket
// does not consume bytes or close the caller's descriptor on normal return.
func captureManagementSshFiles(input io.Reader, output, diagnostic io.Writer) ([]*os.File, error) {
	files := []*os.File{}
	for index, stream := range []any{input, output, diagnostic} {
		file, ok := stream.(*os.File)
		if !ok || file == nil {
			for _, held := range files {
				_ = held.Close()
			}
			return nil, errors.New("managed SSH requires inherited physical pipe or socket descriptors")
		}
		fd, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_DUPFD_CLOEXEC, 0)
		if errno != 0 {
			for _, held := range files {
				_ = held.Close()
			}
			return nil, errors.New("managed SSH could not capture its inherited descriptors")
		}
		owned := os.NewFile(fd, "managed-ssh-inherited-stream")
		files = append(files, owned)
		info, statErr := owned.Stat()
		flags, _, flagErr := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		mode := flags & syscall.O_ACCMODE
		if statErr != nil || info.Mode()&os.ModeNamedPipe == 0 && info.Mode()&os.ModeSocket == 0 || flagErr != 0 ||
			index == 0 && mode != syscall.O_RDONLY && mode != syscall.O_RDWR || index != 0 && mode != syscall.O_WRONLY && mode != syscall.O_RDWR {
			for _, held := range files {
				_ = held.Close()
			}
			return nil, errors.New("managed SSH descriptor has the wrong physical stream direction")
		}
	}
	return files, nil
}

// Registry metadata is bounded, root-owned and outside candidate-writable
// storage. Unknown receipts retain custody; healthy joined receipts may be gc'd.
type managementSshRegistry struct {
	directory *os.File
	path      string
	settings  ManagementSshSettings
	protected bool
}

// The protected config has already admitted its independent journal root.
func openManagementSshRegistry(config Config, settings ManagementSshSettings, protected bool) (*managementSshRegistry, error) {
	root, err := openDirectoryNoFollow("/")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	state, err := openRelativeNoFollow(root, strings.TrimPrefix(config.StateDirectory, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	if err := mkdirRelativeNoFollow(state, "managed-controls", 0700); err != nil {
		return nil, err
	}
	directory, err := openRelativeNoFollow(state, "managed-controls", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	info, err := directory.Stat()
	stat, ok := func() (*syscall.Stat_t, bool) {
		if info == nil {
			return nil, false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return stat, ok
	}()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !ok || protected && stat.Uid != 0 {
		directory.Close()
		return nil, errors.New("managed SSH registry is not private root custody")
	}
	return &managementSshRegistry{directory: directory, path: filepath.Join(config.StateDirectory, "managed-controls"), settings: settings, protected: protected}, nil
}

// Ownership is synchronous; there is no registry cleanup goroutine to abandon.
func (self *managementSshRegistry) Close() error { return self.directory.Close() }

// Only exact regular, single-link root metadata is read through held parents.
func (self *managementSshRegistry) read(name string) ([]byte, error) {
	file, err := openRelativeNoFollow(self.directory, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() > self.settings.Agent.ConfigBytes {
		return nil, errors.New("managed SSH metadata is not a bounded sealed regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || self.protected && stat.Uid != 0 {
		return nil, errors.New("managed SSH metadata has ambiguous root custody")
	}
	content, err := io.ReadAll(io.LimitReader(file, self.settings.Agent.ConfigBytes+1))
	if err != nil || int64(len(content)) > self.settings.Agent.ConfigBytes {
		return nil, errors.New("managed SSH metadata exceeded its bound")
	}
	return content, nil
}

// New leaves are exclusive and fsynced before any corresponding external issue.
func (self *managementSshRegistry) write(name string, content []byte) error {
	if int64(len(content)) > self.settings.Agent.ConfigBytes {
		return errors.New("managed SSH metadata exceeded its write bound")
	}
	file, err := openRelativeNoFollow(self.directory, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return err
	}
	defer file.Close()
	if count, err := file.Write(content); err != nil || count != len(content) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return self.directory.Sync()
}

// The cross-process lock covers metadata only, never blocking manager I/O.
func (self *managementSshRegistry) lock(ctx context.Context) (*os.File, error) {
	file, err := openRelativeNoFollow(self.directory, "registry.lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		file.Close()
		return nil, errors.New("managed SSH registry lock is not a protected regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || self.protected && stat.Uid != 0 {
		file.Close()
		return nil, errors.New("managed SSH registry lock has ambiguous root custody")
	}
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// A bounded census prevents uncertain launches from filling the trusted disk.
func (self *managementSshRegistry) create(ctx context.Context, plan managementSshPlan) error {
	lock, err := self.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	reader, err := openRelativeNoFollow(self.directory, ".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer reader.Close()
	maximum := self.settings.MaximumControls * 4
	entries, err := reader.ReadDir(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > maximum {
		return errors.New("managed SSH registry census exceeded its bound")
	}
	controls := 0
	for _, entry := range entries {
		if entry.Name() == "registry.lock" {
			continue
		}
		id, suffix, ok := strings.Cut(entry.Name(), ".")
		if !ok || !digestPattern.MatchString(id) || suffix != "json" && suffix != "admit" && suffix != "admitted" && suffix != "closed" || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return errors.New("managed SSH registry contains unrelated custody")
		}
		if suffix == "json" {
			controls++
		}
	}
	if controls >= self.settings.MaximumControls {
		return errors.New("managed SSH registry retains its maximum uncertain controls")
	}
	content, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := self.write(plan.Id+".json", content); err != nil {
		return err
	}
	return self.write(plan.Id+".admit", []byte(digestDockerBytes(content)+"\n"))
}

// Removing this exact leaf closes admission even if a manager start is late.
func (self *managementSshRegistry) withdraw(id string) error {
	if !digestPattern.MatchString(id) {
		return errors.New("invalid managed SSH withdrawal identity")
	}
	if err := syscall.Unlinkat(int(self.directory.Fd()), id+".admit"); err != nil && !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return self.directory.Sync()
}

// Unknown records are never consumed as proof and never authorize replay.
func (self *managementSshRegistry) close(plan managementSshPlan, outcome, invocation string) error {
	if outcome != "joined" && outcome != "unknown" {
		return errors.New("invalid managed SSH terminal observation")
	}
	content, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	closed, err := json.Marshal(managementSshClosed{Schema: Schema, Id: plan.Id, PlanSha256: digestDockerBytes(content), Outcome: outcome, InvocationId: invocation})
	if err != nil {
		return err
	}
	return self.write(plan.Id+".closed", closed)
}

// Only the caller's positively joined or never-issued exact nonce is removed.
// No prefix scan, recursive cleanup, expiry or remote input authorizes this.
func (self *managementSshRegistry) retire(id string) error {
	if !digestPattern.MatchString(id) {
		return errors.New("invalid managed SSH retirement identity")
	}
	for _, suffix := range []string{"admit", "admitted", "closed", "json"} {
		if err := syscall.Unlinkat(int(self.directory.Fd()), id+"."+suffix); err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
	}
	return self.directory.Sync()
}

// Nonces are generated locally; a reconnect never reuses or replays one.
func newManagementSshId() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// Manager formatting uses decimal seconds; there is no shell or interpolation.
func managementSshDuration(value time.Duration) string {
	return strconv.FormatFloat(value.Seconds(), 'f', 6, 64) + "s"
}
