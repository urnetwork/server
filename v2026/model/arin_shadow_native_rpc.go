package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server/v2026"
)

type ArinShadowNativeLeaseInfo struct {
	Token             server.Id `json:"token"`
	GenerationSHA256  string    `json:"generation_sha256"`
	SourceStartedAt   time.Time `json:"source_started_at"`
	SourceCompletedAt time.Time `json:"source_completed_at"`
	PublishedAt       time.Time `json:"published_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type ArinShadowNativeMember struct {
	ClientId      server.Id `json:"client_id"`
	Buckets       []string  `json:"buckets"`
	BaseQuality   bool      `json:"base_quality"`
	BaseSpeed     bool      `json:"base_speed"`
	ActiveQuality bool      `json:"active_quality"`
	ActiveSpeed   bool      `json:"active_speed"`
	Unavailable   bool      `json:"unavailable"`
}

type ArinShadowNativeMemberRequest struct {
	Token   server.Id   `json:"token"`
	Clients []server.Id `json:"clients"`
}

type ArinShadowNativeEnd struct {
	GenerationSHA256 string    `json:"generation_sha256"`
	At               time.Time `json:"at"`
}

type ArinShadowNativeStatus struct {
	Available         bool      `json:"available"`
	GenerationSHA256  string    `json:"generation_sha256,omitempty"`
	SourceCompletedAt time.Time `json:"source_completed_at,omitzero"`
}

// Installs only an observer of the existing publisher; it never schedules a
// score update. The protected RPC owns its lifecycle and one immutable lease.
func NewArinShadowNativeRPC(lifetime context.Context, capacity int) (server.ArinShadowRPCHandler, func(), error) {
	capture, err := NewArinShadowScoreCapture(capacity)
	if err != nil {
		return nil, nil, err
	}
	InstallArinShadowScoreCapture(capture)
	var lease *ArinShadowScoreLease
	release := func() {
		if lease != nil {
			lease.Close()
			lease = nil
		}
	}
	close := func() {
		release()
		capture.Close()
	}
	handle := func(ctx context.Context, method string, input json.RawMessage) (any, error) {
		switch method {
		case "native_status":
			var empty struct{}
			if server.DecodeArinShadowRPC(input, &empty) != nil {
				return nil, server.ErrArinShadowInput
			}
			snapshot := capture.Snapshot()
			if snapshot == nil || !snapshot.Validate(ctx) {
				return ArinShadowNativeStatus{}, nil
			}
			return ArinShadowNativeStatus{true, snapshot.generation, snapshot.sourceCompletedAt}, nil
		case "native_release":
			var request struct {
				Token server.Id `json:"token"`
			}
			if server.DecodeArinShadowRPC(input, &request) != nil || lease == nil || request.Token != lease.token {
				return nil, server.ErrArinShadowInput
			}
			release()
			return struct{}{}, nil
		case "native_acquire":
			var empty struct{}
			if lease != nil {
				if _, ok := lease.current(ctx); !ok {
					release()
				}
			}
			if server.DecodeArinShadowRPC(input, &empty) != nil || lease != nil {
				return nil, server.ErrArinShadowInput
			}
			snapshot := capture.Snapshot()
			if snapshot == nil || !snapshot.Validate(ctx) {
				return nil, server.ErrArinShadowInput
			}
			// The RPC's five-second transport deadline must not become the
			// ninety-second generation lease; service shutdown still revokes it.
			expires := server.NowUtc().Add(server.ArinShadowCaptureMaxAge)
			if deadline, ok := lifetime.Deadline(); ok && deadline.Before(expires) {
				expires = deadline
			}
			lease, err = snapshot.acquireCaptureLease(ctx, expires)
			if err != nil {
				return nil, err
			}
			return ArinShadowNativeLeaseInfo{lease.token, snapshot.generation, snapshot.sourceStartedAt, snapshot.sourceCompletedAt, snapshot.publishedAt, lease.expiresAt}, nil
		case "native_members":
			var request ArinShadowNativeMemberRequest
			if server.DecodeArinShadowRPC(input, &request) != nil || lease == nil || request.Token != lease.token || len(request.Clients) == 0 || len(request.Clients) > server.ArinShadowCaptureBatchLimit {
				return nil, server.ErrArinShadowInput
			}
			snapshot, ok := lease.current(ctx)
			if !ok {
				return nil, server.ErrArinShadowInput
			}
			seen := map[server.Id]bool{}
			members := make([]ArinShadowNativeMember, len(request.Clients))
			for i, id := range request.Clients {
				if id == (server.Id{}) || seen[id] {
					return nil, server.ErrArinShadowInput
				}
				seen[id] = true
				member := snapshot.member(id, 1)
				members[i] = ArinShadowNativeMember{id, member.Buckets, member.BaseQuality, member.BaseSpeed, member.ActiveQuality, member.ActiveSpeed, member.MembershipUnavailable}
			}
			return members, nil
		case "native_end":
			var request struct {
				Token server.Id `json:"token"`
			}
			if server.DecodeArinShadowRPC(input, &request) != nil || lease == nil || request.Token != lease.token {
				return nil, server.ErrArinShadowInput
			}
			if _, ok := lease.current(ctx); !ok {
				return nil, server.ErrArinShadowInput
			}
			latest, err := GetClientScoreNativeCensus(ctx)
			if err != nil {
				return nil, server.ErrArinShadowInput
			}
			generation := arinShadowNativeGeneration(latest)
			if generation == "" {
				return nil, server.ErrArinShadowInput
			}
			release()
			return ArinShadowNativeEnd{generation, server.NowUtc()}, nil
		default:
			return nil, server.ErrArinShadowInput
		}
	}
	return handle, close, nil
}
