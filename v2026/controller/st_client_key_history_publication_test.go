// Finite local publication owns only quota coordination: real client-key
// statement signatures, immutable writes and exact readbacks remain required.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/protocol"
	connectprotocol "github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/startifact"
)

// Observations never replace an actual store operation or accepted receipt.
type stClientKeyPublicationStoreTest struct {
	server.BlobStore
	wantWires  map[string]bool
	contexts   []context.Context
	gets       int
	closes     int
	afterClose func(int)
}

// The optional quota source is the same instance used by all original writes.
func (self *stClientKeyPublicationStoreTest) LocalBlobBatchSource() server.BlobStore {
	return self.BlobStore
}

// Every attempted route must stage its exact independently signed wire.
func (self *stClientKeyPublicationStoreTest) PutIfAbsent(ctx context.Context, key, source, contentType string) (bool, error) {
	encoded, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	if self.wantWires != nil && !self.wantWires[string(encoded)] {
		return false, errors.New("publication changed its original signed wire")
	}
	self.contexts = append(self.contexts, ctx)
	return self.BlobStore.PutIfAbsent(ctx, key, source, contentType)
}

// Close observations run only after the actual reader has released its file.
func (self *stClientKeyPublicationStoreTest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	reader, err := self.BlobStore.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	self.gets++
	return &stClientKeyPublicationReadTest{ReadCloser: reader, afterClose: func() {
		self.closes++
		if self.afterClose != nil {
			self.afterClose(self.closes)
		}
	}}, nil
}

// One close observation belongs to one real borrowed reader.
type stClientKeyPublicationReadTest struct {
	io.ReadCloser
	afterClose func()
}

// The underlying close error remains the actual publisher's responsibility.
func (self *stClientKeyPublicationReadTest) Close() error {
	err := self.ReadCloser.Close()
	if self.afterClose != nil {
		self.afterClose()
		self.afterClose = nil
	}
	return err
}

// Both signed layers and every original response slot are independently owned.
type stClientKeyPublicationFixtureTest struct {
	root      string
	owner     *stClientKeyAuthorityOwner
	store     *stClientKeyPublicationStoreTest
	histories [][]model.StClientKeyHistoryRecord
	result    *protocol.ClientKeyObservationBatchResponse
}

// Thirty-three clients produce sixty-six independently signed envelopes, so
// the actual bulk helper must release and reacquire its sixty-four-object scope.
func newStClientKeyPublicationFixtureTest(t *testing.T, clients int) *stClientKeyPublicationFixtureTest {
	t.Helper()
	rootKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	artifactKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	deployment := "synthetic-publication"
	domain := protocol.ClientKeyHistoryDomain{ChainID: 31337, GenesisHash: [32]byte{1}, Netuid: 77, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte(deployment)), PolicyHash: [32]byte{4}, NoID: 1}
	fixture := &stClientKeyPublicationFixtureTest{root: t.TempDir(), result: &protocol.ClientKeyObservationBatchResponse{}}
	fixture.store = &stClientKeyPublicationStoreTest{BlobStore: server.NewLocalBlobStore(fixture.root, "synthetic-history"), wantWires: map[string]bool{}}
	fixture.owner = &stClientKeyAuthorityOwner{domain: domain, deploymentID: deployment, rootKey: rootKey, artifactKey: artifactKey, store: fixture.store}
	for index := 0; index < clients; index++ {
		registration := protocol.ClientKeyRegistration{Domain: domain, ClientID: [16]byte{byte(index + 1)}, NetworkID: [16]byte{7}, Generation: 1, Present: true, PublicKey: [32]byte{8}, EffectiveBoundary: protocol.ClientKeyEffectiveBoundary{Block: 70, Hash: [32]byte{9}}}
		if err := protocol.SignClientKeyRegistration(&registration, rootKey); err != nil {
			t.Fatal(err)
		}
		registrationBytes, err := registration.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		registrationHash, err := registration.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		registered, evidenceHash, err := startifact.SealClientKeyRegistrationEvidence(deployment, registration, artifactKey, time.Unix(1700000000, 0))
		if err != nil {
			t.Fatal(err)
		}
		observation := protocol.ClientKeyObservation{Domain: domain, ClientID: registration.ClientID, Generation: 1, RegistrationHash: registrationHash, Request: protocol.ClientKeyObservationRequest{ClientID: registration.ClientID, ValidatorHotkey: [32]byte{11}, NativeBlock: 80, NativeHash: [32]byte{12}, NativeEpoch: 3, DecisionBoundary: protocol.ClientKeyEffectiveBoundary{Epoch: 1, Block: 79, Hash: [32]byte{13}}, Nonce: [32]byte{14}}}
		if err := protocol.SignClientKeyObservation(&observation, rootKey); err != nil {
			t.Fatal(err)
		}
		observed, _, err := startifact.SealClientKeyObservationEvidence(deployment, observation, artifactKey, time.Unix(1700000001, 0))
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(protocol.ClientKeyHistoryResponse{History: [][]byte{registered}, Observation: observed})
		if err != nil {
			t.Fatal(err)
		}
		fixture.histories = append(fixture.histories, []model.StClientKeyHistoryRecord{{Registration: registration, RegistrationBytes: registrationBytes, EvidenceHash: evidenceHash, EvidenceBytes: registered}})
		fixture.result.Responses = append(fixture.result.Responses, body)
		fixture.store.wantWires[string(registered)] = true
		fixture.store.wantWires[string(observed)] = true
	}
	return fixture
}

