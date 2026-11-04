// The agent has an independent, root-provisioned role. It never loads server
// settings, a production vault, a database connection, or coordinator secrets.
package competitionagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A host-installed executable is selected only by its protected configuration.
type Pin struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// This document contains only local fixture and subevaluation-role identities.
// WorkDirectory is a root-owned searchable parent; individual runs are 0700 and
// owned by WorkerUser. StateDirectory and SigningKeyPath stay root-only.
type Config struct {
	Schema                     int              `json:"schema"`
	HostId                     string           `json:"host_id"`
	BaseDirectory              string           `json:"base_directory"`
	TrustedOperatorUser        string           `json:"trusted_operator_user"`
	ExpectedDataDeviceId       uint64           `json:"expected_data_device_id"`
	StateDirectory             string           `json:"state_directory"`
	WorkDirectory              string           `json:"work_directory"`
	WorkerUser                 string           `json:"worker_user"`
	Evaluator                  Pin              `json:"evaluator"`
	SelfCheck                  Pin              `json:"self_check"`
	HostCheckConfiguration     Pin              `json:"host_check_configuration"`
	Agent                      Pin              `json:"agent"`
	Docker                     Pin              `json:"docker"`
	DockerSocketPath           string           `json:"docker_socket_path"`
	DockerRole                 DockerRoleConfig `json:"docker_role"`
	SubmissionBuilderIdentity  Pin              `json:"submission_builder_identity"`
	SubmissionBuilderSha256    string           `json:"submission_builder_sha256"`
	SubevaluationOwnership     Pin              `json:"subevaluation_ownership"`
	JsonFormatter              Pin              `json:"json_formatter"`
	ConfigLocalDirectory       string           `json:"config_local_directory"`
	VaultLocalDirectory        string           `json:"vault_local_directory"`
	ConfigLocalSha256          string           `json:"config_local_sha256"`
	VaultLocalSha256           string           `json:"vault_local_sha256"`
	EvaluatorImageDigest       string           `json:"evaluator_image_digest"`
	SourceLockSha256           string           `json:"source_lock_sha256"`
	QualificationSha256        string           `json:"qualification_sha256"`
	CoordinatorPublicKeyBase64 string           `json:"coordinator_public_key_base64"`
	SigningKeyPath             string           `json:"signing_key_path"`
	Limits                     Limits           `json:"limits"`
	configPath                 string
}

// All scheduling and diagnostic bounds belong to an agent instance. Nonpositive
// values are invalid; Now is a test seam and is never accepted from a peer.
type AgentSettings struct {
	ControlTimeout            time.Duration
	SelfCheckTimeout          time.Duration
	CancelGrace               time.Duration
	LockPollInterval          time.Duration
	MaximumClockSkew          time.Duration
	DiagnosticBytes           int64
	ConfigBytes               int64
	DirectoryBatch            int
	InputTransferTimeout      time.Duration
	CleanupTimeout            time.Duration
	CleanupCommandTimeout     time.Duration
	CleanupPollInterval       time.Duration
	CleanupObjectCount        int
	OwnerPollInterval         time.Duration
	ManagementMemoryBytes     int64
	ManagementCpuQuotaPercent int
	// Build tasks include the retained BuildKit daemon and all of its children;
	// 4096 is an independent parent ceiling. Nonpositive values refuse launch.
	BuildTasksMaximum int64
	// The 100000-microsecond kernel quota period bounds each exact CPU role.
	// Values outside the cgroup v2 period range refuse launch.
	ContainmentCpuPeriodMicros int64
	Now                        func() time.Time
}

// Control calls are short; only the detached systemd service owns evaluation.
func DefaultAgentSettings() AgentSettings {
	return AgentSettings{ControlTimeout: 30 * time.Second, SelfCheckTimeout: 2 * time.Minute,
		CancelGrace: 10 * time.Second, LockPollInterval: 25 * time.Millisecond,
		MaximumClockSkew: time.Minute, DiagnosticBytes: 4 * 1024 * 1024,
		ConfigBytes: 64 * 1024, DirectoryBatch: 128, InputTransferTimeout: 2 * time.Minute, CleanupTimeout: 2 * time.Minute,
		CleanupCommandTimeout: 10 * time.Second, CleanupPollInterval: 25 * time.Millisecond, CleanupObjectCount: 1024,
		OwnerPollInterval: 25 * time.Millisecond, ManagementMemoryBytes: 8 * 1024 * 1024 * 1024, ManagementCpuQuotaPercent: 200,
		BuildTasksMaximum: 4096, ContainmentCpuPeriodMicros: 100000, Now: time.Now}
}

