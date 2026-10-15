package server

// A current-owner capture is a separate authority from natural location
// writes. It never refreshes a stored lookup clock, writes serving facts or
// installs the candidate policy. Callers must separately attest their complete
// provider/native-membership census and the running resource generations.
import (
	"context"
	"net/netip"
	"time"
)

const ArinShadowCaptureBatchLimit = 256
const ArinShadowCaptureMaxAge = 90 * time.Second
const arinShadowCaptureClockSkew = 2 * time.Second

// Private transport facts are consumed in memory. No capture row or owner
// reference is part of the aggregate report or a public transport API.
type ArinShadowOwnerSnapshot struct {
	ConnectionId, ClientId, HandlerId Id         `json:"-"`
	Address                           netip.Addr `json:"-"`
	At                                time.Time  `json:"-"`
}

type ArinShadowConnectionOwner interface {
	ArinShadowCurrentConnection() (ArinShadowOwnerSnapshot, bool)
}

type ArinShadowCaptureTarget struct {
	ConnectionId Id                        `json:"-"`
	Owner        ArinShadowConnectionOwner `json:"-"`
}

// ObservedAt is a fresh durable read's clock. Actual.At remains the original
// immutable lookup time and may be old under a proved unchanged live owner.
type ArinShadowCaptureFacts struct {
	ConnectionId, ClientId, HandlerId Id                    `json:"-"`
	ObservedAt                        time.Time             `json:"-"`
	Connected, Present                bool                  `json:"-"`
	Actual                            ArinShadowActiveFacts `json:"-"`
}

type ArinShadowCaptureFactReader func(context.Context, []Id) ([]ArinShadowCaptureFacts, error)

type arinShadowCapturedConnection struct {
	recorder                          *ArinShadowRecorder
	connectionId, clientId, handlerId Id
	actualAt, observedAt, capturedAt  time.Time
	facts                             arinShadowFacts
	reason                            string
}

var arinShadowCaptureReasons = [...]string{
	"qualified", "owner_unavailable", "owner_changed", "facts_unavailable",
	"facts_stale", "binding_mismatch", "active_mismatch", "lookup_unavailable",
}

func currentArinShadowOwner(owner ArinShadowConnectionOwner, id Id, now time.Time) (ArinShadowOwnerSnapshot, bool) {
	if owner == nil {
		return ArinShadowOwnerSnapshot{}, false
	}
	s, ok := owner.ArinShadowCurrentConnection()
	return s, ok && id != (Id{}) && s.ConnectionId == id && s.ClientId != (Id{}) && s.HandlerId != (Id{}) &&
		s.Address.IsValid() && !s.Address.IsUnspecified() && s.Address.Zone() == "" &&
		!s.At.IsZero() && !s.At.Before(now.Add(-ArinShadowCaptureMaxAge)) && !s.At.After(now.Add(arinShadowCaptureClockSkew))
}

func sameArinShadowOwner(a, b ArinShadowOwnerSnapshot) bool {
	return a.ConnectionId == b.ConnectionId && a.ClientId == b.ClientId && a.HandlerId == b.HandlerId &&
		a.Address.Unmap() == b.Address.Unmap()
}

