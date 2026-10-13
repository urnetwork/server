// A hosted owner bounds the derived-client registrations its devices run at
// once. One registration is a database transaction that writes the child client
// and its credential state. A proxy process hosts more than a thousand devices
// behind one small connection pool, so without a bound a burst of window
// re-dials (a deploy, a provider outage, a slow database) queued enough mints
// to exhaust the pool for every other control operation on the host, and each
// timed-out mint was retried. Past the bound a registration is refused at once,
// before any credential or database read, as a rate limit: the SDK answers it
// with its jittered rate-limit backoff instead of retrying.
package localclient

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
)

var mintAdmissionRefusals = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "mint_admission_refusals_total",
		Help:      "Hosted derived-client registrations refused because the owner's concurrent registration bound was full",
	},
)

func init() {
	prometheus.MustRegister(mintAdmissionRefusals)
}

// The concurrent registration bound shared by the authorities of one owner. A
// nil admission admits everything. Safe for concurrent use.
type MintAdmission struct {
	slots chan struct{}
}

// Nonpositive `maxConcurrentCount` returns nil, which admits everything.
func NewMintAdmission(maxConcurrentCount int) *MintAdmission {
	if maxConcurrentCount <= 0 {
		return nil
	}
	return &MintAdmission{
		slots: make(chan struct{}, maxConcurrentCount),
	}
}

// Takes a slot without waiting. A true return must be paired with release.
func (self *MintAdmission) tryAcquire() bool {
	if self == nil {
		return true
	}
	select {
	case self.slots <- struct{}{}:
		return true
	default:
		mintAdmissionRefusals.Inc()
		return false
	}
}

func (self *MintAdmission) release() {
	if self == nil {
		return
	}
	<-self.slots
}

// The refusal reads as a rate limit to the SDK's window classifier, which
// backs off with jitter. No mint was attempted, so nothing needs retirement.
func mintAdmissionRefusedError() error {
	return &connect.HttpStatusError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       []byte("hosted client registration is at its concurrent bound"),
	}
}

// The owner's bound this authority registers through, for owner wiring tests.
func (self *Authority) Testing_MintAdmission() *MintAdmission {
	return self.mintAdmission
}
