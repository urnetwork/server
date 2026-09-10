// Authenticated ClientKey control writes become independently operator-signed
// durable registrations. A separate authenticated API read signs its actual
// current generation for one exact validator request, then publishes immutable
// evidence before returning. No endpoint accepts a caller-selected key to sign.
package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/stabi"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/startifact"
)

// A private per-operation owner retains all domain/key/routing inputs before
// the first real external read. Its shared concrete RPC pool owns connections.
type stClientKeyAuthorityOwner struct {
	domain       protocol.ClientKeyHistoryDomain
	deploymentID string
	rootKey      *ecdsa.PrivateKey
	artifactKey  *ecdsa.PrivateKey
	client       *CoreStClient
	rpcURLs      []string
	store        server.BlobStore
}

// Ordinary key registration never falls back to a legacy unsigned write when
// a configured release operator lacks its real signer, RPC owner or evidence.
func newStClientKeyAuthorityOwner() (*stClientKeyAuthorityOwner, error) {
	cfg := stConfig()
	client, ok := stClient().(*CoreStClient)
	if cfg == nil || !cfg.Enabled || cfg.Netuid == 0 || cfg.Netuid > math.MaxUint16 || !ok || client == nil || client.cfg == nil || client.cfg.ChainId != cfg.ChainId || client.cfg.ContractAddress != cfg.ContractAddress || len(cfg.RpcUrls) == 0 || len(cfg.RpcUrls) > 16 {
		return nil, errors.New("client-key authority requires the actual configured release operator and chain reader")
	}
	owner := &stClientKeyAuthorityOwner{
		domain:       protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: cfg.PolicyHash, NoID: cfg.NoId},
		deploymentID: cfg.DeploymentId, client: client, rpcURLs: slices.Clone(cfg.RpcUrls),
	}
	if err := owner.domain.Validate(); err != nil {
		return nil, err
	}
	for index, key := range []*ecdsa.PrivateKey{cfg.RootKey, cfg.ArtifactKey} {
		if key == nil || key.D == nil || key.Curve != crypto.S256() || key.X == nil || key.Y == nil || key.D.Sign() <= 0 || key.D.Cmp(crypto.S256().Params().N) >= 0 || !crypto.S256().IsOnCurve(key.X, key.Y) {
			return nil, errors.New("client-key authority signing owner is malformed")
		}
		owned, err := crypto.ToECDSA(crypto.FromECDSA(key))
		if err != nil || owned.X.Cmp(key.X) != 0 || owned.Y.Cmp(key.Y) != 0 {
			return nil, errors.Join(errors.New("client-key authority private/public signer differs"), err)
		}
		if index == 0 {
			owner.rootKey = owned
		} else {
			owner.artifactKey = owned
		}
	}
	if crypto.PubkeyToAddress(owner.rootKey.PublicKey) == crypto.PubkeyToAddress(owner.artifactKey.PublicKey) {
		return nil, errors.New("client-key root authority and artifact publication signers must remain distinct")
	}
	store, ok := server.LoadBlobStore()
	if !ok || store == nil {
		return nil, errors.New("client-key authority immutable evidence store is unavailable")
	}
	owner.store = store
	return owner, nil
}

// Uses actual raw RPC bytes at one EIP-1898 canonical hash. No mockable
// already-approved signer/boundary verdict enters the production path.
func readStClientKeyAuthorityAt(ctx context.Context, client *ethclient.Client, domain protocol.ClientKeyHistoryDomain, requested *protocol.ClientKeyEffectiveBoundary) (protocol.ClientKeyEffectiveBoundary, stabi.STCoordinatorOperatorVersion, error) {
	limits := stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: protocol.MaxClientKeyObservationBatchRpcRequests, MaximumMethods: protocol.MaxClientKeyObservationBatchRpcMethods, MaximumBytes: protocol.MaxClientKeyObservationBatchControlBytes}
	if requested == nil {
		observed, _, err := stabi.ReadCurrentClientKeyAuthorityContext(ctx, client, domain, limits)
		return observed.Query.Boundary, observed.Operator, err
	}
	observed, _, err := stabi.ReadClientKeyAuthoritiesContext(ctx, client, []stabi.ClientKeyAuthorityQuery{{Domain: domain, Boundary: *requested}}, limits)
	if err != nil {
		return protocol.ClientKeyEffectiveBoundary{}, stabi.STCoordinatorOperatorVersion{}, err
	}
	return observed[0].Query.Boundary, observed[0].Operator, nil
}

