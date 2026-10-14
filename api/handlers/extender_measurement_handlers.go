package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// ExtenderMeasurementCert backs `GET /network/extender-measurement-cert`
// (lag.dev/PLAN.md A8): an extender's poll for its lag.dev measurement
// certificate.
//
// The client jwt is required because the answer is the certificate of an
// extender the calling client activated, with the credential it activated
// with. The optional `public_key_hex` query parameter picks one of several
// extenders a client activated; without it the client's most recently
// activated extender answers.
func ExtenderMeasurementCert(w http.ResponseWriter, r *http.Request) {
	args := &controller.ExtenderMeasurementCertArgs{
		PublicKeyHex: r.URL.Query().Get("public_key_hex"),
	}
	router.WrapRequireClient(
		func(clientSession *session.ClientSession) (*controller.ExtenderMeasurementCertResult, error) {
			return controller.ExtenderMeasurementCert(args, clientSession)
		},
		w,
		r,
	)
}
