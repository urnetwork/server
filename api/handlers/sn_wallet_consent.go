// Original wallet mapping routes share the real API authentication and bounded
// request decoder. Public history returns signatures, never inferred authority.
package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// Only the authenticated network can request its prospective provider mapping.
func SnWalletMappingChallenge(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SnWalletMappingChallenge, w, r)
}

// The caller supplies its independently pinned complete original head.
func SnWalletMappingHistory(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.SnWalletMappingHistory, w, r)
}
