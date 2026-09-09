// The API's existing client JWT authenticates an account; a separately owned
// real-chain cache authenticates VPK staging eligibility. Neither credential
// can substitute for the other, and no operator/MinIO secret reaches validators.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/validator"
	"github.com/urnetwork/server/model"
)

// Discovery references and deployment/runtime hashes are non-secret approved
// provisioning inputs. No listed VPK is admitted without the actual readers.
type StReservedAttemptUploadConfig struct {
	Admission     validator.ValidatorUploadAdmissionConfig `json:"admission" yaml:"admission"`
	Budget        model.StReservedAttemptUploadBudget      `json:"budget" yaml:"budget"`
	NativeRPCURLs []string                                 `json:"native_rpc_urls" yaml:"native_rpc_urls"`
}

// The strict scalar walk runs before generic decoding can truncate a float,
// expand an alias, or ignore a misspelled nested authority/capacity field.
func (self *StReservedAttemptUploadConfig) UnmarshalYAML(node *yaml.Node) error {
	if self == nil || node == nil {
		return errors.New("reserved upload configuration is absent")
	}
	stack := []*yaml.Node{node}
	visited := 0
	for len(stack) != 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		visited++
		if item == nil || visited > 32768 || len(item.Content) > 32768 || len(item.Value) > 1024*1024 || item.Kind == yaml.AliasNode || item.Anchor != "" {
			return errors.New("reserved upload YAML owner is aliased or exceeds its bound")
		}
		if item.Kind == yaml.ScalarNode {
			if item.ShortTag() == "!!float" || item.ShortTag() == "!!null" {
				return errors.New("reserved upload YAML requires explicit exact values")
			}
			if item.ShortTag() == "!!int" {
				value, err := strconv.ParseUint(item.Value, 10, 64)
				if err != nil || strconv.FormatUint(value, 10) != item.Value {
					return errors.New("reserved upload YAML integer is not canonical uint64 decimal")
				}
			}
		}
		if len(stack)+len(item.Content) > 32768 {
			return errors.New("reserved upload YAML tree exceeds its node bound")
		}
		stack = append(stack, item.Content...)
	}
	encoded, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	if len(encoded) > 2*1024*1024 {
		return errors.New("reserved upload YAML exceeds its byte bound")
	}
	type plain StReservedAttemptUploadConfig
	var value plain
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("reserved upload YAML has trailing documents")
	}
	*self = StReservedAttemptUploadConfig(value)
	return nil
}

// Own all reference/endpoint slices before callbacks or startup can run.
func (self *StReservedAttemptUploadConfig) clone() *StReservedAttemptUploadConfig {
	if self == nil {
		return nil
	}
	result := *self
	result.NativeRPCURLs = slices.Clone(self.NativeRPCURLs)
	result.Admission.ActivationContexts = slices.Clone(self.Admission.ActivationContexts)
	return &result
}

// Global account quotas have no access to this finite owner allowance. The
// complete product of admitted owners and reserved capacity is also bounded.
func (self *StReservedAttemptUploadConfig) Validate(cfg *StConfig) error {
	if self == nil || cfg == nil || !cfg.Enabled || cfg.DeploymentKey() == "" {
		return errors.New("reserved upload configured deployment is unavailable")
	}
	if err := errors.Join(self.Admission.Deployment.Validate(), self.ValidateCapacity()); err != nil {
		return err
	}
	domain := self.Admission.Deployment
	if domain.ChainID != cfg.ChainId || domain.GenesisHash != cfg.GenesisHash || uint64(domain.Netuid) != cfg.Netuid || common.Address(domain.Coordinator) != cfg.ContractAddress ||
		common.Address(domain.SettlementVault) != cfg.SettlementVault || domain.DeploymentIDHash != sha256.Sum256([]byte(cfg.DeploymentId)) || self.Admission.ReplicaNoID != cfg.NoId {
		return errors.New("reserved upload authority differs from the exact operator deployment")
	}
	return nil
}

// A setup template may approve finite capacity before generated deployment
// pins exist. This method alone never authorizes runtime staging or an owner.
func (self *StReservedAttemptUploadConfig) ValidateCapacity() error {
	if self == nil {
		return errors.New("reserved upload capacity is absent")
	}
	if err := errors.Join(self.Admission.ValidateCapacity(), self.Budget.Validate()); err != nil {
		return err
	}
	for _, value := range []uint64{self.Budget.RetryRequestsPerHour, self.Budget.ObjectsPerHour, self.Budget.BytesPerHour} {
		if value > 9007199254740991/self.Admission.MaximumOwners {
			return errors.New("reserved upload complete owner reservation exceeds exact accounting")
		}
	}
	if len(self.NativeRPCURLs) == 0 || len(self.NativeRPCURLs) > 8 {
		return errors.New("reserved upload native provider census is invalid")
	}
	seen := make(map[string]bool, len(self.NativeRPCURLs))
	for _, endpoint := range self.NativeRPCURLs {
		parsed, err := url.Parse(endpoint)
		if err != nil || endpoint == "" || len(endpoint) > 2048 || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.String() != endpoint || seen[endpoint] {
			return errors.New("reserved upload native provider identity is invalid")
		}
		if parsed.Scheme != "https" && parsed.Scheme != "wss" {
			address := net.ParseIP(parsed.Hostname())
			if (parsed.Scheme != "http" && parsed.Scheme != "ws") || address == nil || !address.IsLoopback() {
				return errors.New("reserved upload native provider requires TLS or literal loopback")
			}
		}
		seen[endpoint] = true
	}
	return nil
}

