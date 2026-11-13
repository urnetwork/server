// The control transport retains competition-native scoped authentication.
package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
)

func CompetitionV2SourceHandoff(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2SourceHandoffHandler(w, r)
}
