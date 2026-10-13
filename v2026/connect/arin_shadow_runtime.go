package connect

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type ArinShadowHandlerInventory struct {
	Handlers            []server.Id                   `json:"handlers"`
	TrackedConnections  int                           `json:"tracked_connections"`
	OverflowConnections int                           `json:"overflow_connections"`
	Complete            bool                          `json:"complete"`
	Resources           server.ArinShadowResourcePins `json:"resources"`
}

// Current-cause inventory is a separate bounded method. It observes registry
// membership and the already-published serving-reader epoch without opening
// the legacy capture recorder or advertising whole-file hash identity.
type ArinCurrentCauseHandlerInventory struct {
	Handlers            []server.Id `json:"handlers"`
	TrackedConnections  int         `json:"tracked_connections"`
	OverflowConnections int         `json:"overflow_connections"`
	Complete            bool        `json:"complete"`
	ReaderEpoch         int64       `json:"reader_epoch"`
}

func startArinShadowCaptureRuntime(ctx context.Context) (*server.ArinShadowRuntime, error) {
	config, err := server.LoadArinShadowRuntimeConfig()
	if err != nil || config == nil {
		return nil, err
	}
	return server.StartArinShadowRuntime(ctx, config, "connect", func(lifetime context.Context) (server.ArinShadowRPCHandler, func(), error) {
		return newArinShadowConnectRPC(lifetime, config)
	})
}

func newArinShadowConnectRPC(lifetime context.Context, config *server.ArinShadowRuntimeConfig) (server.ArinShadowRPCHandler, func(), error) {
	// Opening a candidate is capture work, not service startup work. A
	// disabled or not-yet-invoked endpoint retains no candidate mapping.
	var recorder *server.ArinShadowRecorder
	closed := false
	release := func() {
		if recorder != nil {
			recorder.Close()
			recorder = nil
		}
	}
	close := func() {
		closed = true
		release()
	}
	open := func() error {
		if closed {
			return server.ErrArinShadowInput
		}
		if recorder == nil {
			var err error
			recorder, err = server.OpenArinShadowCaptureRecorder(config.ActivePath, config.ActiveSHA256, config.CandidatePath, config.CandidateSHA256, server.NowUtc(), config.Capacity)
			return err
		}
		return nil
	}
	owners := func(ctx context.Context, ids []server.Id) ([]server.ArinShadowCaptureTarget, error) {
		if ctx.Err() != nil {
			return nil, server.ErrArinShadowInput
		}
		targets, _, err := CaptureArinShadowOwners(ids)
		return targets, err
	}
	handler := func(ctx context.Context, method string, input json.RawMessage) (any, error) {
		if method == "capture" || method == "inventory" {
			if err := open(); err != nil {
				return nil, err
			}
		}
		switch method {
		case server.ArinCurrentCauseInventoryMethod:
			var empty struct{}
			if closed || server.DecodeArinShadowRPC(input, &empty) != nil {
				return nil, server.ErrArinShadowInput
			}
			r := currentArinShadowOwners
			r.mu.Lock()
			defer r.mu.Unlock()
			result := ArinCurrentCauseHandlerInventory{Handlers: []server.Id{}, TrackedConnections: len(r.entries), OverflowConnections: r.overflow,
				Complete: len(r.handlers) <= 256 && r.overflow == 0, ReaderEpoch: server.CurrentArinCauseReaderEpoch()}
			if len(r.handlers) <= 256 {
				for handler := range r.handlers {
					result.Handlers = append(result.Handlers, handler)
				}
			}
			slices.SortFunc(result.Handlers, func(a, b server.Id) int {
				if a.Less(b) {
					return -1
				}
				if b.Less(a) {
					return 1
				}
				return 0
			})
			return result, nil
		case server.ArinCurrentCauseMethod:
			if closed {
				return nil, server.ErrArinShadowInput
			}
			return server.ArinCurrentCauseRPC(ctx, input, owners, model.ReadArinShadowCaptureFacts)
		case "capture":
			return recorder.CaptureRPC(ctx, input, owners, model.ReadArinShadowCaptureFacts)
		case "inventory":
			var empty struct{}
			if server.DecodeArinShadowRPC(input, &empty) != nil {
				return nil, server.ErrArinShadowInput
			}
			pins, err := recorder.ResourcePins()
			if err != nil {
				return nil, err
			}
			r := currentArinShadowOwners
			r.mu.Lock()
			defer r.mu.Unlock()
			result := ArinShadowHandlerInventory{TrackedConnections: len(r.entries), OverflowConnections: r.overflow, Complete: len(r.handlers) <= 256 && r.overflow == 0, Resources: pins}
			if len(r.handlers) <= 256 {
				for handler := range r.handlers {
					result.Handlers = append(result.Handlers, handler)
				}
			}
			slices.SortFunc(result.Handlers, func(a, b server.Id) int {
				if a.Less(b) {
					return -1
				}
				if b.Less(a) {
					return 1
				}
				return 0
			})
			return result, nil
		case "release":
			var empty struct{}
			if server.DecodeArinShadowRPC(input, &empty) != nil {
				return nil, server.ErrArinShadowInput
			}
			release()
			return struct{}{}, nil
		default:
			return nil, server.ErrArinShadowInput
		}
	}
	return handler, close, nil
}
