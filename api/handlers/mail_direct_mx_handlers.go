package handlers

import (
	"errors"
	"net/http"
	"net/netip"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §15.15's direct-MX routes (MAIL-M7-23; R-MAIL-2, D-MAIL-12, I-8, I-15;
// owner decisions 33, 98): POST /mail/direct-mx names up to two of a Team's
// mail indexers answering public TCP 25 themselves, and DELETE /mail/direct-mx
// takes them all back. Network class (api/route_authz.go), so a client token,
// a sidecar's above all, is refused before the handler; the caller must be one
// of the Team's Owners or Admins. Each is an outward action whose approval is
// the admin's own consent to the one-command admin token it runs under (ur
// admin's flow, §7.8). Both are dark, 503 with Retry-After before the body is
// read or the caller authenticated, while the binary does not link MG2, MG5 and
// MG7 or the process serves no dns root, as the other mail routes are.
//
// POST writes pending rows: nothing points at one until mxedge's prober
// verified its address, STARTTLS and published key, when §12.8's records name
// it and the network's {mxr_} routes drop. DELETE drops the rows and the
// records in one transaction, the router interfaces back at mx. No answer or
// log names an address beyond the caller's own rows (I-13).

// One receiver a POST names.
type MailDirectMxReceiverArgs struct {
	// A mail indexer of the network with a published mail host key.
	ReceiverSidecarId server.Id `json:"receiver_sidecar_id"`
	// Its public unicast address, IPv4 or IPv6.
	Address string `json:"address"`
}

// POST /mail/direct-mx's body.
type MailDirectMxArgs struct {
	Receivers []*MailDirectMxReceiverArgs `json:"receivers"`
}

// One receiver as the answer shows it.
type MailDirectMxReceiverResult struct {
	ReceiverSidecarId server.Id `json:"receiver_sidecar_id"`
	Address           string    `json:"address"`
	// pending, verified or failing.
	State string `json:"state"`
	// When its last probe passed, in Unix milliseconds; absent before one did.
	VerifyTime int64 `json:"verify_time,omitempty"`
}

// POST's and DELETE's answer: the network's direct-MX receivers after the
// call, none after a DELETE.
type MailDirectMxResult struct {
	Receivers []*MailDirectMxReceiverResult `json:"receivers"`
}

// The answer of rows.
func mailDirectMxResultOf(rows []*model.MailDirectMx) *MailDirectMxResult {
	result := &MailDirectMxResult{Receivers: []*MailDirectMxReceiverResult{}}
	for _, row := range rows {
		receiver := &MailDirectMxReceiverResult{ReceiverSidecarId: row.ReceiverSidecarId, Address: row.Address.String(), State: string(row.State)}
		if !row.VerifyTime.IsZero() {
			receiver.VerifyTime = row.VerifyTime.UnixMilli()
		}
		result.Receivers = append(result.Receivers, receiver)
	}
	return result
}

// The caller's network, refused unless it is a Team admin's credential.
func mailDirectMxCaller(clientSession *session.ClientSession) (server.Id, error) {
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId != nil || !model.MailDirectMxCallerPermitted(clientSession.Ctx, clientSession) {
		return server.Id{}, router.PrivateCause(http.StatusForbidden, "Only the network's Owner or an Admin names a direct MX.", nil)
	}
	return clientSession.ByJwt.NetworkId, nil
}

// The declared answer to a direct-MX write's refusal, or err itself.
func mailDirectMxRefusal(err error) error {
	var quota *model.QuotaRefusalError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &quota):
		// Team only: the refusal's own 402 and body
		return err
	case errors.Is(err, model.ErrMailDirectMxInvalid), errors.Is(err, dnsservice.ErrDnsDirectMxRefused):
		return router.PrivateCause(http.StatusBadRequest, "Name one or two receivers, each at a public unicast address.", err)
	case errors.Is(err, model.ErrMailDirectMxFull):
		return router.PrivateCause(http.StatusConflict, "The network names two direct-MX receivers already.", err)
	case errors.Is(err, model.ErrMailDirectMxNoReceiver):
		return router.PrivateCause(http.StatusBadRequest, "A receiver named holds no published mail host key for the network's mx.", err)
	case errors.Is(err, model.ErrMailDirectMxMailOff), errors.Is(err, model.ErrDnsDirectMxMailOff):
		return router.PrivateCause(http.StatusConflict, "Mail is off for the network.", err)
	case errors.Is(err, dnsservice.ErrDnsRestoring), errors.Is(err, model.ErrMailUnlinked), errors.Is(err, model.ErrDnsUnlinked), model.IsDnsZoneWaiting(err):
		return router.PrivateCause(http.StatusServiceUnavailable, "A direct MX cannot be changed right now.", err)
	case errors.Is(err, model.ErrDnsZoneSuspended):
		return router.PrivateCause(http.StatusForbidden, "The network's zone is suspended.", err)
	}
	return err
}

// POST /mail/direct-mx: names the receivers, each written pending (see the
// file's comment). Naming one already named changes nothing.
func PutMailDirectMx(w http.ResponseWriter, r *http.Request) {
	if !mailRoutesOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(args *MailDirectMxArgs, clientSession *session.ClientSession) (*MailDirectMxResult, error) {
		networkId, err := mailDirectMxCaller(clientSession)
		if err != nil {
			return nil, err
		}
		receivers := []*model.MailDirectMxReceiver{}
		for _, receiver := range args.Receivers {
			if receiver == nil {
				return nil, mailDirectMxRefusal(model.ErrMailDirectMxInvalid)
			}
			address, err := netip.ParseAddr(receiver.Address)
			if err != nil {
				return nil, mailDirectMxRefusal(model.ErrMailDirectMxInvalid)
			}
			receivers = append(receivers, &model.MailDirectMxReceiver{ReceiverSidecarId: receiver.ReceiverSidecarId, Address: address})
		}
		rows, err := model.NameMailDirectMx(clientSession.Ctx, networkId, receivers)
		if err != nil {
			return nil, mailDirectMxRefusal(err)
		}
		return mailDirectMxResultOf(rows), nil
	}
	router.WrapWithInputRequireAuth(router.PrivateErrorsWithInput(impl), w, r)
}

// DELETE /mail/direct-mx: every receiver and §12.8's records go, the router
// interfaces back at mx.
func DeleteMailDirectMx(w http.ResponseWriter, r *http.Request) {
	if !mailRoutesOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(clientSession *session.ClientSession) (*MailDirectMxResult, error) {
		networkId, err := mailDirectMxCaller(clientSession)
		if err != nil {
			return nil, err
		}
		if _, err := model.ClearMailDirectMx(clientSession.Ctx, networkId, model.DnsUserRoot()); err != nil {
			return nil, mailDirectMxRefusal(err)
		}
		return mailDirectMxResultOf(nil), nil
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}
