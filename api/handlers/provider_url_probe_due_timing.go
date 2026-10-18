package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The collector and its fixed label domain live in controller, which the
// taskworker's in-process operator shares; this file keeps the HTTP boundary.
type urlDueTimingOutcome = controller.ProviderUrlProbeDueOutcome

const (
	urlDueTimingReturned    = controller.ProviderUrlProbeDueReturned
	urlDueTimingMaintenance = controller.ProviderUrlProbeDueMaintenance
	urlDueTimingEncodeError = controller.ProviderUrlProbeDueEncodeError
	urlDueTimingPanic       = controller.ProviderUrlProbeDuePanic
)

var urlDueModelPhaseLabels = controller.ProviderUrlProbeDueModelPhaseLabels

var urlDueDatabasePhaseLabels = controller.ProviderUrlProbeDueDatabasePhaseLabels

// Only the URL route publishes these phases. The old egress EDF collector does
// not describe the durable URL path. Neither collector is receipt evidence.
type providerUrlProbeDueTimingCollectors = controller.ProviderUrlProbeDueTiming

var providerUrlProbeDueTiming = newProviderUrlProbeDueTimingCollectors(prometheus.DefaultRegisterer)

func newProviderUrlProbeDueTimingCollectors(registerer prometheus.Registerer) *providerUrlProbeDueTimingCollectors {
	return controller.NewProviderUrlProbeDueTiming(registerer)
}

type providerUrlProbeDueClaim = controller.ProviderUrlProbeDueClaim

// This boundary runs only after the existing authorization and argument checks.
// A defer retains partial timings without recovering or replacing a model panic.
// The injected callable keeps handler failure tests independent of a database.
func respondProviderUrlProbeDue(w http.ResponseWriter, r *http.Request, limit, shardIndex, shardCount int, claim providerUrlProbeDueClaim, metrics *providerUrlProbeDueTimingCollectors) {
	started := time.Now()
	var observation model.ProviderUrlProbeDueObservation
	var encode server.DbTimingSample
	outcome := urlDueTimingPanic
	defer func() { metrics.Observe(observation, time.Since(started), encode, outcome) }()
	// Stored timestamps hold UTC without a timezone; keep the same UTC clock.
	result := claim(r.Context(), server.NowUtc(), limit, shardIndex, shardCount, &observation)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	func() {
		encodeStarted := time.Now()
		defer func() { encode = server.DbTimingSample{Count: 1, Duration: time.Since(encodeStarted)} }()
		encodeErr = json.NewEncoder(w).Encode(result)
	}()
	if encodeErr != nil {
		outcome = urlDueTimingEncodeError
		glog.Infof("[pegl]could not write response. err = %s\n", encodeErr)
	} else if result.PriorityMaintenancePending {
		outcome = urlDueTimingMaintenance
	} else {
		outcome = urlDueTimingReturned
	}
}
