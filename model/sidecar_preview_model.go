package model

import (
	"sync/atomic"

	"github.com/urnetwork/server"
)

// The preview gate (D-TRUST-3, D-PHASE-3, §7.4's preview row): until stage 2 is
// live in an environment, POST /network/enroll and POST /network/auth-client
// refuse strict_exp: true outside config/<env>/sidecar.yml's preview_networks.
// Stage 2 removes it (TEAM-M3-01): once SidecarStage2Live, every network may
// hold sidecar clients.
//
// Stage 2 is live in a binary built with the sidecar_stage2 tag
// (sidecar_stage2_on.go), which waits for D-SDK-11's desktop app and extension
// releases; every other binary keeps stage 1's routes and this gate
// (sidecar_stage2_off.go). The switch is question team-m1-07-stage2-switch.

// The refusal's text. No code names it yet (question
// core-m1-08-enroll-preview-code), so a client tells it apart by status alone.
const SidecarPreviewRefusedMessage = "This network cannot hold sidecar clients yet."

// Whether the network may hold strict_exp clients now: only one sidecar.yml's
// preview_networks lists. An absent file lists none, and a file that cannot be
// read on a process's first read lists none either, so both refuse; a later
// failed read keeps the last good list (server/sidecar_config.go).
func SidecarPreviewAllowed(networkId server.Id) bool {
	if SidecarStage2Live() {
		return true
	}
	return server.CurrentSidecarConfig().PreviewNetwork(networkId)
}

// A test's stage 2 state in place of the build's, nil for the build's.
var sidecarStage2Override atomic.Pointer[bool]

// Whether §7.4's stage 2 is live in this binary: the six moved routes refuse
// every client token (api's stage2NetworkRoutes), and enrollment is open to
// every network.
func SidecarStage2Live() bool {
	if override := sidecarStage2Override.Load(); override != nil {
		return *override
	}
	return sidecarStage2Build
}

// Sets stage 2's state for a test until the returned restore.
func Testing_SetSidecarStage2Live(live bool) func() {
	previous := sidecarStage2Override.Swap(&live)
	return func() {
		sidecarStage2Override.Store(previous)
	}
}
