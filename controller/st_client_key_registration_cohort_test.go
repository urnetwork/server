// Real authenticated Connect requests retain per-client Sql signatures and
// immutable content/history objects while sharing only a sealed authority read.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/connect"
	connectprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/startifact"
	"google.golang.org/protobuf/proto"
)

// Mutable fixture controls are synchronized, and only raw network responses
// can affect the actual stabi authority verifier.
type stClientKeyRegistrationRpcFixture struct {
	stateLock              sync.Mutex
	base                   stClientKeyHistoryRPCFixture
	boundaries             map[common.Hash]protocol.ClientKeyEffectiveBoundary
	requests               uint64
	methods                uint64
	finalizedReads         int
	canonicalReads         int
	beforeFinalizedForTest func(context.Context, int)
}

// Returns an immutable fixture snapshot; no lock crosses an external call.
func (self *stClientKeyRegistrationRpcFixture) snapshot() stClientKeyHistoryRPCFixture {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.base
}

// Actual identity methods are decoded by the real Json-Rpc server.
func (self *stClientKeyRegistrationRpcFixture) ChainId(ctx context.Context) (hexutil.Uint64, error) {
	base := self.snapshot()
	return base.ChainId(ctx)
}

// Native genesis is separately required even though Evm block zero is null.
func (self *stClientKeyRegistrationRpcFixture) GetBlockHash(ctx context.Context, block uint64) (*common.Hash, error) {
	base := self.snapshot()
	return base.GetBlockHash(ctx, block)
}

// A historical numbered/hash view remains available after the current head
// advances. This is not a current-head shortcut for old authority selectors.
func (self *stClientKeyRegistrationRpcFixture) GetBlockByNumber(ctx context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	if tag == rpc.FinalizedBlockNumber {
		self.stateLock.Lock()
		self.finalizedReads++
		index, before := self.finalizedReads, self.beforeFinalizedForTest
		self.stateLock.Unlock()
		if before != nil {
			before(ctx, index)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	base := self.snapshot()
	if tag != rpc.FinalizedBlockNumber && tag != 0 {
		self.stateLock.Lock()
		self.canonicalReads++
		if base.fault == "final-witness" && self.canonicalReads == 2 {
			base.fault = "canonical"
		}
		for _, boundary := range self.boundaries {
			if boundary.Block == uint64(tag) {
				base.boundary = boundary
				break
			}
		}
		self.stateLock.Unlock()
	}
	return base.GetBlockByNumber(ctx, tag, full)
}

// Actual generated Abi checks still require the exact contract and hash.
func (self *stClientKeyRegistrationRpcFixture) Call(ctx context.Context, call map[string]hexutil.Bytes, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if selector.BlockHash == nil {
		return nil, errors.New("registration view lacks its hash")
	}
	self.stateLock.Lock()
	base := self.base
	boundary, found := self.boundaries[*selector.BlockHash]
	self.stateLock.Unlock()
	if !found {
		return nil, errors.New("registration view names an unknown hash")
	}
	base.boundary = boundary
	return base.Call(ctx, call, selector)
}

// Counts real Http requests, including the cold connection's separate call.
func (self *stClientKeyRegistrationRpcFixture) counts() (uint64, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.requests, self.methods
}

// The existing concrete controller fixture owns Sql, keys and local storage.
// Only its raw network endpoint and private collection clock are selected here.
func newStClientKeyRegistrationFixture(t testing.TB, noId uint64, collection time.Duration) (*stClientKeyRegistrationRpcFixture, *jwt.ByJwt, *StConfig, *stClientKeyRegistrationCohorts) {
	t.Helper()
	base, credential, cfg := newStClientKeyHistoryControllerFixture(t)
	base.domain.NoID, cfg.NoId = noId, noId
	fixture := &stClientKeyRegistrationRpcFixture{base: *base, boundaries: map[common.Hash]protocol.ClientKeyEffectiveBoundary{common.Hash(base.boundary.Hash): base.boundary}}
	service := rpc.NewServer()
	if err := service.RegisterName("eth", fixture); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterName("chain", fixture); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024+1))
		_ = r.Body.Close()
		if err != nil || len(body) > 1024*1024 {
			http.Error(w, "invalid request", 400)
			return
		}
		count := 1
		if len(body) > 0 && body[0] == '[' {
			var members []json.RawMessage
			if json.Unmarshal(body, &members) != nil || len(members) == 0 || len(members) > stabi.MaxClientKeyAuthorityRpcBatchMembers {
				http.Error(w, "invalid batch", 400)
				return
			}
			count = len(members)
		}
		fixture.stateLock.Lock()
		fixture.requests++
		fixture.methods += uint64(count)
		fixture.stateLock.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		service.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { endpoint.Close(); service.Stop() })
	cfg.RpcUrls = []string{endpoint.URL}
	client := stClient().(*CoreStClient)
	if collection == stClientKeyRegistrationCollection {
		client.clientKeyRegistrations = nil
	} else {
		client.clientKeyRegistrations = newStClientKeyRegistrationCohorts(collection)
	}
	return fixture, credential, cfg, client.registrationCohorts()
}

