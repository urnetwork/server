// Actual authenticated controller, Sql history, immutable publication and
// validator Http transport exercise both shared and distinct boundary censuses.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/stabi"
	"github.com/urfoundation/sn/v2026/validator"
	connectprotocol "github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// The existing genuine ABI fixture is reused at independently named raw
// boundaries. State is changed only between completely joined operations.
type stClientKeyBatchRpcFixture struct {
	stateLock  sync.Mutex
	base       *stClientKeyHistoryRPCFixture
	boundaries map[common.Hash]protocol.ClientKeyEffectiveBoundary
	requests   uint64
	methods    uint64
}

// Network methods still run through the real Rpc decoder and base checks.
func (self *stClientKeyBatchRpcFixture) ChainId(ctx context.Context) (hexutil.Uint64, error) {
	return self.base.ChainId(ctx)
}
func (self *stClientKeyBatchRpcFixture) GetBlockHash(ctx context.Context, block uint64) (*common.Hash, error) {
	return self.base.GetBlockHash(ctx, block)
}

// A real numbered lookup cannot substitute another boundary's current hash.
func (self *stClientKeyBatchRpcFixture) GetBlockByNumber(ctx context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	if tag == rpc.FinalizedBlockNumber || tag == 0 {
		return self.base.GetBlockByNumber(ctx, tag, full)
	}
	for _, boundary := range self.boundaries {
		if boundary.Block == uint64(tag) {
			copy := *self.base
			copy.boundary = boundary
			return copy.GetBlockByNumber(ctx, tag, full)
		}
	}
	return nil, errors.New("unknown historical block")
}

// Every view retains its concrete hash and the independently configured root.
func (self *stClientKeyBatchRpcFixture) Call(ctx context.Context, call map[string]hexutil.Bytes, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if selector.BlockHash == nil {
		return nil, errors.New("missing historical hash")
	}
	boundary, found := self.boundaries[*selector.BlockHash]
	if !found {
		return nil, errors.New("foreign historical hash")
	}
	copy := *self.base
	copy.boundary = boundary
	return copy.Call(ctx, call, selector)
}

// Counts actual wire requests and all batch members, without rate-policy edits.
func (self *stClientKeyBatchRpcFixture) count() (uint64, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.requests, self.methods
}

// The real concrete controller owns the new endpoint; source decoding cannot
// inject an accepted key, operator signer, chain result or storage receipt.
func stClientKeyBatchTestEndpoint(t testing.TB) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sn/client-key/observations" {
			http.Error(w, "wrong route", 400)
			return
		}
		router.WrapRequireClient(func(clientSession *session.ClientSession) (*protocol.ClientKeyObservationBatchResponse, error) {
			defer clientSession.Cancel()
			defer r.Body.Close()
			encoded, err := io.ReadAll(io.LimitReader(r.Body, protocol.MaxClientKeyObservationBatchRequestBytes+1))
			if err != nil {
				return nil, err
			}
			request, err := protocol.DecodeClientKeyObservationBatchRequest(encoded)
			if err != nil {
				return nil, err
			}
			result, err := SnClientKeyObservations(&request, clientSession)
			if errors.Is(err, stabi.ErrClientKeyAuthorityRpcWork) {
				w.Header().Set("X-Ur-Client-Key-Batch-Admission", "work")
				return nil, errors.New("413 Client-key batch work admission refused.")
			}
			return result, err
		}, w, r)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// Two operators each have404 active clients and both real validator requests