// The actual default publisher still stages and independently reads both
// immutable routes on a fresh write and an exact collided retry.
func TestStClientKeyPublicationBatchRollsWithEveryImmutableReadback(t *testing.T) {
	fixture := newStClientKeyPublicationFixtureTest(t, 33)
	before, err := json.Marshal(fixture.result)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := publishStClientKeyObservationBatch(t.Context(), fixture.owner, fixture.histories, fixture.result, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
			t.Fatal(err)
		}
		count := 4 * 33
		if len(fixture.store.contexts) != count*(pass+1) || fixture.store.gets != count*(pass+1) || fixture.store.closes != fixture.store.gets {
			t.Fatal("bulk publication omitted original immutable attempts or exact readbacks")
		}
		contexts := fixture.store.contexts[pass*count : (pass+1)*count]
		for _, ctx := range contexts[:2*stClientKeyPublicationBatchEnvelopes] {
			if ctx != contexts[0] || ctx == t.Context() {
				t.Fatal("bulk publication rescanned instead of retaining its finite quota owner")
			}
		}
		for _, ctx := range contexts[2*stClientKeyPublicationBatchEnvelopes:] {
			if ctx != contexts[len(contexts)-1] || ctx == contexts[0] || ctx == t.Context() {
				t.Fatal("bulk publication failed to roll its finite quota owner")
			}
		}
		if pass > 0 && contexts[0] == fixture.store.contexts[0] {
			t.Fatal("a completed response retained its previous quota owner")
		}
	}
	after, err := json.Marshal(fixture.result)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("publication mutated retained response bytes", err)
	}
}

// Fresh signature, retained hash and exact-wire validation cannot be bypassed
// by the context's accepted storage accounting owner.
func TestStClientKeyPublicationBatchRejectsChangedRetainedEvidence(t *testing.T) {
	for _, fault := range []string{"signature", "hash", "trailing"} {
		fixture := newStClientKeyPublicationFixtureTest(t, 1)
		record := &fixture.histories[0][0]
		switch fault {
		case "signature":
			var envelope startifact.EvidenceEnvelope
			if err := json.Unmarshal(record.EvidenceBytes, &envelope); err != nil {
				t.Fatal(err)
			}
			envelope.Signature = "0x" + strings.Repeat("00", 65)
			var err error
			record.EvidenceBytes, err = json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
		case "hash":
			record.EvidenceHash = "sha256:" + strings.Repeat("00", 32)
		case "trailing":
			record.EvidenceBytes = append(bytes.Clone(record.EvidenceBytes), '\n')
		}
		if err := publishStClientKeyObservationBatch(t.Context(), fixture.owner, fixture.histories, fixture.result, protocol.MaxClientKeyHistoryResponseBytes); err == nil || len(fixture.store.contexts) != 0 || fixture.store.gets != 0 {
			t.Fatalf("%s retained evidence reached publication: %v", fault, err)
		}
	}
}

