package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
)

// §14.14's four routes on api (SMTP-M9-19; R-SMTP-10, D-SMTP-5, D-SMTP-9, I-6, I-15). The three Client routes are
// on §7.4's strict_exp allowlist; POST /smtp/appeal is Network class, an Owner's or Admin's with the network
// credential, never a client token (AUTHZ1), and stays off it (api/route_authz.go):
//
//	POST /smtp/dkim-keys   a sidecar's own selectors through its current client, model.PublishSmtpDkimKeys; a
//	                       publish needs §8's mail_send and sidecar.yml's smtpedge gate open to the network, a
//	                       retirement neither
//	GET /smtp/status       the network's limits, today's counts and its halts, any credential of the network
//	GET /smtp/reports      the network's reports and sealed copies, a page at a time, to a sidecar §8 grants read
//	                       on the repo that holds the network mail key
//	POST /smtp/appeal      rechecks the network's halts on §14.8's ladder (smtpedgeservice.SmtpHaltLadder.Lift),
//	                       lifting duplicates and not-spam cancellations; §21 reviews the rest (ABUSE-M9-01)
//
// Each answers 503 with Retry-After, before the body is read or the caller authenticated, while the binary does
// not link its groups, as a closed gate does; a §8 check this process has not installed (model.SetSmtpApiGate)
// answers 503 too. No recipient or client IP is logged (I-13).

// Whether the binary links what the DKIM route writes: MG2's labels, MG5's zones and MG9's keys, and serves a dns
// root.
func smtpDkimKeysRouteOpen() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg9Linked() && model.DnsUserRoot() != ""
}

// The sidecar clientId is the current client of: its binding's, the identity active and the binding its newest
// generation, so an older generation's token speaks for no sidecar (§5.15). False for none.
var smtpClientSidecar = func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool) {
	if _, err := model.GetClientBindingKey(ctx, clientId); err != nil {
		return server.Id{}, false
	}
	view := model.GetSidecarIdentityByClient(ctx, networkId, clientId)
	if view == nil || view.Status != model.SidecarIdentityStatusActive {
		return server.Id{}, false
	}
	return view.SidecarId, true
}

// The answer to a model error: its own status for a declared refusal, 403 for a suspended zone, 503 for what
// cannot be read or written now, and otherwise the error, which PrivateErrors answers 503 with its text kept.
func smtpRouteError(err error, unavailable string) error {
	var apiErr *model.SmtpApiError
	var quotaErr *model.QuotaRefusalError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &apiErr), errors.As(err, &quotaErr):
		return err
	case errors.Is(err, model.ErrDnsZoneSuspended):
		return router.PrivateCause(http.StatusForbidden, "The network's zone is suspended.", err)
	case errors.Is(err, model.ErrSmtpApiUnavailable), errors.Is(err, dnsservice.ErrDnsRestoring), errors.Is(err, model.ErrDnsUnlinked), model.IsDnsZoneWaiting(err):
		return router.PrivateCause(http.StatusServiceUnavailable, unavailable, err)
	case errors.Is(err, model.ErrDnsDkimRefused):
		return router.PrivateCause(http.StatusBadRequest, "A selector's record was refused.", err)
	}
	return err
}

