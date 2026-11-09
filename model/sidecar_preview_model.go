package model

import (
	"github.com/urnetwork/server"
)

// The preview gate (D-TRUST-3, D-PHASE-3, §7.4's preview row): until stage 2 is
// live in an environment, POST /network/enroll and POST /network/auth-client
// refuse strict_exp: true outside config/<env>/sidecar.yml's preview_networks.
// TEAM-M3-01 removes this gate with stage 2.

// The refusal's text. No code names it yet (question
// core-m1-08-enroll-preview-code), so a client tells it apart by status alone.
const SidecarPreviewRefusedMessage = "This network cannot hold sidecar clients yet."

// Whether the network may hold strict_exp clients now: only one sidecar.yml's
// preview_networks lists. An absent file lists none, and a file that cannot be
// read on a process's first read lists none either, so both refuse; a later
// failed read keeps the last good list (server/sidecar_config.go).
func SidecarPreviewAllowed(networkId server.Id) bool {
	return server.CurrentSidecarConfig().PreviewNetwork(networkId)
}