// Only an explicitly pinned path may select the role; unknown fields fail closed.
func LoadConfig(path string) (*Config, error) {
	settings := DefaultAgentSettings()
	if err := protectedRootPath(path, false, true); err != nil {
		return nil, err
	}
	content, err := readBounded(path, settings.ConfigBytes)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	config := &Config{Limits: DefaultLimits(), configPath: path}
	if err := decodeStrict(content, config); err != nil {
		return nil, err
	}
	if err := config.validate(true); err != nil {
		return nil, err
	}
	return config, nil
}

// Public construction never bypasses protected path or non-root worker checks.
func (self *Config) validate(protected bool) error {
	if self == nil || self.Schema != Schema || !namePattern.MatchString(self.HostId) || self.WorkerUser == "" || self.TrustedOperatorUser == "" || self.ExpectedDataDeviceId == 0 ||
		!digestPattern.MatchString(self.SourceLockSha256) || !digestPattern.MatchString(self.QualificationSha256) ||
		!digestPattern.MatchString(self.SubmissionBuilderSha256) ||
		!digestPattern.MatchString(self.ConfigLocalSha256) || !digestPattern.MatchString(self.VaultLocalSha256) ||
		!strings.HasPrefix(self.EvaluatorImageDigest, "sha256:") || !digestPattern.MatchString(strings.TrimPrefix(self.EvaluatorImageDigest, "sha256:")) {
		return errors.New("invalid subevaluation agent configuration")
	}
	if err := self.Limits.Validate(); err != nil {
		return err
	}
	if self.Limits.MaximumJobDuration > DefaultLimits().MaximumJobDuration {
		return errors.New("subevaluation agent cannot expand the three-hour job budget")
	}
	if !filepath.IsAbs(self.BaseDirectory) || filepath.Clean(self.BaseDirectory) != self.BaseDirectory || self.BaseDirectory == "/" {
		return errors.New("agent runtime base must be canonical, absolute and non-root")
	}
	if self.StateDirectory == "" {
		self.StateDirectory = filepath.Join(self.BaseDirectory, "state")
	}
	if self.WorkDirectory == "" {
		self.WorkDirectory = filepath.Join(self.BaseDirectory, "jobs")
	}
	if self.StateDirectory == self.BaseDirectory || self.WorkDirectory == self.BaseDirectory || !pathWithin(self.BaseDirectory, self.StateDirectory) || !pathWithin(self.BaseDirectory, self.WorkDirectory) {
		return errors.New("agent state and jobs must remain beneath its explicit runtime base")
	}
	var operatorUid uint32
	if protected {
		operator, err := user.Lookup(self.TrustedOperatorUser)
		if err != nil {
			return err
		}
		uid, err := strconv.ParseUint(operator.Uid, 10, 32)
		if err != nil {
			return errors.New("invalid trusted runtime operator identity")
		}
		operatorUid = uint32(uid)
		if err := protectedRuntimePath(self.BaseDirectory, self.BaseDirectory, operatorUid); err != nil {
			return err
		}
		if err := self.requireDataMount(); err != nil {
			return err
		}
	}
	for _, path := range []string{self.StateDirectory, self.WorkDirectory, self.ConfigLocalDirectory, self.VaultLocalDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("subevaluation directories must be canonical absolute non-root paths")
		}
		if protected {
			var pathErr error
			if pathWithin(self.BaseDirectory, path) {
				pathErr = protectedRuntimePath(path, self.BaseDirectory, operatorUid)
			} else {
				pathErr = protectedRootPath(path, true, false)
			}
			if pathErr != nil {
				return pathErr
			}
		}
	}
	if filepath.Base(self.ConfigLocalDirectory) != "local" || filepath.Base(self.VaultLocalDirectory) != "local" {
		return errors.New("agent fixture mounts must be direct config/local and vault/local directories")
	}
	directories := []string{self.StateDirectory, self.WorkDirectory, self.ConfigLocalDirectory, self.VaultLocalDirectory}
	for i, parent := range directories {
		for j, path := range directories {
			if i != j && pathWithin(parent, path) {
				return errors.New("agent state, work and local fixture directories must be independent")
			}
		}
	}
	for _, pin := range []Pin{self.Evaluator, self.SelfCheck, self.Agent, self.Docker, self.SubmissionBuilderIdentity, self.SubevaluationOwnership, self.JsonFormatter} {
		if err := validatePin(pin, protected); err != nil {
			return err
		}
	}
	if protected {
		if err := validateFilePin(self.HostCheckConfiguration, DefaultAgentSettings().ConfigBytes, true); err != nil {
			return errors.New("private host check requires its own protected configuration pin")
		}
	}
	if err := self.DockerRole.validate(self, protected); err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(self.CoordinatorPublicKeyBase64)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("invalid independent coordinator public key")
	}
	if protected {
		// The prospective role uses only the independently qualified private
		// engine. The public helper metadata never exposes that root endpoint.
		if err := protectedRootPath(self.configPath, false, true); err != nil {
			return err
		}
		if err := protectedRootPath(self.SigningKeyPath, false, true); err != nil {
			return err
		}
		info, err := os.Lstat(self.StateDirectory)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			return errors.New("agent state must be root-only")
		}
		// A scoped search ACL need not set the world-execute bit. NewAgent
		// probes this protected path and all ancestors as the actual worker
		// before it creates state; mode bits alone cannot establish ACL access.
		if _, err := workerCredential(self.WorkerUser); err != nil {
			return err
		}
	}
	return nil
}