// POST /smtp/dkim-keys: the caller's sidecar.SmtpDkimKeysArgs; answers sidecar.SmtpDkimKeysResult.
func SmtpDkimKeys(w http.ResponseWriter, r *http.Request) {
	if !smtpDkimKeysRouteOpen() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(args sidecar.SmtpDkimKeysArgs, clientSession *session.ClientSession) (*sidecar.SmtpDkimKeysResult, error) {
		ctx := clientSession.Ctx
		networkId := clientSession.ByJwt.NetworkId
		clientId := *clientSession.ByJwt.ClientId
		sidecarId, ok := smtpClientSidecar(ctx, networkId, clientId)
		if !ok {
			return nil, &model.SmtpApiError{Status: http.StatusForbidden, Code: model.SmtpApiCodeNoSidecar, Message: "The caller is no sidecar of the network."}
		}
		now := server.NowUtc()
		if 0 < len(args.Publish) {
			gate := server.CurrentSidecarConfig().Service(sidecar.ServiceGateSmtpedge)
			if !gate.Admits(networkId) || !gate.Writes() {
				return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, "Mail sending is not open to this network.")
			}
		}
		mailSend := func() (bool, error) {
			gate := model.CurrentSmtpApiGate()
			if gate == nil {
				return false, fmt.Errorf("%w: no §8 check is installed", model.ErrSmtpApiUnavailable)
			}
			return gate.MaySendMail(ctx, networkId, sidecarId, now)
		}
		caller := &model.SmtpDkimKeysCaller{NetworkId: networkId, ClientId: clientId, SidecarId: sidecarId}
		result, err := model.PublishSmtpDkimKeys(ctx, caller, &args, mailSend, now, model.DefaultSmtpApiSettings())
		if err != nil {
			return nil, smtpRouteError(err, "The keys cannot be published right now.")
		}
		return result, nil
	}
	router.WrapWithInputRequireClient(router.PrivateErrorsWithInput(impl), w, r)
}

// GET /smtp/status: model.SmtpStatusResult for the bearer's network.
func GetSmtpStatus(w http.ResponseWriter, r *http.Request) {
	if !server.SidecarMg9Linked() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(clientSession *session.ClientSession) (*model.SmtpStatusResult, error) {
		status, err := model.GetSmtpStatus(clientSession.Ctx, clientSession.ByJwt.NetworkId, server.NowUtc(), nil)
		if err != nil {
			return nil, smtpRouteError(err, "The status cannot be read right now.")
		}
		return status, nil
	}
	router.WrapRequireAuth(router.PrivateErrors(impl), w, r)
}

// GET /smtp/reports?since=<unix ms>[&cursor=<the last page's>]: model.SmtpReportsResult, to a sidecar of the
// network that §8 grants read on the repo holding the network mail key.
func GetSmtpReports(w http.ResponseWriter, r *http.Request) {
	if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		server.WriteGateClosed(w, 0)
		return
	}
	query := r.URL.Query()
	impl := func(clientSession *session.ClientSession) (*model.SmtpReportsResult, error) {
		ctx := clientSession.Ctx
		networkId := clientSession.ByJwt.NetworkId
		sinceMs, err := strconv.ParseInt(query.Get("since"), 10, 64)
		if err != nil || sinceMs < 0 {
			return nil, &model.SmtpApiError{Status: http.StatusBadRequest, Code: model.SmtpApiCodeCursor, Message: "since is Unix milliseconds."}
		}
		sidecarId, ok := smtpClientSidecar(ctx, networkId, *clientSession.ByJwt.ClientId)
		if !ok {
			return nil, &model.SmtpApiError{Status: http.StatusForbidden, Code: model.SmtpApiCodeNoSidecar, Message: "The caller is no sidecar of the network."}
		}
		gate := model.CurrentSmtpApiGate()
		if gate == nil {
			return nil, router.PrivateCause(http.StatusServiceUnavailable, "The reports cannot be read right now.", errors.New("no §8 check is installed"))
		}
		allowed, err := gate.MayReadReports(ctx, networkId, sidecarId, server.NowUtc())
		if err != nil {
			return nil, router.PrivateCause(http.StatusServiceUnavailable, "The reports cannot be read right now.", err)
		}
		if !allowed {
			return nil, &model.SmtpApiError{Status: http.StatusForbidden, Code: model.SmtpApiCodeReportsRead, Message: "The sidecar may not read the network's reports."}
		}
		page, err := model.GetSmtpReportsPage(ctx, networkId, time.UnixMilli(sinceMs), query.Get("cursor"), model.DefaultSmtpApiSettings())
		if err != nil {
			return nil, smtpRouteError(err, "The reports cannot be read right now.")
		}
		return page, nil
	}
	router.WrapRequireClient(router.PrivateErrors(impl), w, r)
}

// POST /smtp/appeal's body: the halt to recheck, or none for every halt of the network holding now.
type SmtpAppealArgs struct {
	HaltId *server.Id `json:"halt_id,omitempty"`
}

