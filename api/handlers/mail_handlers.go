package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §15.15's mail routes on api (MAIL-M7-22; R-MAIL-11, D-MAIL-1; QD-478): POST
// /mail/enable and GET /mail/status, Client class on §7.4's strict_exp
// allowlist (api/route_authz.go). Both are dark, 503 with Retry-After before
// the body is read or the caller authenticated, while the binary does not link
// MG2, MG5 and MG7 or the process serves no dns root, as a closed gate is.
// POST /mail/enable also asks sidecar.yml's mxedge gate to admit the network
// and take writes; a status is read whatever the gate says, so a receiver
// still learns that mail is off.

// Whether the mail routes are open in this process.
func mailRoutesOpen() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg7Linked() && model.DnsUserRoot() != ""
}

// POST /mail/enable's answer.
type MailEnableResult struct {
	// When mail was first turned on for the network, in Unix milliseconds:
	// §14.7's 24 h send gate counts from it. Enabling again answers the same.
	EnabledTime int64 `json:"enabled_time"`
}

// POST /mail/enable: an empty body. Turns mail on for the bearer's network:
// its mail_network row and §12's mail records, the wildcard MX included, in
// one transaction, then its {mxr_} routes (model.EnableMail). network_verified
// is not asked. Idempotent.
func MailEnable(w http.ResponseWriter, r *http.Request) {
	if !mailRoutesOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(clientSession *session.ClientSession) (*MailEnableResult, error) {
		networkId := clientSession.ByJwt.NetworkId
		gate := server.CurrentSidecarConfig().Service(sidecar.ServiceGateMxedge)
		if !gate.Admits(networkId) || !gate.Writes() {
			return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, "Mail is not open to this network.")
		}
		mailNetwork, err := model.EnableMail(clientSession.Ctx, networkId, model.DnsUserRoot())
		switch {
		case err == nil:
			return &MailEnableResult{EnabledTime: mailNetwork.CreateTime.UnixMilli()}, nil
		case errors.Is(err, dnsservice.ErrDnsRestoring), errors.Is(err, model.ErrMailUnlinked), errors.Is(err, model.ErrDnsUnlinked), model.IsDnsZoneWaiting(err):
			return nil, router.PrivateCause(http.StatusServiceUnavailable, "Mail cannot be turned on right now.", err)
		case errors.Is(err, model.ErrDnsZoneSuspended):
			return nil, router.PrivateCause(http.StatusForbidden, "The network's zone is suspended.", err)
		}
		return nil, err
	}
	router.WrapRequireClient(router.PrivateErrors(impl), w, r)
}

// GET /mail/status: {enabled, storage_full, spent, direct_mx} for the bearer's
// network (model.MailStatus). A receiver reads it at most every 30 s for
// RCPT's 452 4.2.2.
func GetMailStatus(w http.ResponseWriter, r *http.Request) {
	if !mailRoutesOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(clientSession *session.ClientSession) (*model.MailStatus, error) {
		return model.GetMailStatus(clientSession.Ctx, clientSession.ByJwt.NetworkId)
	}
	router.WrapRequireClient(router.PrivateErrors(impl), w, r)
}