// A missing bind mount must fail before creating state on the smaller root
// filesystem. The expected device is explicitly provisioned, never discovered
// and silently trusted from the path currently occupying the runtime base.
func (self *Config) requireDataMount() error {
	baseInfo, err := os.Stat(self.BaseDirectory)
	if err != nil {
		return err
	}
	rootInfo, err := os.Stat("/")
	if err != nil {
		return err
	}
	baseStat, baseOk := baseInfo.Sys().(*syscall.Stat_t)
	rootStat, rootOk := rootInfo.Sys().(*syscall.Stat_t)
	if !baseOk || !rootOk || uint64(baseStat.Dev) != self.ExpectedDataDeviceId || baseStat.Dev == rootStat.Dev {
		return errors.New("runtime data mount is missing or differs from its explicit non-root filesystem pin")
	}
	return nil
}

// Runtime leaves stay root-owned. Only ancestors above the configured base may
// belong to the explicitly trusted operator; no home directory is chowned.
func protectedRuntimePath(path, base string, operatorUid uint32) error {
	if !pathWithin(base, path) {
		return errors.New("runtime path escaped its configured base")
	}
	inside := true
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || inside && stat.Uid != 0 || !inside && stat.Uid != 0 && stat.Uid != operatorUid {
			return errors.New("runtime authority or trusted ancestor is writable or untrusted")
		}
		if current == base {
			inside = false
		}
		if current == "/" {
			return nil
		}
	}
}

// Rejects either direction of containment without confusing sibling prefixes.
func pathWithin(parent, path string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Every ancestor is protected, not merely an apparently safe leaf. Private
// files may not be read by the evaluator user or an SSH session user.
func protectedRootPath(path string, directory, private bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("agent authority path must be canonical, absolute and non-root")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 ||
			(current != path || directory) && !info.IsDir() || current == path && !directory && !info.Mode().IsRegular() ||
			current == path && private && info.Mode().Perm()&0077 != 0 {
			return errors.New("agent authority path or ancestor is not protected root ownership")
		}
		if current == "/" {
			return nil
		}
	}
}

// Executable bytes are checked again immediately before every invocation.
func validatePin(pin Pin, protected bool) error {
	if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || !digestPattern.MatchString(pin.Sha256) {
		return errors.New("invalid subevaluation executable pin")
	}
	if protected {
		if err := protectedRootPath(pin.Path, false, false); err != nil {
			return err
		}
	}
	file, err := openRegularNoFollow(pin.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm()&0111 == 0 {
		return errors.New("subevaluation executable is not executable")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != pin.Sha256 {
		return errors.New("subevaluation executable digest mismatch")
	}
	return nil
}

// Group membership belongs to the installed unprivileged evaluation account.
func workerCredential(name string) (*syscall.Credential, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 {
		return nil, errors.New("evaluation worker must be an explicit non-root account")
	}
	groupIds, err := account.GroupIds()
	if err != nil {
		return nil, err
	}
	groups := make([]uint32, 0, len(groupIds))
	for _, groupId := range groupIds {
		group, err := strconv.ParseUint(groupId, 10, 32)
		if err != nil {
			return nil, err
		}
		groups = append(groups, uint32(group))
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, nil
}

// This decoder does not expose raw content in errors or diagnostic logs.
func decodeStrict(content []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid strict agent document")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("agent document has trailing data")
	}
	return nil
}

// Bounded regular-file reads exclude devices, pipes, symlinks and growth.
func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || maximum <= 0 || info.Size() > maximum {
		return nil, errors.New("agent file exceeds its bound")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		clear(content)
		return nil, errors.New("agent file exceeds its bound")
	}
	return content, nil
}