// What a recheck did with each halt.
type SmtpAppealResult struct {
	Halts []*SmtpAppealHalt `json:"halts"`
}

type SmtpAppealHalt struct {
	HaltId server.Id `json:"halt_id"`
	// Lifted on the recheck: a duplicate's or a cancelled complaint's halt, which leaves every edge within a
	// minute. One that still holds is §21's to review.
	Lifted bool `json:"lifted"`
}

// What the appeal reads and lifts on: the halt rule's store and the halts holding now.
type smtpAppealLadder interface {
	Lift(ctx context.Context, networkId sidecar.Id, haltId sidecar.Id) (bool, error)
}

// The process's ladder for appeals, made on first need from smtpedge.yml, which names the root that labels
// complaints' domains; replaced by tests.
var smtpAppealLadderOf = func() (smtpAppealLadder, error) {
	return defaultSmtpAppealLadder()
}

var defaultSmtpAppealLadder = sync.OnceValues(func() (smtpAppealLadder, error) {
	resource, err := server.Config.SimpleResource("smtpedge.yml")
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	settings, err := smtpedgeservice.LoadSmtpEdgeSettings(data)
	if err != nil {
		return nil, err
	}
	// the ladder reads tiers through Quota, which Lift does not ask; the edges reload the halts it lifts, so it
	// needs no Redis, and it hints no client, so a lift reaches the queue at its next preamble
	quota, err := quotahost.NewLocalQuota(context.Background(), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	store := model.NewSmtpEdgeStore(nil)
	host := &sidecar.ServiceHost{Metrics: sidecarhost.NewMetrics(), Quota: quota, Clock: sidecar.RealClock{}}
	return smtpedgeservice.NewSmtpHaltLadder(host, &smtpedgeservice.SmtpHaltLadderDeps{
		Store:   store,
		Lookups: store,
		Halts:   store,
		Sink:    model.NewAbuseSmtpSink(nil),
	}, settings)
})

// POST /smtp/appeal: SmtpAppealArgs; rechecks the named halt, or each of the network's holding now, and answers
// which lifted.
func SmtpAppeal(w http.ResponseWriter, r *http.Request) {
	if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		server.WriteGateClosed(w, 0)
		return
	}
	impl := func(args SmtpAppealArgs, clientSession *session.ClientSession) (*SmtpAppealResult, error) {
		if clientSession.ByJwt.ClientId != nil {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, router.ClientCredentialRefusedMessage)
		}
		ctx := clientSession.Ctx
		networkId := clientSession.ByJwt.NetworkId
		ladder, err := smtpAppealLadderOf()
		if err != nil {
			return nil, router.PrivateCause(http.StatusServiceUnavailable, "Appeals cannot be taken right now.", err)
		}
		now := server.NowUtc()
		halts, err := model.FindSmtpHalts(ctx, networkId, now.Add(-smtpedgeservice.SmtpHaltDuration))
		if err != nil {
			return nil, smtpRouteError(err, "Appeals cannot be taken right now.")
		}
		result := &SmtpAppealResult{Halts: []*SmtpAppealHalt{}}
		for _, halt := range halts {
			if !halt.ActiveAt(now) || (args.HaltId != nil && server.Id(halt.HaltId) != *args.HaltId) {
				continue
			}
			lifted, err := ladder.Lift(ctx, sidecar.Id(networkId), halt.HaltId)
			if err != nil {
				return nil, router.PrivateCause(http.StatusServiceUnavailable, "Appeals cannot be taken right now.", err)
			}
			result.Halts = append(result.Halts, &SmtpAppealHalt{HaltId: server.Id(halt.HaltId), Lifted: lifted})
		}
		if args.HaltId != nil && len(result.Halts) == 0 {
			// an unknown halt, another network's and one that has ended answer alike
			return nil, fmt.Errorf("%d Unknown halt.", http.StatusNotFound)
		}
		return result, nil
	}
	router.WrapWithInputRequireAuth(router.PrivateErrorsWithInput(impl), w, r)
}