// CaptureCurrent reads at most256 exact durable keys between two live-owner
// observations. The reader must honor its context. No owner or recorder lock
// is held across that callback, and addresses are discarded before return.
func (r *ArinShadowRecorder) CaptureCurrent(ctx context.Context, targets []ArinShadowCaptureTarget, read ArinShadowCaptureFactReader) ([]arinShadowCapturedConnection, error) {
	if r == nil || ctx == nil || read == nil || len(targets) == 0 || len(targets) > ArinShadowCaptureBatchLimit || ctx.Err() != nil {
		return nil, ErrArinShadowInput
	}
	started := r.clock()
	ids := make([]Id, len(targets))
	before := make([]ArinShadowOwnerSnapshot, len(targets))
	valid := make([]bool, len(targets))
	seen := make(map[Id]bool, len(targets))
	for i, target := range targets {
		if target.ConnectionId == (Id{}) || seen[target.ConnectionId] {
			return nil, ErrArinShadowInput
		}
		seen[target.ConnectionId] = true
		ids[i] = target.ConnectionId
		before[i], valid[i] = currentArinShadowOwner(target.Owner, target.ConnectionId, started)
	}
	bounded, cancel := context.WithTimeout(ctx, ArinShadowCaptureMaxAge)
	defer cancel()
	facts, err := read(bounded, ids)
	if err != nil || bounded.Err() != nil || len(facts) > len(ids) {
		return nil, ErrArinShadowInput
	}
	byId := make(map[Id]ArinShadowCaptureFacts, len(facts))
	for _, fact := range facts {
		if !seen[fact.ConnectionId] {
			return nil, ErrArinShadowInput
		}
		if _, exists := byId[fact.ConnectionId]; exists {
			return nil, ErrArinShadowInput
		}
		byId[fact.ConnectionId] = fact
	}
	rows := make([]arinShadowCapturedConnection, 0, len(targets))
	for i, target := range targets {
		now := r.clock()
		if bounded.Err() != nil || now.Before(started) || now.Sub(started) > ArinShadowCaptureMaxAge {
			return nil, ErrArinShadowInput
		}
		row := arinShadowCapturedConnection{recorder: r, connectionId: target.ConnectionId, capturedAt: now, reason: "owner_unavailable"}
		if f, exists := byId[target.ConnectionId]; exists {
			row.clientId, row.actualAt, row.observedAt = f.ClientId, f.Actual.At, f.ObservedAt
			row.handlerId = f.HandlerId
		}
		if valid[i] {
			row.reason = "facts_unavailable"
			f, exists := byId[target.ConnectionId]
			if exists && f.Present && f.Connected {
				row.reason = "binding_mismatch"
				if f.ClientId == before[i].ClientId && f.HandlerId == before[i].HandlerId {
					row.reason = "facts_stale"
					if !f.ObservedAt.Before(started.Add(-arinShadowCaptureClockSkew)) && !f.ObservedAt.After(now.Add(arinShadowCaptureClockSkew)) &&
						!f.Actual.At.IsZero() && !f.Actual.At.After(f.ObservedAt.Add(arinShadowCaptureClockSkew)) {
						row.facts, row.reason = r.classifyCurrentArinShadow(before[i].Address.Unmap(), f.Actual)
					}
				}
			}
			after, current := currentArinShadowOwner(target.Owner, target.ConnectionId, r.clock())
			if !current || !sameArinShadowOwner(before[i], after) {
				row.reason = "owner_changed"
			}
			row.capturedAt = r.clock()
		}
		rows = append(rows, row)
	}
	if bounded.Err() != nil || r.clock().Before(started) || r.clock().Sub(started) > ArinShadowCaptureMaxAge {
		return nil, ErrArinShadowInput
	}
	return rows, nil
}

func (r *ArinShadowRecorder) classifyCurrentArinShadow(address netip.Addr, actual ArinShadowActiveFacts) (arinShadowFacts, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return arinShadowFacts{}, "lookup_unavailable"
	}
	_, active, err := shadowFacts(r.active, address)
	if err != nil {
		return arinShadowFacts{}, "lookup_unavailable"
	}
	if active.DatabaseBuildEpoch != actual.Epoch || actual.Epoch <= 0 || actual.At.Before(time.Unix(actual.Epoch, 0)) ||
		active.Risk != actual.Risk || active.NonQuality != actual.NonQuality || active.QualityVerified() != actual.Verified {
		return arinShadowFacts{}, "active_mismatch"
	}
	candidate, info, err := shadowFacts(r.candidate, address)
	if err != nil || info.QualityPolicyVersion != 2 {
		return arinShadowFacts{}, "lookup_unavailable"
	}
	return candidate, "qualified"
}
