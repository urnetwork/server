//go:build linux

// The prospective Docker role binds private daemon/quota authority and public
// non-root helper metadata to one immutable trusted evaluator installation.
package competitionagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/user"
	"path/filepath"
	"strconv"
)

// Writable owner scratch and worker jobs must occupy the separately verified
// bounded candidate filesystem. Journals and the private control socket do not.
type DockerRoleConfig struct {
	ScratchDirectory       string `json:"scratch_directory"`
	Compose                Pin    `json:"compose"`
	Buildx                 Pin    `json:"buildx"`
	Dockerfile             Pin    `json:"dockerfile"`
	PostgresInitialization Pin    `json:"postgres_initialization"`
	ResourcePolicy         Pin    `json:"resource_policy"`
	ResourceBoundary       Pin    `json:"resource_boundary"`
	StockSeccompProfile    Pin    `json:"stock_seccomp_profile"`
	CandidateCoreProfile   Pin    `json:"candidate_core_profile"`
}

// Root-pinned public metadata is compared, not generated from an SSH request.
// No private agent document is made worker-readable to configure Docker.
func (self DockerRoleConfig) validate(config *Config, protected bool) error {
	if !filepath.IsAbs(self.ScratchDirectory) || filepath.Clean(self.ScratchDirectory) != self.ScratchDirectory ||
		self.ScratchDirectory == config.BaseDirectory || !pathWithin(config.BaseDirectory, self.ScratchDirectory) {
		return errors.New("Docker owner scratch must be beneath the explicit runtime base")
	}
	for _, path := range []string{config.StateDirectory, config.WorkDirectory, config.ConfigLocalDirectory, config.VaultLocalDirectory} {
		if pathWithin(path, self.ScratchDirectory) || pathWithin(self.ScratchDirectory, path) {
			return errors.New("Docker owner scratch must be independent of authority, work and fixtures")
		}
	}
	for _, pin := range []Pin{self.Compose, self.Buildx, self.ResourcePolicy} {
		if err := validatePin(pin, protected); err != nil {
			return err
		}
	}
	for _, pin := range []Pin{self.Dockerfile, self.PostgresInitialization, self.ResourceBoundary} {
		if err := validateFilePin(pin, DefaultAgentSettings().ConfigBytes, protected); err != nil {
			return err
		}
	}
	if _, err := ValidateCandidateCoreProfiles(self.StockSeccompProfile, self.CandidateCoreProfile, DefaultCandidateCoreProfileSettings(), protected); err != nil {
		return err
	}
	if !filepath.IsAbs(config.DockerSocketPath) || filepath.Clean(config.DockerSocketPath) != config.DockerSocketPath || config.DockerSocketPath == "/" {
		return errors.New("private Docker endpoint must be canonical and absolute")
	}
	if !protected {
		return nil
	}
	if !pathWithin(config.BaseDirectory, config.DockerSocketPath) || pathWithin(config.StateDirectory, config.DockerSocketPath) ||
		pathWithin(config.WorkDirectory, config.DockerSocketPath) || pathWithin(self.ScratchDirectory, config.DockerSocketPath) {
		return errors.New("private Docker control endpoint must stay outside candidate and journal roots")
	}
	operatorUid, err := trustedOperatorUid(config.TrustedOperatorUser)
	if err != nil {
		return err
	}
	for _, path := range []string{self.ScratchDirectory, filepath.Join(config.BaseDirectory, "clients"), filepath.Dir(config.DockerSocketPath)} {
		if err := protectedRuntimePath(path, config.BaseDirectory, operatorUid); err != nil {
			return err
		}
	}
	public, err := loadDockerClientConfig(config.Agent.Path, defaultDockerClientSettings(), true)
	if err != nil {
		return err
	}
	worker, err := workerCredential(config.WorkerUser)
	if err != nil {
		return err
	}
	if public.Agent != config.Agent || public.Docker != config.Docker || public.Compose != self.Compose || public.BaseDirectory != config.BaseDirectory ||
		public.WorkDirectory != config.WorkDirectory || public.SocketDirectory != filepath.Join(config.BaseDirectory, "clients") ||
		public.WorkerUid != worker.Uid || public.TrustedOperatorUid != operatorUid {
		return errors.New("public Docker client metadata differs from the protected agent role")
	}
	return nil
}

// Non-executable trusted documents are bounded and hashed without treating a
// compiler Dockerfile or engine policy as an executable host capability.
func validateFilePin(pin Pin, maximum int64, protected bool) error {
	if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || !digestPattern.MatchString(pin.Sha256) {
		return errors.New("invalid trusted agent document pin")
	}
	if protected {
		if err := protectedRootPath(pin.Path, false, false); err != nil {
			return err
		}
	}
	content, err := readBounded(pin.Path, maximum)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != pin.Sha256 {
		return errors.New("trusted agent document content pin mismatch")
	}
	return nil
}

// Ancestor ownership is deliberately separate from worker credentials. The
// trusted operator's home remains unchanged and never becomes candidate-owned.
func trustedOperatorUid(name string) (uint32, error) {
	operator, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	uid, err := strconv.ParseUint(operator.Uid, 10, 32)
	if err != nil {
		return 0, errors.New("invalid trusted runtime operator identity")
	}
	return uint32(uid), nil
}

// Static declared ids are not live namespace proof. Admission separately
// verifies the complete native resource boundary, and output handoff requires
// an owned-container kernel probe matching this exact pinned declaration.
func (self DockerRoleConfig) namespaceMetadata(protected bool) (HostNamespaceMap, error) {
	maximum := DefaultAgentSettings().ConfigBytes
	if err := validateFilePin(self.ResourceBoundary, maximum, protected); err != nil {
		return HostNamespaceMap{}, err
	}
	content, err := readBounded(self.ResourceBoundary.Path, maximum)
	if err != nil {
		return HostNamespaceMap{}, err
	}
	var metadata struct {
		Schema               int    `json:"schema"`
		ContainerRootUid     uint32 `json:"container_root_uid"`
		ContainerRootGid     uint32 `json:"container_root_gid"`
		ContainerIdMapLength uint32 `json:"container_id_map_length"`
	}
	if validateDocumentKeys(content) != nil || json.Unmarshal(content, &metadata) != nil || metadata.Schema != Schema {
		return HostNamespaceMap{}, errors.New("private engine has no canonical declared namespace range")
	}
	return HostNamespaceMap{ContainerRootUid: metadata.ContainerRootUid, ContainerRootGid: metadata.ContainerRootGid, ContainerIdMapLength: metadata.ContainerIdMapLength}, nil
}
