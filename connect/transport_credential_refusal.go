// A transport admission refused for the credential itself, rather than for a
// failed check, is answered on the carrier's own terms and counted by carrier
// and app release, so the clients that keep retrying refused credentials are
// attributable without ids.
//
// H1+ refuses before its response with a terminal 401 or 403. A WebSocket has
// already sent its 101 and an H3 connection is already up, so their refusal is
// in band: the authorization close an admitted transport gets when its
// credential is refused (session.AuthorizationRejectedCloseCode, or
// session.AuthorizationRevokedCloseCode for a revoked session). Without it a
// WebSocket refusal reads as a dropped connection, which a client redials at
// once, forever. Clients from connect 2026-10-09 read the close as a definitive
// refusal; older clients drop the unknown close control and see the close they
// always saw.
package connect

import (
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/session"
)

var connectCredentialRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "connect", Name: "credential_refusals_total",
	Help: "Transport admissions refused for the credential itself, not for a failed check, by carrier and the client's app release (year.month.day for recent releases, year.month for older ones, unknown or other).",
}, []string{"carrier", "app_version"})

func init() {
	prometheus.MustRegister(connectCredentialRefusals)
}

// A refusal of the credential itself. A malformed instance (400) and an
// unavailable dependency (503) are not.
func connectCredentialRefused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func countConnectCredentialRefusal(carrier string, appVersion string) {
	connectCredentialRefusals.WithLabelValues(carrier, providerAppVersionLabel(appVersion, time.Now())).Inc()
}

// The authorization close code of a credential refused with err.
func connectCredentialRefusalCloseCode(err error) int {
	if errors.Is(err, session.ErrSessionRevoked) {
		return session.AuthorizationRevokedCloseCode
	}
	return session.AuthorizationRejectedCloseCode
}