// Ordered failover restarts the complete read at an endpoint; it never splices
// one endpoint's finality or chain ID into another endpoint's root signer.
func (self *stClientKeyAuthorityOwner) readBoundary(ctx context.Context, requested *protocol.ClientKeyEffectiveBoundary) (protocol.ClientKeyEffectiveBoundary, stabi.STCoordinatorOperatorVersion, error) {
	if self == nil || self.client == nil || ctx == nil {
		return protocol.ClientKeyEffectiveBoundary{}, stabi.STCoordinatorOperatorVersion{}, errors.New("client-key chain owner is absent")
	}
	var failures []error
	for _, endpoint := range self.rpcURLs {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		client, err := self.client.client(callCtx, endpoint)
		if err != nil {
			cancel()
			failures = append(failures, err)
			continue
		}
		boundary, operator, err := readStClientKeyAuthorityAt(callCtx, client, self.domain, requested)
		cancel()
		if err == nil {
			return boundary, operator, ctx.Err()
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	return protocol.ClientKeyEffectiveBoundary{}, stabi.STCoordinatorOperatorVersion{}, errors.Join(errors.New("client-key boundary has no complete authenticated RPC observation"), errors.Join(failures...), ctx.Err())
}

// Called directly by the existing authenticated ClientKey control dispatch.
// Public publication failure does not discard the committed signed generation;
// the exact same bytes are retried by this API and by observation capture.
func StRegisterClientKey(ctx context.Context, clientID server.Id, publicKey []byte) error {
	if ctx == nil || clientID == (server.Id{}) || len(publicKey) != 0 && len(publicKey) != 32 {
		return errors.New("client-key registration context, identity or key length is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	publicKey = bytes.Clone(publicKey)
	owner, err := newStClientKeyAuthorityOwner()
	if err != nil {
		return err
	}
	operationCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
	defer cancel()
	reservation, err := owner.client.registrationCohorts().admit(operationCtx, owner)
	if err != nil {
		return err
	}
	defer reservation.release()
	boundary, operator, err := reservation.readBoundary(operationCtx)
	if err != nil || operator.RootSigner != crypto.PubkeyToAddress(owner.rootKey.PublicKey) {
		return errors.Join(errors.New("client-key registration signer differs from the actual operator version"), err)
	}
	record, err := model.StoreStClientKeyRegistration(operationCtx, model.StClientKeyRegistrationInput{Domain: owner.domain, DeploymentID: owner.deploymentID, ClientID: clientID, PublicKey: publicKey, Boundary: boundary, RootKey: owner.rootKey, ArtifactKey: owner.artifactKey, CreatedAt: server.NowUtc()})
	if err != nil {
		return err
	}
	err = reservation.publish(operationCtx, owner.store, record.EvidenceBytes, record.EvidenceHash)
	return errors.Join(err, operationCtx.Err())
}

// Client identity is separately parsed by the authenticated API. Request bytes
// encode only the fixed observation context, never a caller's proposed key.
type SnClientKeyObservationArgs struct {
	ClientID server.Id `json:"client_id"`
	Request  []byte    `json:"request"`
}

// These are exact complete existing-envelope bytes, not storage acknowledgments
// or a later mutable current-key response. The validator owns and replays them.
type SnClientKeyObservationResult = protocol.ClientKeyHistoryResponse

// Real validator sessions use this bounded read instead of today's unsigned
// key endpoint. The same retained bytes are public through /sn/evidence.
func SnClientKeyObservation(args *SnClientKeyObservationArgs, clientSession *session.ClientSession) (result *SnClientKeyObservationResult, resultErr error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil || clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil || *clientSession.ByJwt.ClientId == (server.Id{}) || args.ClientID == (server.Id{}) || len(args.Request) == 0 || len(args.Request) > protocol.MaxClientKeyStatementBytes {
		return nil, errors.New("client-key observation authenticated request is incomplete")
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, stCallTimeout)
	defer cancel()
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = nil
		}
	}()
	requestBytes, clientID := bytes.Clone(args.Request), args.ClientID
	var request protocol.ClientKeyObservationRequest
	decoder := json.NewDecoder(bytes.NewReader(requestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("client-key observation request has trailing JSON")
	}
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(canonical, requestBytes) || request.ClientID != [16]byte(clientID) {
		return nil, errors.Join(errors.New("client-key observation request is noncanonical or names another client"), err)
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	owner, err := newStClientKeyAuthorityOwner()
	if err != nil {
		return nil, err
	}
	_, operator, err := owner.readBoundary(ctx, &request.DecisionBoundary)
	if err != nil || operator.RootSigner != crypto.PubkeyToAddress(owner.rootKey.PublicKey) {
		return nil, errors.Join(errors.New("client-key observation signer differs from the exact decision operator"), err)
	}
	history, err := model.LoadStClientKeyHistory(ctx, owner.domain, clientID, model.MaxStClientKeyHistoryRegistrations, model.MaxStClientKeyHistoryBytes)
	if err != nil || len(history) == 0 {
		return nil, errors.Join(errors.New("client-key observation has no complete durable registration history"), err)
	}
	latest := history[len(history)-1].Registration
	if latest.EffectiveBoundary.Block > request.DecisionBoundary.Block || latest.EffectiveBoundary.Epoch > request.DecisionBoundary.Epoch || latest.EffectiveBoundary.Block == request.DecisionBoundary.Block && latest.EffectiveBoundary != request.DecisionBoundary {
		return nil, errors.New("current key registration follows the requested decision boundary")
	}
	result = &SnClientKeyObservationResult{History: make([][]byte, len(history))}
	for index, record := range history {
		_, priorOperator, err := owner.readBoundary(ctx, &record.Registration.EffectiveBoundary)
		if err != nil || priorOperator.RootSigner != record.Registration.Signer {
			return nil, errors.Join(errors.New("client-key history signer lacks actual historical operator authority"), err)
		}
		if _, err := startifact.PublishClientKeyEvidence(ctx, owner.store, record.EvidenceBytes, record.EvidenceHash); err != nil {
			return nil, err
		}
		result.History[index] = record.EvidenceBytes
	}
	registrationHash, err := latest.ContentHash()
	if err != nil {
		return nil, err
	}
	observation := protocol.ClientKeyObservation{Domain: owner.domain, ClientID: [16]byte(clientID), Generation: latest.Generation, RegistrationHash: registrationHash, Request: request}
	if err := protocol.SignClientKeyObservation(&observation, owner.rootKey); err != nil {
		return nil, err
	}
	encoded, contentHash, err := startifact.SealClientKeyObservationEvidence(owner.deploymentID, observation, owner.artifactKey, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if _, err := startifact.PublishClientKeyEvidence(ctx, owner.store, encoded, contentHash); err != nil {
		return nil, err
	}
	result.Observation = encoded
	return result, ctx.Err()
}
