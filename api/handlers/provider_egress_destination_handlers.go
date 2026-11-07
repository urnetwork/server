package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
)

// Serves the prober's destination pool (connect/GEOMAP.md §11.4): GET
// /network/provider-egress-destinations, under the operator secret every
// egress route uses. The body is the prober module's own Pool type
// (egresshealth.Pool) -- the active destinations with their load contracts,
// the pool version, when it was generated, and the request profile to load
// them with -- which the prober fetches at the start of every pass.
//
// A pool the server cannot build answers 500, and the reason is logged. There
// is no built-in table on the prober: its pass then measures no destination,
// counted under urnetwork_egress_probe_pool_fetches_total{result="unavailable"}
// (SIGNALS.md §2.19h). A country site that collides with the general pool is
// dropped from the pool rather than refusing it.
func ProviderEgressDestinations(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pool, err := controller.GetProviderEgressDestinationPool(r.Context())
	if err != nil {
		glog.Errorf("[pegd]could not build the egress destination pool: %s\n", err)
		http.Error(w, "The destination pool cannot be served.", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pool); err != nil {
		glog.Infof("[pegd]could not write response. err = %s\n", err)
	}
}