// A foreign observation domain and an invalid typed body still fail through
// the original decoder even after authentic history routes were published.
func TestStClientKeyPublicationBatchPreservesTypedObservationAdmission(t *testing.T) {
	for _, fault := range []string{"domain", "body", "allowance"} {
		fixture := newStClientKeyPublicationFixtureTest(t, 1)
		maximum := uint64(protocol.MaxClientKeyHistoryResponseBytes)
		switch fault {
		case "domain":
			fixture.owner.domain.ChainID++
		case "body":
			fixture.result.Responses[0] = json.RawMessage(`{"history":[],"observation":"e30="}`)
		case "allowance":
			maximum = 1
		}
		if err := publishStClientKeyObservationBatch(t.Context(), fixture.owner, fixture.histories, fixture.result, maximum); err == nil || len(fixture.store.contexts) != 2 || fixture.store.gets != 2 || fixture.store.closes != 2 {
			t.Fatalf("%s observation bypassed original typed admission: %v", fault, err)
		}
	}
}

// Final reconciliation and cancellation both remain observable, with all
// actual immutable routes completed before the explicit fault boundary.
func TestStClientKeyPublicationBatchRetainsFinalFailureAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		fixture := newStClientKeyPublicationFixtureTest(t, 1)
		ctx, cancel := context.WithCancel(t.Context())
		fixture.store.afterClose = func(count int) {
			if count == 4 {
				if cancelled {
					cancel()
				} else if err := os.WriteFile(filepath.Join(fixture.root, "unaccounted.json"), []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		err := publishStClientKeyObservationBatch(ctx, fixture.owner, fixture.histories, fixture.result, protocol.MaxClientKeyHistoryResponseBytes)
		cancel()
		if err == nil || len(fixture.store.contexts) != 4 || fixture.store.closes != 4 || cancelled && !errors.Is(err, context.Canceled) || !cancelled && !strings.Contains(err.Error(), "unaccounted bytes") {
			t.Fatal("bulk publication lost its final failure", cancelled, err)
		}
		fixture.store.afterClose = nil
		if err := publishStClientKeyObservationBatch(t.Context(), fixture.owner, fixture.histories, fixture.result, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
			t.Fatal("failed publication kept its quota lock or stale usage", err)
		}
	}
}

// Nil/default entrypoint admission retains the same real implementation, and
// no invalid operation can acquire publication storage before authority work.
func TestStClientKeyPublicationBatchRejectsUnownedAdmission(t *testing.T) {
	fixture := newStClientKeyPublicationFixtureTest(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, input := range []struct {
		ctx       context.Context
		owner     *stClientKeyAuthorityOwner
		histories [][]model.StClientKeyHistoryRecord
		result    *protocol.ClientKeyObservationBatchResponse
	}{
		{ctx: nil, owner: fixture.owner, histories: fixture.histories, result: fixture.result},
		{ctx: ctx, owner: fixture.owner, histories: fixture.histories, result: fixture.result},
		{ctx: t.Context(), histories: fixture.histories, result: fixture.result},
		{ctx: t.Context(), owner: fixture.owner, result: fixture.result},
		{ctx: t.Context(), owner: fixture.owner, histories: fixture.histories},
	} {
		if err := publishStClientKeyObservationBatch(input.ctx, input.owner, input.histories, input.result, protocol.MaxClientKeyHistoryResponseBytes); err == nil || len(fixture.store.contexts) != 0 {
			t.Fatal("unowned bulk publication reached storage", err)
		}
	}
	if result, err := SnClientKeyObservations(nil, nil); result != nil || err == nil {
		t.Fatal("public default delegation lost initial admission", result, err)
	}
	wrapped := false
	if result, err := snClientKeyObservationsWithPublicationStore(nil, nil, func(store server.BlobStore) server.BlobStore { wrapped = true; return store }); result != nil || err == nil || wrapped {
		t.Fatal("invalid operation reached the private publication observer", result, err, wrapped)
	}
}

// The public endpoint delegates to this exact implementation with nil. Only
// real publication I/O is observed after actual Sql, Rpc, signatures and bound
// checks; a final storage failure must clear its already-built response.
func TestStClientKeyPublicationBatchActualControllerClearsFinalFailedResponse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(tb)
		root := tb.TempDir()
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf("authority: local\npath: %s\nprefix: key-history\nmax_bytes: %d\n", root, 32*1024*1024)))
		tb.Cleanup(pop)
		if err := SetClientKey(tb.Context(), *credential.ClientId, &connectprotocol.ClientKey{PublicKey: bytes.Repeat([]byte{9}, 32)}); err != nil {
			tb.Fatal(err)
		}
		request := &protocol.ClientKeyObservationBatchRequest{Requests: []protocol.ClientKeyObservationRequest{{ClientID: [16]byte(*credential.ClientId), ValidatorHotkey: [32]byte{6}, NativeBlock: 101, NativeHash: [32]byte{7}, NativeEpoch: 2, DecisionBoundary: fixture.boundary, Nonce: [32]byte{8}}}, MaximumResponseBytes: protocol.MaxClientKeyHistoryResponseBytes}
		clientSession := &session.ClientSession{Ctx: tb.Context(), ByJwt: credential}
		var observer *stClientKeyPublicationStoreTest
		changed := false
		result, err := snClientKeyObservationsWithPublicationStore(request, clientSession, func(store server.BlobStore) server.BlobStore {
			observer = &stClientKeyPublicationStoreTest{BlobStore: store}
			observer.afterClose = func(count int) {
				if count == 4 {
					if err := os.WriteFile(filepath.Join(root, "unaccounted.json"), []byte("synthetic"), 0o600); err != nil {
						tb.Fatal(err)
					}
					changed = true
				}
			}
			return observer
		})
		if observer == nil || !changed || result != nil || err == nil || !strings.Contains(err.Error(), "unaccounted bytes") {
			tb.Fatalf("actual controller leaked its failed response: observed=%v changed=%v result=%v err=%v", observer != nil, changed, result, err)
		}
		if len(observer.contexts) != 4 || observer.gets != 4 || observer.closes != 4 {
			tb.Fatal("actual final failure control omitted a signed immutable route or readback")
		}
		// The nil/default public route must independently retry the genuine
		// durable registration and return a freshly authenticated observation.
		result, err = SnClientKeyObservations(request, clientSession)
		if err != nil || result == nil || len(result.Responses) != 1 {
			tb.Fatal("actual default retry retained failed quota ownership", result, err)
		}
		response, err := protocol.DecodeClientKeyHistoryResponse(result.Responses[0], protocol.MaxClientKeyHistoryResponseBytes)
		if err != nil || len(response.History) != 1 {
			tb.Fatal("default retry lost complete durable history", err)
		}
		registrationEnvelope, err := protocol.DecodeClientKeyEvidence(response.History[0], fixture.domain, protocol.ClientKeyRegistrationEvidenceKind)
		if err != nil {
			tb.Fatal(err)
		}
		registration, err := protocol.DecodeClientKeyRegistration(registrationEnvelope.Payload)
		if err != nil {
			tb.Fatal(err)
		}
		observationEnvelope, err := protocol.DecodeClientKeyEvidence(response.Observation, fixture.domain, protocol.ClientKeyObservationEvidenceKind)
		if err != nil {
			tb.Fatal(err)
		}
		observation, err := protocol.DecodeClientKeyObservation(observationEnvelope.Payload)
		if err != nil {
			tb.Fatal(err)
		}
		if err := observation.VerifyRegistration(registration, fixture.domain, request.Requests[0], fixture.root, fixture.root); err != nil {
			tb.Fatal("default retry bypassed actual historical authority", err)
		}
	})
}