// retain all808 independent source signatures. This tests a boundary
// distribution, not a claim that the public paced provider meets a deadline.
func runStClientKeyHistoryBatchPopulation(t *testing.T, distinct bool) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		var totalHttp, totalMethods uint64
		var registrationHttp, registrationMethods uint64
		started := time.Now()
		for _, noId := range []uint64{1, 2} {
			base, credential, cfg := newStClientKeyHistoryControllerFixture(tb)
			base.domain.NoID, cfg.NoId = noId, noId
			transport := &stClientKeyBatchRpcFixture{base: base, boundaries: map[common.Hash]protocol.ClientKeyEffectiveBoundary{}}
			if !distinct {
				transport.boundaries[common.Hash(base.boundary.Hash)] = base.boundary
			}
			service := rpc.NewServer()
			if err := service.RegisterName("eth", transport); err != nil {
				tb.Fatal(err)
			}
			if err := service.RegisterName("chain", transport); err != nil {
				tb.Fatal(err)
			}
			chainEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024+1))
				_ = r.Body.Close()
				if err != nil || len(body) > 1024*1024 {
					http.Error(w, "invalid request", 400)
					return
				}
				var batch []json.RawMessage
				count := 1
				if len(body) > 0 && body[0] == '[' {
					if json.Unmarshal(body, &batch) != nil || len(batch) == 0 || len(batch) > stabi.MaxClientKeyAuthorityRpcBatchMembers {
						http.Error(w, "excessive batch", 400)
						return
					}
					count = len(batch)
				}
				transport.stateLock.Lock()
				transport.requests++
				transport.methods += uint64(count)
				transport.stateLock.Unlock()
				r.Body = io.NopCloser(bytes.NewReader(body))
				service.ServeHTTP(w, r)
			}))
			tb.Cleanup(func() { chainEndpoint.Close(); service.Stop() })
			cfg.RpcUrls = []string{chainEndpoint.URL}
			requests := make([]protocol.ClientKeyObservationRequest, 404)
			for index := range requests {
				clientId, deviceId := server.NewId(), server.NewId()
				model.Testing_CreateDevice(tb.Context(), credential.NetworkId, deviceId, clientId, "batch-client", "test")
				if distinct {
					base.boundary = protocol.ClientKeyEffectiveBoundary{Block: uint64(index + 100), Hash: [32]byte(common.BigToHash(new(big.Int).SetUint64(uint64(index + 100))))}
					transport.boundaries[common.Hash(base.boundary.Hash)] = base.boundary
				}
				if err := SetClientKey(tb.Context(), clientId, &connectprotocol.ClientKey{PublicKey: bytes.Repeat([]byte{9}, 32)}); err != nil {
					tb.Fatal(err)
				}
				requests[index] = protocol.ClientKeyObservationRequest{ClientID: [16]byte(clientId), ValidatorHotkey: [32]byte{6}, NativeBlock: 1001, NativeHash: [32]byte{7}, NativeEpoch: 2, Nonce: [32]byte{8}}
			}
			if distinct {
				base.boundary = protocol.ClientKeyEffectiveBoundary{Block: 1000, Hash: [32]byte(common.BigToHash(big.NewInt(1000)))}
				transport.boundaries[common.Hash(base.boundary.Hash)] = base.boundary
			}
			for index := range requests {
				requests[index].DecisionBoundary = base.boundary
			}
			sort.Slice(requests, func(i, j int) bool { return bytes.Compare(requests[i].ClientID[:], requests[j].ClientID[:]) < 0 })
			endpoint := stClientKeyBatchTestEndpoint(tb)
			reader, err := validator.NewHTTPClientKeyHistoryReader(endpoint.URL, func() string { return credential.Sign() })
			if err != nil {
				tb.Fatal(err)
			}
			beforeHttp, beforeMethods := transport.count()
			registrationHttp += beforeHttp
			registrationMethods += beforeMethods
			original := make([][]byte, len(requests))
			for validatorIndex := byte(1); validatorIndex <= 2; validatorIndex++ {
				for index := range requests {
					requests[index].ValidatorHotkey[1], requests[index].Nonce[1] = validatorIndex, validatorIndex
				}
				responses, err := reader.ReadBatch(tb.Context(), requests, protocol.MaxClientKeyObservationBatchResponseBytes)
				if err != nil || len(responses) != 404 {
					tb.Fatalf("actual complete batch no_id=%d distinct=%t: count=%d error=%v", noId, distinct, len(responses), err)
				}
				for index, encoded := range responses {
					response, err := protocol.DecodeClientKeyHistoryResponse(encoded, protocol.MaxClientKeyHistoryResponseBytes)
					if err != nil || len(response.History) != 1 {
						tb.Fatal("actual durable history differs", err)
					}
					envelope, err := protocol.DecodeClientKeyEvidence(response.Observation, base.domain, protocol.ClientKeyObservationEvidenceKind)
					if err != nil {
						tb.Fatal(err)
					}
					observation, err := protocol.DecodeClientKeyObservation(envelope.Payload)
					if err != nil || observation.Request != requests[index] || observation.Signer != base.root {
						tb.Fatal("actual per-validator source signature differs", err)
					}
					if validatorIndex == 1 {
						original[index] = bytes.Clone(response.History[0])
					} else if !bytes.Equal(original[index], response.History[0]) {
						tb.Fatal("retry replaced original signed history")
					}
				}
			}
			afterHttp, afterMethods := transport.count()
			totalHttp += afterHttp - beforeHttp
			totalMethods += afterMethods - beforeMethods
		}
		wantHttp, wantMethods := uint64(16), uint64(80)
		if distinct {
			wantHttp, wantMethods = 4*(25*8+4), 4*(25*244+76)
		}
		if totalHttp != wantHttp || totalMethods != wantMethods {
			tb.Fatalf("full808 x2 validators source work: Http=%d/%d methods=%d/%d", totalHttp, wantHttp, totalMethods, wantMethods)
		}
		tb.Logf("actual source census: operators=2 active_clients_per_operator=404 validators=2 distinct_registration_boundaries=%t observation_rpc_http=%d observation_rpc_methods=%d registration_rpc_http=%d registration_rpc_methods=%d elapsed=%s; the local Http fixture is not a public paced latency measurement", distinct, totalHttp, totalMethods, registrationHttp, registrationMethods, time.Since(started))
	})
}

func TestStClientKeyHistoryBatchActualFullPopulationSharedBoundary(t *testing.T) {
	runStClientKeyHistoryBatchPopulation(t, false)
}
func TestStClientKeyHistoryBatchActualFullPopulationDistinctBoundaries(t *testing.T) {
	runStClientKeyHistoryBatchPopulation(t, true)
}
