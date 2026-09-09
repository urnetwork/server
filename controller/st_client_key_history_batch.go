// One authenticated operation owns a finite client census and all of its exact
// historical authorities. Client signatures and original retained rows remain
// independent; there is no cross-request current-state cache.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/startifact"
)

// Work refusal is issued only before dialing, signatures or publication. The
// handler has already reserved each logical client's maximum response quota.
func SnClientKeyObservations(request *protocol.ClientKeyObservationBatchRequest, clientSession *session.ClientSession) (result *protocol.ClientKeyObservationBatchResponse, resultErr error) {
	return snClientKeyObservationsWithPublicationStore(request, clientSession, nil)
}

// The default and observed paths share every authority check and the same
// response-clearing defer. An optional private wrapper can observe only real
// publication I/O after Sql, Rpc, signatures and response admission complete.
func snClientKeyObservationsWithPublicationStore(request *protocol.ClientKeyObservationBatchRequest, clientSession *session.ClientSession, wrapPublicationStore func(server.BlobStore) server.BlobStore) (result *protocol.ClientKeyObservationBatchResponse, resultErr error) {
	if request == nil || clientSession == nil || clientSession.Ctx == nil || clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil || *clientSession.ByJwt.ClientId == (server.Id{}) {
		return nil, errors.New("client-key batch authenticated owner is incomplete")
	}
	if err := errors.Join(clientSession.Ctx.Err(), request.Validate()); err != nil {
		return nil, err
	}
	owned := *request
	owned.Requests = slices.Clone(request.Requests)
	ctx, cancel := context.WithTimeout(clientSession.Ctx, time.Duration(protocol.ClientKeyObservationBatchOperationSeconds)*time.Second)
	defer cancel()
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = nil
		}
	}()
	owner, err := newStClientKeyAuthorityOwner()
	if err != nil {
		return nil, err
	}
	control := uint64(len(owned.Requests)) * (uint64(reflect.TypeFor[protocol.ClientKeyObservationRequest]().Size()) + uint64(reflect.TypeFor[[]model.StClientKeyHistoryRecord]().Size()) + 512)
	histories := make([][]model.StClientKeyHistoryRecord, len(owned.Requests))
	queries := []stabi.ClientKeyAuthorityQuery{{Domain: owner.domain, Boundary: owned.Requests[0].DecisionBoundary}}
	queryKVs := map[stabi.ClientKeyAuthorityQuery]bool{queries[0]: true}
	for index, item := range owned.Requests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if control >= protocol.MaxClientKeyObservationBatchControlBytes {
			return nil, errors.New("client-key batch exhausted its aggregate controls")
		}
		history, err := model.LoadStClientKeyHistory(ctx, owner.domain, server.Id(item.ClientID), model.MaxStClientKeyHistoryRegistrations, min(model.MaxStClientKeyHistoryBytes, protocol.MaxClientKeyObservationBatchControlBytes-control))
		if err != nil || len(history) == 0 {
			return nil, errors.Join(errors.New("client-key batch member has no complete durable history"), err)
		}
		for _, record := range history {
			// Match the model's admission for owned rows, then separately
			// charge the outgoing canonical/base64 response before its copy.
			width := uint64(reflect.TypeFor[model.StClientKeyHistoryRecord]().Size() + reflect.TypeFor[startifact.EvidenceEnvelope]().Size() + reflect.TypeFor[protocol.ClientKeyRegistration]().Size() + 256)
			width += uint64(6*len(record.RegistrationBytes) + 6*len(record.EvidenceBytes) + 2*len(record.EvidenceHash) + 32)
			if width > protocol.MaxClientKeyObservationBatchControlBytes-control {
				return nil, errors.New("client-key batch complete history exceeds its aggregate controls")
			}
			control += width
			boundary := record.Registration.EffectiveBoundary
			if boundary.Block > item.DecisionBoundary.Block || boundary.Epoch > item.DecisionBoundary.Epoch || boundary.Block == item.DecisionBoundary.Block && boundary != item.DecisionBoundary {
				return nil, errors.New("client-key batch registration follows its requested decision")
			}
			query := stabi.ClientKeyAuthorityQuery{Domain: owner.domain, Boundary: boundary}
			if !queryKVs[query] {
				if len(queries) >= protocol.MaxClientKeyObservationBatchRpcMethods {
					return nil, stabi.ErrClientKeyAuthorityRpcWork
				}
				queries = append(queries, query)
				queryKVs[query] = true
			}
		}
		histories[index] = history
	}
	limits := stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: protocol.MaxClientKeyObservationBatchRpcRequests, MaximumMethods: protocol.MaxClientKeyObservationBatchRpcMethods, MaximumBytes: protocol.MaxClientKeyObservationBatchControlBytes - control}
	if _, err := stabi.PlanClientKeyAuthorityRpc(queries, limits); err != nil {
		return nil, err
	}
	// Dialing an Http client performs no chain read. The batch itself owns
	// all native/network checks, and a failed owner is never reused.
	var client *ethclient.Client
	for _, endpoint := range owner.rpcURLs {
		client, err = ethclient.DialContext(ctx, endpoint)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if err != nil || client == nil {
		return nil, errors.Join(errors.New("client-key batch has no actual chain transport"), err)
	}
	defer client.Close()
	authorities, work, err := stabi.ReadClientKeyAuthoritiesContext(ctx, client, queries, limits)
	if err != nil {
		// A post-I/O failure must never be confused with fallback admission.
		return nil, errors.New("client-key batch actual authority read failed: " + err.Error())
	}
	control += work.Bytes
	root := crypto.PubkeyToAddress(owner.rootKey.PublicKey)
	if authorities[0].Operator.RootSigner != root {
		return nil, errors.New("client-key batch signer differs from its actual decision operator")
	}
	operatorKVs := make(map[stabi.ClientKeyAuthorityQuery]int, len(authorities))
	for index, authority := range authorities {
		operatorKVs[authority.Query] = index
	}
	result = &protocol.ClientKeyObservationBatchResponse{Responses: make([]json.RawMessage, len(owned.Requests))}
	// All authority reads, including their final witness, finish before the
	// first signature. Each exact canonical inner body remains its own slot.
	responseBytes := uint64(len("{\"responses\":[]}"))
	for index, item := range owned.Requests {
		history := histories[index]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response := protocol.ClientKeyHistoryResponse{History: make([][]byte, len(history))}
		for recordIndex, record := range history {
			query := stabi.ClientKeyAuthorityQuery{Domain: owner.domain, Boundary: record.Registration.EffectiveBoundary}
			position, found := operatorKVs[query]
			if !found || authorities[position].Operator.RootSigner != record.Registration.Signer {
				return nil, errors.New("client-key batch registration lacks independently observed historical authority")
			}
			response.History[recordIndex] = record.EvidenceBytes
		}
		latest := history[len(history)-1].Registration
		hash, err := latest.ContentHash()
		if err != nil {
			return nil, err
		}
		// Fixed statement/envelope bounds are reserved before signing. The
		// actual serialized body below then consumes the remaining controls.
		if control > protocol.MaxClientKeyObservationBatchControlBytes || 8*startifact.MaxClientKeyEvidenceBytes > protocol.MaxClientKeyObservationBatchControlBytes-control {
			return nil, errors.New("client-key batch signing controls are exhausted")
		}
		observation := protocol.ClientKeyObservation{Domain: owner.domain, ClientID: item.ClientID, Generation: latest.Generation, RegistrationHash: hash, Request: item}
		if err := protocol.SignClientKeyObservation(&observation, owner.rootKey); err != nil {
			return nil, err
		}
		encoded, _, err := startifact.SealClientKeyObservationEvidence(owner.deploymentID, observation, owner.artifactKey, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		response.Observation = encoded
		body, err := json.Marshal(response)
		if err != nil || uint64(len(body)) > (protocol.MaxClientKeyObservationBatchControlBytes-control)/8 {
			return nil, errors.Join(errors.New("client-key batch response exceeds aggregate controls"), err)
		}
		control += uint64(8 * len(body))
		responseBytes += uint64(len(body))
		if index != 0 {
			responseBytes++
		}
		if responseBytes+1 > owned.MaximumResponseBytes {
			return nil, errors.New("client-key batch response exceeds its requested wire allowance")
		}
		result.Responses[index] = body
	}
	// No prefix is published when a later member fails authority or bounds.
	// Original registration wrappers are never re-signed on retry.
	if wrapPublicationStore != nil {
		owner.store = wrapPublicationStore(owner.store)
	}
	if err := publishStClientKeyObservationBatch(ctx, owner, histories, result, owned.MaximumResponseBytes); err != nil {
		return nil, err
	}
	return result, ctx.Err()
}
