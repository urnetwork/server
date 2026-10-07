package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
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
	(*WarpStatusState)(nil).Handler(w, r)
}

// A nil receiver preserves the standalone process latch. A nonnil state is
// owned by one service in a process running several independent listeners.
func (self *WarpStatusState) Handler(w http.ResponseWriter, r *http.Request) {
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

	status := self.Status()

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

// Each composed service owns its readiness. Methods are safe for concurrent
// use. The zero value is pending, so a ready sibling cannot admit this service.
type WarpStatusState struct {
	status atomic.Pointer[string]
}

// Reads only the service's latch; nil retains the historical process status.
func (self *WarpStatusState) Status() string {
	if self == nil {
		if status := warpStatusOverride.Load(); status != nil {
			return *status
		}
		return "ok"
	}
	if status := self.status.Load(); status != nil {
		return *status
	}
	return "error not ready: startup pending"
}

// Stores one service's status without changing a sibling or legacy latch.
func (self *WarpStatusState) set(status string) {
	if self == nil {
		warpStatusOverride.Store(&status)
	} else {
		self.status.Store(&status)
	}
}

// Admits this service after its startup work succeeds.
func (self *WarpStatusState) SetReady() { self.set("ok") }

// Retains the startup failure through graceful shutdown.
func (self *WarpStatusState) SetNotReady(err error) {
	self.set(fmt.Sprintf("error not ready: %s", err))
}

// A concurrent startup failure wins over a graceful drain transition.
func (self *WarpStatusState) SetDrainingIfReady() {
	latch := &warpStatusOverride
	if self != nil {
		latch = &self.status
	}
	for {
		previous := latch.Load()
		if previous != nil && strings.HasPrefix(*previous, "error") || previous == nil && self != nil {
			return
		}
		draining := "draining"
		if latch.CompareAndSwap(previous, &draining) {
			return
		}
	}
}

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
	(*WarpStatusState)(nil).SetDrainingIfReady()
}

func collectStatus(ctx context.Context) (string, error) {
	if status := warpStatusOverride.Load(); status != nil {
		return *status, nil
	}
	return "ok", nil
}

// StartupReadiness runs the one-shot deep checks behind the /status readiness
// latch — the database's successful migration head must be at least the
// binary-required head, then Redis must answer PING, all under a 300s budget —
// and latches the outcome: ready ("ok") or not ready ("error not ready: ...",
// which the deploy poll fails on). Allowing the database to be ahead preserves
// rollback compatibility for older binaries after append-only migrations.
// Call once at service startup, before taking traffic or claiming work. On
// failure the service must keep serving /status rather than exit: the poll then
// times out and warpctl reverts the deploy while the old container keeps
// serving; an exit just flaps the container without ever producing the truthful
// status (TASKDRAIN1 §2.2, APIDRAIN1 §2.1). One-shot by design: /status stays
// O(1) per poll and runtime health remains the monitor's job.
func StartupReadiness(ctx context.Context) error {
	err := CheckStartupReadiness(ctx)
	if err == nil {
		SetWarpStatusReady()
	} else {
		SetWarpStatusNotReady(err)
	}
	return err
}

// CheckStartupReadiness runs the shared deep checks without changing the
// /status latch. Services with additional startup sequencing can use this and
// latch only after all of their own activation work is ready.
func CheckStartupReadiness(ctx context.Context) error {
	return startupReadinessCheckAtMigration(ctx, server.MigrationCount())
}

func startupReadinessCheckAtMigration(ctx context.Context, requiredMigrationVersion int) error {
	if ctx == nil {
		return fmt.Errorf("startup readiness context is absent")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, 300*time.Second)
	defer checkCancel()

	databaseVersion := 0
	if r := server.HandleError(func() {
		server.Db(checkCtx, func(conn server.PgConn) {
			result, err := conn.Query(checkCtx, `
				SELECT COALESCE(MAX(end_version_number), 0)
				FROM migration_audit
				WHERE status = 'success'
			`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					server.Raise(result.Scan(&databaseVersion))
				}
			})
		})
	}); r != nil {
		return startupReadinessReadError("pg", r)
	}
	if err := validateMigrationHead(databaseVersion, requiredMigrationVersion); err != nil {
		return fmt.Errorf("pg: %w", err)
	}

	if r := server.HandleError(func() {
		server.Redis(checkCtx, func(client server.RedisClient) {
			server.Raise(client.Ping(checkCtx).Err())
		})
	}); r != nil {
		return startupReadinessReadError("redis", r)
	}

	return checkCtx.Err()
}

func validateMigrationHead(databaseVersion int, requiredMigrationVersion int) error {
	if databaseVersion < requiredMigrationVersion {
		return fmt.Errorf(
			"database migration head %d is below binary-required head %d",
			databaseVersion,
			requiredMigrationVersion,
		)
	}
	return nil
}
