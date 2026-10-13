package server

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

type ArinShadowResourcePins struct {
	ActiveSHA256    string `json:"active_sha256"`
	CandidateSHA256 string `json:"candidate_sha256"`
	ActiveEpoch     int64  `json:"active_epoch"`
	CandidateEpoch  int64  `json:"candidate_epoch"`
}

func (r *ArinShadowRecorder) ResourcePins() (ArinShadowResourcePins, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ArinShadowResourcePins{}, ErrArinShadowInput
	}
	return ArinShadowResourcePins{r.activeHash, r.candidateHash, r.active.Metadata.BuildTime().Unix(), r.candidate.Metadata.BuildTime().Unix()}, nil
}

// Private wire rows have no address, masked-address hash or free-form reason.
// Only authenticated, exact nonce/source/resource-bound replies can import
// these into the opaque reducer. Missing owners retain their requested key.
type arinShadowCaptureWireRow struct {
	ConnectionId Id                     `json:"connection_id"`
	ClientId     Id                     `json:"client_id"`
	HandlerId    Id                     `json:"handler_id"`
	ActualAt     time.Time              `json:"actual_at"`
	ObservedAt   time.Time              `json:"observed_at"`
	CapturedAt   time.Time              `json:"captured_at"`
	State        string                 `json:"state"`
	Risk         bool                   `json:"risk"`
	ProxyRisk    bool                   `json:"proxy_risk"`
	Verified     bool                   `json:"verified"`
	Reason       string                 `json:"reason"`
	Registration ArinShadowRegistration `json:"registration"`
	Origin       ArinShadowOrigin       `json:"origin"`
}

type arinShadowCaptureRPCRequest struct {
	Pins        ArinShadowResourcePins `json:"pins"`
	Connections []Id                   `json:"connections"`
}
type arinShadowCaptureRPCReply struct {
	Pins ArinShadowResourcePins     `json:"pins"`
	Rows []arinShadowCaptureWireRow `json:"rows"`
}

// CaptureRPC is used only by a process owning the live registry. Classification
// and the fresh primary-key read occur there, so an IP never crosses IPC.
func (r *ArinShadowRecorder) CaptureRPC(ctx context.Context, input json.RawMessage, owners ArinShadowCaptureOwnerReader, facts ArinShadowCaptureFactReader) (any, error) {
	var request arinShadowCaptureRPCRequest
	pins, err := r.ResourcePins()
	if err != nil || DecodeArinShadowRPC(input, &request) != nil || request.Pins != pins || len(request.Connections) == 0 || len(request.Connections) > ArinShadowCaptureBatchLimit {
		return nil, ErrArinShadowInput
	}
	targets, err := owners(ctx, request.Connections)
	if err != nil || len(targets) != len(request.Connections) {
		return nil, ErrArinShadowInput
	}
	for i, id := range request.Connections {
		if targets[i].ConnectionId != id {
			return nil, ErrArinShadowInput
		}
	}
	rows, err := r.CaptureCurrent(ctx, targets, facts)
	if err != nil {
		return nil, err
	}
	reply := arinShadowCaptureRPCReply{Pins: pins, Rows: make([]arinShadowCaptureWireRow, len(rows))}
	for i, row := range rows {
		reply.Rows[i] = arinShadowCaptureWireRow{row.connectionId, row.clientId, row.handlerId, row.actualAt, row.observedAt, row.capturedAt, row.facts.state, row.facts.risk, row.facts.proxyRisk, row.facts.verified, row.reason, row.facts.registration, row.facts.origin}
	}
	return reply, nil
}

type ArinShadowCapturedBatch struct {
	rows []arinShadowCapturedConnection
}

func (r *ArinShadowRecorder) UnavailableCaptureBatch(ids []Id) (ArinShadowCapturedBatch, error) {
	if len(ids) == 0 || len(ids) > ArinShadowCaptureBatchLimit {
		return ArinShadowCapturedBatch{}, ErrArinShadowInput
	}
	rows := make([]arinShadowCapturedConnection, len(ids))
	seen := map[Id]bool{}
	for i, id := range ids {
		if id == (Id{}) || seen[id] {
			return ArinShadowCapturedBatch{}, ErrArinShadowInput
		}
		seen[id] = true
		rows[i] = arinShadowCapturedConnection{recorder: r, connectionId: id, capturedAt: r.clock(), reason: "owner_unavailable"}
	}
	return ArinShadowCapturedBatch{rows}, nil
}

