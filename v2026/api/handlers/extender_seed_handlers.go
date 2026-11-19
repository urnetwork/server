package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
)

// ExtenderSeed backs `GET /v1/seed.bin`: the public seed bundle the publisher
// stored, as raw envelope bytes (connect/EXTENDER-UPDATE2.md §2.3). No auth by
// design: the bundle is public, identical on every mirror, and holds only
// signed open-tier records. 404 while the seed is off or nothing is stored.
func ExtenderSeed(w http.ResponseWriter, r *http.Request) {
	controller.ServeExtenderSeed(w, r)
}