// This is the actual production authentication wrapper and controller. It
// does not accept a fixture-provided authenticated client or storage verdict.
func stClientKeyRegistrationConnectEndpoint(t testing.TB) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/connect/control" {
			http.Error(w, "wrong route", 400)
			return
		}
		router.WrapWithInputRequireClient(ConnectControl, w, r)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// Every frame is an ordinary ClientKey protobuf, not a registration receipt.
func stClientKeyRegistrationRequest(ctx context.Context, endpoint, token string, key []byte) error {
	frame, err := connect.ToFrame(&connectprotocol.ClientKey{PublicKey: key}, connect.DefaultClientSettings().ProtocolVersion)
	if err != nil {
		return err
	}
	defer connect.MessagePoolReturn(frame.MessageBytes)
	pack, err := proto.Marshal(&connectprotocol.Pack{Frames: []*connectprotocol.Frame{frame}})
	if err != nil {
		return err
	}
	body, err := json.Marshal(ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/connect/control", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Connect status %d", response.StatusCode)
	}
	var result ConnectControlResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&result); err != nil {
		return err
	}
	if result.Error != nil {
		return errors.New(result.Error.Message)
	}
	return nil
}

// Original independently signed Sql bytes must match both immutable paths.
func stClientKeyRegistrationAssertStored(t testing.TB, fixture *stClientKeyRegistrationRpcFixture, cfg *StConfig, credential *jwt.ByJwt, key []byte, boundary protocol.ClientKeyEffectiveBoundary) model.StClientKeyHistoryRecord {
	t.Helper()
	base := fixture.snapshot()
	history, err := model.LoadStClientKeyHistory(t.Context(), base.domain, *credential.ClientId, model.MaxStClientKeyHistoryRegistrations, model.MaxStClientKeyHistoryBytes)
	if err != nil || len(history) != 1 {
		t.Fatalf("original registration census differs: %d %v", len(history), err)
	}
	record := history[0]
	registration := record.Registration
	if registration.Domain != base.domain || registration.ClientID != [16]byte(*credential.ClientId) || registration.NetworkID != [16]byte(credential.NetworkId) || registration.Generation != 1 || !registration.Present || !bytes.Equal(registration.PublicKey[:], key) || registration.EffectiveBoundary != boundary || registration.Signer != base.root {
		t.Fatal("stored registration is not this authenticated client's original authority")
	}
	if err := registration.VerifySignature(); err != nil {
		t.Fatal(err)
	}
	envelope, err := protocol.DecodeClientKeyEvidence(record.EvidenceBytes, base.domain, protocol.ClientKeyRegistrationEvidenceKind)
	if err != nil || !bytes.Equal(envelope.Payload, record.RegistrationBytes) || envelope.ContentHash != record.EvidenceHash || envelope.Signer != crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey) {
		t.Fatal("retained signed wrapper differs", err)
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("actual publication store is unavailable")
	}
	contentKey, err := startifact.EvidenceContentKey(store, record.EvidenceHash)
	if err != nil {
		t.Fatal(err)
	}
	historyKey, err := startifact.EvidenceHistoryKey(store, cfg.DeploymentId, base.domain.Netuid, startifact.ClientKeyRegistrationEvidenceKind, startifact.EvidenceDeploymentHistoryRunID, record.EvidenceHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{contentKey, historyKey} {
		reader, err := store.Get(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(io.LimitReader(reader, startifact.MaxClientKeyEvidenceBytes+1))
		err = errors.Join(err, reader.Close())
		if err != nil || !bytes.Equal(actual, record.EvidenceBytes) {
			t.Fatal("actual public original differs", path, err)
		}
	}
	return record
}

// Two genuine operator processes are represented by two separately owned
// concrete Core clients, each admitting 500 authenticated Connect requests.
// This forces a burst census, not a claim that real staggered startup has one.
func TestStClientKeyRegistrationCohortActualThousandClientsTwoOperators(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		started := time.Now()
		var authorityHttp, authorityMethods uint64
		for _, noId := range []uint64{1, 2} {
			fixture, credential, cfg, cohorts := newStClientKeyRegistrationFixture(tb, noId, stClientKeyRegistrationCollection)
			ctx, cancel := context.WithCancel(tb.Context())
			admitted, seal := make(chan struct{}, 500), make(chan struct{})
			cohorts.afterAdmissionForTest = func() { admitted <- struct{}{} }
			cohorts.beforeSealForTest = func(ctx context.Context) {
				select {
				case <-ctx.Done():
				case <-seal:
				}
			}
			endpoint := stClientKeyRegistrationConnectEndpoint(tb)
			var workers sync.WaitGroup
			tb.Cleanup(func() { cancel(); workers.Wait() })
			credentials := make([]*jwt.ByJwt, 500)
			tokens := make([]string, 500)
			keys := make([][]byte, 500)
			for index := range credentials {
				clientId, deviceId := server.NewId(), server.NewId()
				model.Testing_CreateDevice(tb.Context(), credential.NetworkId, deviceId, clientId, "registration-cohort", "test")
				credentials[index] = credential.Client(deviceId, clientId)
				tokens[index] = credentials[index].Sign()
				seed := sha256.Sum256(clientId[:])
				keys[index] = bytes.Clone(ed25519.NewKeyFromSeed(seed[:])[32:])
			}
			results := make(chan error, len(credentials))
			dispatchStarted := time.Now()
			workers.Add(len(credentials))
			for index := range credentials {
				go func(index int) {
					defer workers.Done()
					results <- stClientKeyRegistrationRequest(ctx, endpoint.URL, tokens[index], keys[index])
				}(index)
			}
			for range credentials {
				select {
				case <-admitted:
				case err := <-results:
					tb.Fatal("request ended before sealed population", err)
				case <-ctx.Done():
					tb.Fatal(ctx.Err())
				}
			}
			if requests, methods := fixture.counts(); requests != 0 || methods != 0 {
				tb.Fatal("authority started before population sealing")
			}
			cohorts.stateLock.Lock()
			exactPending := cohorts.slots == len(credentials) && cohorts.pending != nil && cohorts.pending.waiters == len(credentials) && cohorts.active == nil
			cohorts.stateLock.Unlock()
			if !exactPending {
				tb.Fatal("full authenticated pending census differs from its finite ownership count")
			}
			close(seal)
			for range credentials {
				select {
				case err := <-results:
					if err != nil {
						tb.Fatal(err)
					}
				case <-ctx.Done():
					tb.Fatal(ctx.Err())
				}
			}
			tb.Logf("actual operator=%d authenticated_members=500 Connect_dispatch_to_complete_Sql_and_publication_elapsed=%s", noId, time.Since(dispatchStarted))
			requests, methods := fixture.counts()
			if requests != 5 || methods != 21 {
				tb.Fatalf("operator%d actual authority+cold dial=%d/%d, want5/21", noId, requests, methods)
			}
			authorityHttp += requests - 1
			authorityMethods += methods - 1
			boundary := fixture.snapshot().boundary
			var original model.StClientKeyHistoryRecord
			for index := range credentials {
				record := stClientKeyRegistrationAssertStored(tb, fixture, cfg, credentials[index], keys[index], boundary)
				if index == 0 {
					original = record
				}
			}
			if err := stClientKeyRegistrationRequest(ctx, endpoint.URL, tokens[0], keys[0]); err != nil {
				tb.Fatal(err)
			}
			retried := stClientKeyRegistrationAssertStored(tb, fixture, cfg, credentials[0], keys[0], boundary)
			if !bytes.Equal(original.EvidenceBytes, retried.EvidenceBytes) || !bytes.Equal(original.RegistrationBytes, retried.RegistrationBytes) {
				tb.Fatal("same-key retry replaced original bytes")
			}
			if requests, methods := fixture.counts(); requests != 9 || methods != 41 {
				tb.Fatal("later retry reused completed boundary", requests, methods)
			}
			store, ok := server.LoadBlobStore()
			if !ok {
				tb.Fatal("actual original store disappeared")
			}
			objects, err := store.List(ctx, store.Prefix())
			if err != nil || len(objects) != 2*len(credentials) {
				tb.Fatal("complete original content/history object census differs", len(objects), err)
			}
			cohorts.stateLock.Lock()
			closed := cohorts.slots == 0 && cohorts.pending == nil && cohorts.active == nil
			cohorts.stateLock.Unlock()
			if !closed {
				tb.Fatal("registration ownership survived completed calls")
			}
			cancel()
		}
		if authorityHttp != 8 || authorityMethods != 40 {
			tb.Fatal("cross-operator cohort census differs", authorityHttp, authorityMethods)
		}
		tb.Logf("actual authenticated_clients=1000 operators=2 clients_per_operator=500 observed_pending_slots_per_operator=500 hard_slots_per_Core=1024 cohort_authority_http=8 cohort_authority_methods=40 cold_dial_http=2 cold_dial_methods=2 original_signed_rows=1000 immutable_content_and_history_objects=2000 separately_retried_clients=2 retry_http=8 retry_methods=40 elapsed=%s; logical ownership census, not heap peak or public paced startup latency", time.Since(started))
	})
}

// A missing real client credential is refused before admission or chain work.
func TestStClientKeyRegistrationCohortAuthenticatesBeforeAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, _, _, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		endpoint := stClientKeyRegistrationConnectEndpoint(tb)
		if err := stClientKeyRegistrationRequest(tb.Context(), endpoint.URL, "", bytes.Repeat([]byte{9}, 32)); err == nil {
			tb.Fatal("unsigned Connect request was accepted")
		}
		if requests, methods := fixture.counts(); requests != 0 || methods != 0 {
			tb.Fatal("unauthenticated registration reached Rpc")
		}
		cohorts.stateLock.Lock()
		closed := cohorts.slots == 0 && cohorts.pending == nil && cohorts.active == nil
		cohorts.stateLock.Unlock()
		if !closed {
			tb.Fatal("unauthenticated registration retained a slot")
		}
	})
}