// CallCapture accepts only the owning authenticated client's exact process.
// False/absent/dropped rows cannot become positive classification evidence.
func (r *ArinShadowRecorder) CallCapture(ctx context.Context, client *ArinShadowRPCClient, ids []Id) (ArinShadowCapturedBatch, error) {
	if client == nil || client.identity.Role != "connect" || len(ids) == 0 || len(ids) > ArinShadowCaptureBatchLimit {
		return ArinShadowCapturedBatch{}, ErrArinShadowInput
	}
	pins, err := r.ResourcePins()
	if err != nil {
		return ArinShadowCapturedBatch{}, err
	}
	started := r.clock()
	var reply arinShadowCaptureRPCReply
	if client.Call(ctx, "capture", arinShadowCaptureRPCRequest{pins, ids}, &reply) != nil || reply.Pins != pins || len(reply.Rows) != len(ids) {
		return ArinShadowCapturedBatch{}, ErrArinShadowInput
	}
	rows := make([]arinShadowCapturedConnection, len(ids))
	seen := map[Id]bool{}
	for i, row := range reply.Rows {
		if ids[i] == (Id{}) || seen[ids[i]] || row.ConnectionId != ids[i] || !validArinShadowRegistration(row.Registration) || !validArinShadowOrigin(row.Origin) || !slices.Contains(arinShadowCaptureReasons[:], row.Reason) || row.CapturedAt.Before(started.Add(-arinShadowCaptureClockSkew)) || row.CapturedAt.After(r.clock().Add(arinShadowCaptureClockSkew)) {
			return ArinShadowCapturedBatch{}, ErrArinShadowInput
		}
		seen[ids[i]] = true
		if row.Reason == "qualified" && (row.ClientId == (Id{}) || row.HandlerId == (Id{}) || row.ActualAt.IsZero() || row.ObservedAt.Before(started.Add(-arinShadowCaptureClockSkew)) || row.ActualAt.After(row.ObservedAt.Add(arinShadowCaptureClockSkew)) || !slices.Contains([]string{"subscriber", "excluded", "unknown", "ambiguous"}, row.State) || row.ProxyRisk && !row.Risk || row.Verified && row.State != "subscriber") {
			return ArinShadowCapturedBatch{}, ErrArinShadowInput
		}
		rows[i] = arinShadowCapturedConnection{recorder: r, connectionId: row.ConnectionId, clientId: row.ClientId, handlerId: row.HandlerId, actualAt: row.ActualAt, observedAt: row.ObservedAt, capturedAt: row.CapturedAt, facts: arinShadowFacts{state: row.State, risk: row.Risk, proxyRisk: row.ProxyRisk, verified: row.Verified, registration: row.Registration, origin: newArinShadowOrigin(row.Origin.ASNs, row.Origin.UseState)}, reason: row.Reason}
	}
	return ArinShadowCapturedBatch{rows}, nil
}

// ConsumeCaptured consumes exactly one source page in source order, even when
// an operator prefetched several bounded per-handler batches concurrently.
func (c *ArinShadowCaptureCollector) ConsumeCaptured(page ArinShadowCapturePage, batches ...ArinShadowCapturedBatch) error {
	s := c.stream
	if !s.live() || page.GenerationSHA256 != c.cohort.GenerationSHA256 || page.Sequence != c.sequence || len(page.Records) == 0 || len(page.Records) > ArinShadowCaptureBatchLimit {
		return s.reject()
	}
	rows := map[Id]arinShadowCapturedConnection{}
	for _, batch := range batches {
		for _, row := range batch.rows {
			if row.recorder != s.recorder || len(rows) >= ArinShadowCaptureBatchLimit {
				return s.reject()
			}
			if _, exists := rows[row.connectionId]; exists {
				return s.reject()
			}
			rows[row.connectionId] = row
		}
	}
	if len(rows) != len(page.Records) {
		return s.reject()
	}
	for _, record := range page.Records {
		row, exists := rows[record.ConnectionId]
		if !exists || record.HandlerId == (Id{}) {
			return s.reject()
		}
		if record.Provider != nil {
			if s.report.Providers >= c.cohort.Providers || s.BeginProvider(*record.Provider) != nil {
				return s.reject()
			}
		}
		if row.reason == "qualified" && row.handlerId != record.HandlerId {
			row.reason = "binding_mismatch"
		}
		if s.addCaptured([]arinShadowCapturedConnection{row}) != nil {
			return s.reject()
		}
		if s.current.count == s.current.input.ExpectedConnections && s.EndProvider() != nil {
			return s.reject()
		}
	}
	c.sequence++
	return nil
}

// SplitArinShadowCapturedBatch keeps prefetch bounded without exposing private
// rows to the operator. Only exact keys from existing sealed batches may split.
func SplitArinShadowCapturedBatch(ids []Id, batches ...ArinShadowCapturedBatch) (ArinShadowCapturedBatch, error) {
	if len(ids) == 0 || len(ids) > ArinShadowCaptureBatchLimit || len(batches) > 256 {
		return ArinShadowCapturedBatch{}, ErrArinShadowInput
	}
	rows := make([]arinShadowCapturedConnection, 0, len(ids))
	indexed := make(map[Id]arinShadowCapturedConnection)
	for _, batch := range batches {
		for _, row := range batch.rows {
			if _, duplicate := indexed[row.connectionId]; duplicate || len(indexed) >= 4096 {
				return ArinShadowCapturedBatch{}, ErrArinShadowInput
			}
			indexed[row.connectionId] = row
		}
	}
	seen := map[Id]bool{}
	for _, id := range ids {
		if seen[id] {
			return ArinShadowCapturedBatch{}, ErrArinShadowInput
		}
		seen[id] = true
		row, found := indexed[id]
		if !found {
			return ArinShadowCapturedBatch{}, ErrArinShadowInput
		}
		rows = append(rows, row)
	}
	return ArinShadowCapturedBatch{rows}, nil
}