// One production API lifecycle owns its cache, RPC clients and finite quota
// policy. Close joins the cache before either underlying transport is closed.
type StReservedAttemptUpload struct {
	admission   *validator.ValidatorUploadAdmission
	chain       *validator.ChainClient
	native      *crv4.Chain
	deployment  model.StDeploymentKey
	replicaNoID uint64
	budget      model.StReservedAttemptUploadBudget
	closeOnce   sync.Once
}

// Optional omission keeps old planning/API profiles readable but enables no
// reserved staging. A supplied malformed profile is an error, not a fallback.
func NewStReservedAttemptUpload(ctx context.Context) (*StReservedAttemptUpload, error) {
	return NewStReservedAttemptUploadWithConfig(ctx, stConfig())
}

// Explicit API construction owns a copied deployment configuration and real
// chain clients; it accepts no eligibility callback or pre-authorized entry.
func NewStReservedAttemptUploadWithConfig(ctx context.Context, cfg *StConfig) (*StReservedAttemptUpload, error) {
	if ctx == nil {
		return nil, errors.New("reserved upload lifecycle context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil || cfg.ReservedAttemptUpload == nil {
		return nil, nil
	}
	config := cfg.ReservedAttemptUpload.clone()
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(config.Admission.MaximumRefreshSeconds)*time.Second)
	defer cancel()
	chain, err := validator.DialReleaseChainContext(dialCtx, slices.Clone(cfg.RpcUrls), cfg.ContractAddress)
	if err != nil {
		return nil, err
	}
	var native *crv4.Chain
	var nativeErr error
	for _, endpoint := range config.NativeRPCURLs {
		if dialCtx.Err() != nil {
			break
		}
		native, nativeErr = crv4.DialChainContext(dialCtx, endpoint)
		if nativeErr == nil {
			break
		}
	}
	if native == nil || nativeErr != nil || dialCtx.Err() != nil {
		if native != nil {
			native.API.Client.Close()
		}
		chain.Close()
		return nil, errors.Join(errors.New("reserved upload native initialization failed"), nativeErr, dialCtx.Err())
	}
	admission, err := validator.NewValidatorUploadAdmission(ctx, chain, native, config.Admission)
	if err != nil {
		native.API.Client.Close()
		chain.Close()
		return nil, err
	}
	return &StReservedAttemptUpload{admission: admission, chain: chain, native: native, deployment: cfg.DeploymentKey(), replicaNoID: cfg.NoId, budget: config.Budget}, nil
}

// Readiness waits for an actual complete authority refresh; constructing the
// lifecycle alone never grants admission to an upload or fixture.
func (self *StReservedAttemptUpload) WaitReady(ctx context.Context) error {
	if self == nil || self.admission == nil {
		return errors.New("reserved upload lifecycle is unavailable")
	}
	return self.admission.WaitReady(ctx)
}

// No account identity or mutable JWT value selects the protected quota owner.
// Only the actual client-auth wrapper may call this with its accepted bearer.
func (self *StReservedAttemptUpload) Begin(ctx context.Context, credential, header, kind string, contentHash [32]byte, size uint64) (*validator.ValidatorUploadLease, error) {
	if self == nil || self.admission == nil {
		return nil, errors.New("503 Validator reserved upload is unavailable.")
	}
	sessionHash, err := protocol.ValidatorAttemptUploadSessionHash(credential)
	if err != nil {
		return nil, fmt.Errorf("401 Invalid upload client session: %w", err)
	}
	objectKind := byte(0)
	switch kind {
	case "metadata":
		objectKind = 1
	case "records":
		objectKind = 2
	case "proofs":
		objectKind = 3
	default:
		return nil, errors.New("400 Invalid reserved object kind.")
	}
	lease, err := self.admission.Begin(ctx, header, sessionHash, objectKind, contentHash, size)
	if err != nil {
		return nil, fmt.Errorf("403 Validator staging authority refused: %w", err)
	}
	return lease, nil
}

// Redis atomically decides whether this is a new object before selecting the
// independent fresh or duplicate active pool. Unknown outcomes remain errors;
// they are never retried or transformed into a successful acknowledgement.
func (self *StReservedAttemptUpload) Reserve(lease *validator.ValidatorUploadLease) error {
	if self == nil || self.admission == nil || lease == nil {
		return errors.New("503 Validator reservation owner is unavailable.")
	}
	if err := self.admission.ValidateLease(lease); err != nil {
		return err
	}
	intent, owner := lease.Intent(), lease.Owner()
	object, err := intent.ObjectReservationHash()
	if err != nil {
		return err
	}
	fresh, err := model.ReserveStReservedAttemptUpload(lease.Context(), self.deployment, self.replicaNoID,
		model.StReservedAttemptUploadOwner{Hotkey: owner.Hotkey, OperatorNoID: owner.OperatorNoID}, object, intent.Size, intent.NotAfter, self.admission.MaximumIntentSeconds(), self.budget)
	if err != nil {
		return err
	}
	return lease.Start(fresh)
}

// Leases finish actual body/storage cleanup before the cache owner joins.
func (self *StReservedAttemptUpload) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() { self.admission.Close(); self.native.API.Client.Close(); self.chain.Close() })
}
