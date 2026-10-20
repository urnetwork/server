// Hotkey wallet mapping routes share the real API authentication and bounded
// request decoder. Public history returns signatures, never inferred authority.
package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

// Only the authenticated network owner (a network JWT) submits the complete
// global chain, which links the network to its hotkey.
func SnHotkeyWalletMappingConsent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SnHotkeyWalletMappingConsent, w, r)
}

// The caller supplies the global chain's independently pinned head.
func SnHotkeyWalletMappingHistory(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.SnHotkeyWalletMappingHistory, w, r)
}

// Only the authenticated network owner (a network JWT) can request the
// network's prospective delegation.
func SnHotkeyNetworkDelegationChallenge(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SnHotkeyNetworkDelegationChallenge, w, r)
}

// The caller supplies the delegation chain's independently pinned head.
func SnHotkeyNetworkDelegationHistory(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.SnHotkeyNetworkDelegationHistory, w, r)
}
