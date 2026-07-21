package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type WarpStatusResult struct {
	Version       *string `json:"version,omitempty"`
	ConfigVersion *string `json:"config_version,omitempty"`
	Status        string  `json:"status"`
	ClientAddress string  `json:"client_address"`
	Host          string  `json:"host"`
	Service       string  `json:"service"`
	Block         string  `json:"block"`
}

func WarpStatus(w http.ResponseWriter, r *http.Request) {
	var warpVersion *string
	if version, err := server.Version(); err == nil {
		warpVersion = &version
	} else {
		warpVersion = nil
	}

	var warpConfigVersion *string
	if configVersion, err := server.ConfigVersion(); err == nil {
		warpConfigVersion = &configVersion
	} else {
		warpConfigVersion = nil
	}

	var clientAddress string
	if session, err := session.NewClientSessionFromRequest(r); err == nil {
		clientAddress = session.ClientAddress
	} else {
		clientAddress = fmt.Sprintf("error: %s", err.Error())
	}

	status, err := collectStatus(r.Context())
	if err != nil {
		status = fmt.Sprintf("error: %s", err.Error())
	}

	result := &WarpStatusResult{
		Version:       warpVersion,
		ConfigVersion: warpConfigVersion,
		Status:        status,
		ClientAddress: clientAddress,
		Host:          server.RequireHost(),
		Service:       server.RequireService(),
		Block:         server.RequireBlock(),
	}

	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

// warpStatusOverride is the latched status served by `WarpStatus`. Services
// that run one-shot startup readiness checks latch the outcome
// (SetWarpStatusReady / SetWarpStatusNotReady) and flip to draining at
// SIGTERM (SetWarpStatusDraining); per-poll deep checks are deliberately
// avoided so /status stays O(1) and never couples deploy polls to db load.
// Services that never call the setters keep the historical constant "ok".
//
// The warpctl deploy poll parses the status json and fails a status matching
// `^(?i)error(\s|:)` (warpctl docker.go IsError), polling until the deploy
// times out and reverts. So:
//   - not ready reports "error not ready: ..." (matches) — a container that
//     cannot serve never takes traffic over from a working one;
//   - draining reports "draining" (deliberately NOT an error): the deploy
//     poll never targets the old container, and fleet status sampling must
//     not count an operator-initiated drain as a service error.
var warpStatusOverride atomic.Pointer[string]

func setWarpStatus(status string) {
	warpStatusOverride.Store(&status)
}

// SetWarpStatusReady latches /status to "ok".
func SetWarpStatusReady() {
	setWarpStatus("ok")
}

// SetWarpStatusNotReady latches /status to an error status the warpctl
// deploy poll treats as failed, keeping the container from taking traffic.
func SetWarpStatusNotReady(err error) {
	setWarpStatus(fmt.Sprintf("error not ready: %s", err))
}

// SetWarpStatusDraining marks /status as draining (informational; not an
// error status).
func SetWarpStatusDraining() {
	setWarpStatus("draining")
}

// SetWarpStatusDrainingIfReady marks /status as draining unless a not-ready
// error is latched. A container that failed readiness must keep reporting its
// error through SIGTERM: flipping it to the benign "draining" would hide the
// failure from fleet status sampling. Use this in signal handlers; use
// SetWarpStatusDraining only where the caller knows the service was ready.
func SetWarpStatusDrainingIfReady() {
	if status := warpStatusOverride.Load(); status != nil && strings.HasPrefix(*status, "error") {
		return
	}
	setWarpStatus("draining")
}

func collectStatus(ctx context.Context) (string, error) {
	if status := warpStatusOverride.Load(); status != nil {
		return *status, nil
	}
	return "ok", nil
}

// StartupReadiness runs the one-shot deep checks behind the /status
// readiness latch — pg SELECT 1 and redis PING, under a 15s budget — and
// latches the outcome: ready ("ok") or not ready ("error not ready: ...",
// which the deploy poll fails on). Call once at service startup, before
// taking traffic or claiming work. On failure the service must keep serving
// /status rather than exit: the poll then times out and warpctl reverts the
// deploy while the old container keeps serving; an exit just flaps the
// container without ever producing the truthful status (TASKDRAIN1 §2.2,
// APIDRAIN1 §2.1). One-shot by design: /status stays O(1) per poll and
// runtime health remains the monitor's job.
func StartupReadiness(ctx context.Context) error {
	err := startupReadinessCheck(ctx)
	if err == nil {
		SetWarpStatusReady()
	} else {
		SetWarpStatusNotReady(err)
	}
	return err
}

func startupReadinessCheck(ctx context.Context) error {
	checkCtx, checkCancel := context.WithTimeout(ctx, 15*time.Second)
	defer checkCancel()

	if r := server.HandleError(func() {
		server.Db(checkCtx, func(conn server.PgConn) {
			var one int
			result, err := conn.Query(checkCtx, "SELECT 1")
			server.WithPgResult(result, err, func() {
				for result.Next() {
					server.Raise(result.Scan(&one))
				}
			})
		})
	}); r != nil {
		return fmt.Errorf("pg: %s", r)
	}

	if r := server.HandleError(func() {
		server.Redis(checkCtx, func(client server.RedisClient) {
			server.Raise(client.Ping(checkCtx).Err())
		})
	}); r != nil {
		return fmt.Errorf("redis: %s", r)
	}

	return nil
}
