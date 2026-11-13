package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The lag.dev measurement certificate tasks (lag.dev/PLAN.md C5-C9). Two
// chains, each one task a minute:
//
//   - ExtenderMeasurementDns withdraws the live certificates of extenders that
//     were moved to the gated tier or a canary channel, or deactivated for
//     longer than controller.ExtenderMeasurementAwayGrace (C9, amendment 23),
//     each judged again under its extender's lock (amendment 24), then makes
//     the zone's address records exactly the extenders that serve,
//     each name at its extender's active addresses, and records which names
//     are published (C8). It is quick -- reads and one Route 53 listing -- so
//     a deactivation leaves dns, and an address change reaches it, within
//     about a minute. Only a pass that publishes a name waits, until Route 53
//     reports the name's change in sync, since the lag.dev sample offers a
//     name only once it is published (A4).
//   - ExtenderMeasurementIssue revokes what was withdrawn (C9), renews what is
//     due (C6) and issues for pending requests under the weekly cap, spread
//     across countries and networks (C5, C7, amendment 24), asking again for
//     an extender that came back after its request was withdrawn (C9). An
//     order waits on Route 53 and the CA for a minute or more, so this
//     chain is apart from the dns one, runs alone with a longer limit, and
//     starts no new order once its budget is spent. Each certificate it
//     stores wakes the dns chain, so the name is published right after its
//     issuance rather than at the dns chain's next minute (C8).
//
// The reconcile runs only in the dns chain, whose run-once key keeps one pass
// at a time: two at once could each delete what the other just wrote.
//
// Activation does not wake the issuance chain: the pending request is the
// queue, and the next pass, at most a minute away, takes it (A10).
//
// Both chains do nothing while the measurement block is disabled, and
// neither fails its task for a failure of one certificate: a failed order
// backs off on its own row, or waits out the CA's Retry-After when the CA
// refused it under a rate limit, a refusal of a new certificate holding every
// new certificate until then (amendment 24), and the chain re-arms either
// way.

const (
	// How often each chain runs.
	ExtenderMeasurementIssueTimeout = 1 * time.Minute
	ExtenderMeasurementDnsTimeout   = 1 * time.Minute

	// The issuance task's limit, and the part of it in which a new order
	// may still start: past the budget, an order already started finishes
	// within ExtenderMeasurementOrderTimeout.
	ExtenderMeasurementIssueMaxTime = 20 * time.Minute
	ExtenderMeasurementIssueBudget  = 8 * time.Minute

	// The dns task's limit, past the longest wait for a change to be in sync.
	ExtenderMeasurementDnsMaxTime = ExtenderMeasurementInsyncTimeout + 2*time.Minute

	// Orders one pass places at most, new and renewal together.
	ExtenderMeasurementMaxOrdersPerPass = 8

	// Requests and certificates one pass reads.
	ExtenderMeasurementPendingLimit = 256
	ExtenderMeasurementRenewalLimit = 64
	ExtenderMeasurementRevokeLimit  = 64

	// The window of the weekly cap (C7).
	ExtenderMeasurementCapWindow = 7 * 24 * time.Hour

	// The backoff after a failed order or revocation: doubling from the
	// first, up to the most. The CA allows five failed validations of a name
	// an hour and pauses a name that keeps failing, and it issues the same
	// name at most five times in seven days, so a name that keeps failing is
	// tried four times in its first hour and then ever more rarely
	// (lag.dev/ops/letsencrypt.md). A refusal under a rate limit waits for
	// the CA's Retry-After instead (extenderMeasurementRetryTime, amendment
	// 24).
	ExtenderMeasurementFirstRetryTimeout = 5 * time.Minute
	ExtenderMeasurementMaxRetryTimeout   = 72 * time.Hour
)

// The wait before attempt `failureCount + 1`.
func extenderMeasurementRetryTimeout(failureCount int) time.Duration {
	timeout := ExtenderMeasurementFirstRetryTimeout
	for range min(max(failureCount, 0), 16) {
		timeout *= 2
		if ExtenderMeasurementMaxRetryTimeout <= timeout {
			return ExtenderMeasurementMaxRetryTimeout
		}
	}
	return timeout
}

// How long past the CA's Retry-After the next attempt waits (amendment 24).
// The CA's clock and ours may differ by seconds, and a Retry-After date names
// a whole second, so an attempt at the very moment could be refused again.
// The issuance chain runs once a minute anyway, so the margin delays the
// attempt by at most one pass.
const (
	ExtenderMeasurementRateLimitMargin = 1 * time.Minute
)

// The time of the next attempt after a failed order, renewal or revocation
// at `now` (C5, C6, C9). A refusal under one of the CA's rate limits, a
// rateLimited problem with a Retry-After, is tried again once the Retry-After
// and ExtenderMeasurementRateLimitMargin have passed, sooner or later than the
// backoff would have it: the CA says when the name, the domain or the account
// has room again (lag.dev/ops/letsencrypt.md §5.1), and the doubling by
// failures is only a guess at that, which can miss it by most of a day
// (amendment 24). Any other failure, a refusal without a Retry-After among
// them, backs off by `failureCount`, the failures before this one
// (extenderMeasurementRetryTimeout).
//
// The client returns a refusal at once rather than retrying it
// (extenderMeasurementAcmeRetryBackoff), and wraps the CA's error, so it is
// unwrapped before acme.RateLimit reads it.
//
// The refusal still counts as a failure of its row, as every failure does
// (model.DeferPendingNetworkExtenderMeasurement and the like): the count only
// spaces the attempts the CA gives no time for, it starts over with the next
// certificate, and a name the CA keeps refusing is one to try more rarely when
// it then fails some other way.
func extenderMeasurementRetryTime(err error, failureCount int, now time.Time) time.Time {
	var acmeErr *acme.Error
	if errors.As(err, &acmeErr) {
		if retryAfter, ok := acme.RateLimit(acmeErr); ok && 0 < retryAfter {
			return now.Add(retryAfter + ExtenderMeasurementRateLimitMargin)
		}
	}
	return now.Add(extenderMeasurementRetryTimeout(failureCount))
}

type ExtenderMeasurementIssueArgs struct {
}

type ExtenderMeasurementIssueResult struct {
	Revoked int `json:"revoked"`
	Renewed int `json:"renewed"`
	Issued  int `json:"issued"`
	Refused int `json:"refused"`
	Waiting int `json:"waiting"`
	Failed  int `json:"failed"`
}

func ScheduleExtenderMeasurementIssue(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleExtenderMeasurementIssueAt(clientSession, tx, server.NowUtc().Add(ExtenderMeasurementIssueTimeout))
}

func scheduleExtenderMeasurementIssueAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderMeasurementIssue,
		&ExtenderMeasurementIssueArgs{},
		clientSession,
		task.RunOnce("extender_measurement_issue"),
		task.RunAt(runAt),
		task.MaxTime(ExtenderMeasurementIssueMaxTime),
	)
}

// ExtenderMeasurementIssue is one pass of the issuance chain (C5, C6, C7, C9).
func ExtenderMeasurementIssue(
	_ *ExtenderMeasurementIssueArgs,
	clientSession *session.ClientSession,
) (*ExtenderMeasurementIssueResult, error) {
	result := &ExtenderMeasurementIssueResult{}
	config, err := controller.EnvExtenderConfig()
	if err != nil || !config.Measurement.Enabled {
		return result, nil
	}
	issueExtenderMeasurementCerts(clientSession, config, server.NowUtc(), result)
	if 0 < result.Revoked+result.Renewed+result.Issued+result.Refused+result.Failed {
		glog.Infof(
			"[extendermeasurement]revoked %d, renewed %d, issued %d, refused %d, waiting %d, failed %d\n",
			result.Revoked,
			result.Renewed,
			result.Issued,
			result.Refused,
			result.Waiting,
			result.Failed,
		)
	}
	return result, nil
}

func ExtenderMeasurementIssuePost(
	_ *ExtenderMeasurementIssueArgs,
	_ *ExtenderMeasurementIssueResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExtenderMeasurementIssue(clientSession, tx)
	return nil
}

// One pass of the issuance chain at `now`, into `result`: revocations first,
// since a withdrawn certificate should stop being valid as soon as it can,
// then renewals, which keep serving extenders serving, then new certificates.
// Each live certificate stored wakes the dns chain.
func issueExtenderMeasurementCerts(
	clientSession *session.ClientSession,
	config *controller.ExtenderConfig,
	now time.Time,
	result *ExtenderMeasurementIssueResult,
) {
	ctx := clientSession.Ctx
	acmeClient, err := extenderMeasurementAcmeForConfig(config)
	if err != nil {
		glog.Errorf("[extendermeasurement]no acme account, nothing is issued: %s\n", err)
		return
	}

	revokeExtenderMeasurementCerts(ctx, acmeClient, now, result)

	zone, err := openExtenderMeasurementZone(ctx, config)
	if err != nil {
		glog.Errorf("[extendermeasurement]the measurement zone is not usable, nothing is issued: %s\n", err)
		return
	}

	budgetEnd := now.Add(ExtenderMeasurementIssueBudget)
	orders := 0
	mayOrder := func() bool {
		return orders < ExtenderMeasurementMaxOrdersPerPass && server.NowUtc().Before(budgetEnd) && ctx.Err() == nil
	}

	// a stored certificate's name is published by the dns chain, woken at
	// once rather than at its next minute (C8)
	stored := func(cert *model.NetworkExtenderMeasurementCert) {
		if model.AddNetworkExtenderMeasurementCert(ctx, cert) {
			wakeExtenderMeasurementDns(clientSession)
		}
	}

	renewExtenderMeasurementCerts(ctx, config, acmeClient, zone, now, mayOrder, &orders, stored, result)
	issuePendingExtenderMeasurementCerts(ctx, config, acmeClient, zone, now, mayOrder, &orders, stored, result)
}

// Revokes withdrawn certificates (C9). A failure backs off on the
// certificate's row; a certificate that expires meanwhile needs no
// revocation and drops out of the queue.
func revokeExtenderMeasurementCerts(
	ctx context.Context,
	acmeClient *extenderMeasurementAcme,
	now time.Time,
	result *ExtenderMeasurementIssueResult,
) {
	for _, cert := range model.GetNetworkExtenderMeasurementCertsToRevoke(ctx, now, ExtenderMeasurementRevokeLimit) {
		if ctx.Err() != nil {
			return
		}
		leaf, err := extenderMeasurementLeaf(cert.ChainPem)
		if err == nil {
			revokeCtx, revokeCancel := context.WithTimeout(ctx, ExtenderMeasurementAcmeRequestTimeout)
			err = acmeClient.revoke(revokeCtx, leaf.Raw)
			revokeCancel()
		}
		if err != nil {
			glog.Errorf("[extendermeasurement]%s not revoked: %s\n", cert.CertId, err)
			// a refusal under a rate limit waits out the CA's Retry-After, and
			// any other failure backs off (C9, amendment 24)
			model.DeferNetworkExtenderMeasurementCertRevocation(
				ctx,
				cert.CertId,
				extenderMeasurementRetryTime(err, cert.RevokeFailureCount, server.NowUtc()),
			)
			result.Failed += 1
			continue
		}
		model.MarkNetworkExtenderMeasurementCertRevoked(ctx, cert.CertId, server.NowUtc())
		result.Revoked += 1
	}
}

const (
	// How long the renewal of a due certificate is put off when the pass may
	// not renew it: its extender may not be measured now, or it is for
	// another zone's name (C6, amendment 24). A certificate falls due with a
	// third of its lifetime to run, so once its extender may be measured
	// again its renewal waits at most this long, and only one with less than
	// this left runs out first.
	ExtenderMeasurementRenewalPostponeTimeout = 1 * time.Hour
)

// Renews the live certificates that are due (C6): two thirds of the way
// through their lifetime, since the CA's renewal information is not read. A
// renewal reuses the stored request -- the extender's latest when that is for
// the same name, so a key the extender rotated is followed, else the
// certificate's own -- and is a renewal to the CA too, outside the weekly cap
// (C7). A certificate of an extender that may not be measured now, or that is
// for another zone's name, is not renewed, and its renewal is put off by
// ExtenderMeasurementRenewalPostponeTimeout without counting a failure
// (amendment 24). A tcp port moved off the measurement port, the loss of the
// extender's public inbound address and a change of zone withdraw nothing
// (C9), so left due, such certificates would come back at the head of every
// read until they expired, and enough of them could fill its limit and keep
// every serving extender's renewal from being read. One of an extender that is
// not active is not read (model.GetNetworkExtenderMeasurementCertsForRenewal):
// it is renewed when the extender comes back within the grace, and withdrawn
// by the dns chain when it does not (amendment 23).
func renewExtenderMeasurementCerts(
	ctx context.Context,
	config *controller.ExtenderConfig,
	acmeClient *extenderMeasurementAcme,
	zone *extenderMeasurementZone,
	now time.Time,
	mayOrder func() bool,
	orders *int,
	stored func(*model.NetworkExtenderMeasurementCert),
	result *ExtenderMeasurementIssueResult,
) {
	measurementZone := config.Measurement.MeasurementZone()
	for _, target := range model.GetNetworkExtenderMeasurementCertsForRenewal(ctx, now, ExtenderMeasurementRenewalLimit) {
		if ctx.Err() != nil {
			return
		}
		cert := target.Cert
		// one this pass may not renew is put off rather than left due, where
		// every pass would read it first until it expired (amendment 24)
		postpone := func() {
			model.PostponeNetworkExtenderMeasurementCertRenewal(
				ctx,
				cert.CertId,
				now.Add(ExtenderMeasurementRenewalPostponeTimeout),
				now,
			)
		}
		if _, reason := controller.ExtenderMeasurementEligibleAddresses(target.Extender, target.Addresses); reason != "" {
			postpone()
			continue
		}
		names := []string{controller.ExtenderMeasurementName(cert.ExtenderId, measurementZone)}
		if !slices.Equal(cert.Names, names) {
			// a certificate for another zone's name
			postpone()
			continue
		}
		if !mayOrder() {
			return
		}

		csr := cert.Csr
		publicKey := cert.PublicKey
		if measurement := target.Measurement; measurement != nil && measurement.Csr != nil && slices.Equal(measurement.Names, names) {
			csr = measurement.Csr
			publicKey = measurement.PublicKey
		}
		if !model.FlagNetworkExtenderMeasurementEverIssued(ctx, cert.ExtenderId) {
			continue
		}
		*orders += 1
		newCert, err := orderExtenderMeasurementCert(ctx, acmeClient, zone, cert.ExtenderId, names, csr, publicKey, false)
		if err != nil {
			glog.Errorf("[extendermeasurement]%s not renewed: %s\n", cert.CertId, err)
			// a refusal under a rate limit waits out the CA's Retry-After, and
			// any other failure backs off (C6, amendment 24)
			model.DeferNetworkExtenderMeasurementCertRenewal(
				ctx,
				cert.CertId,
				extenderMeasurementRetryTime(err, cert.RenewFailureCount, server.NowUtc()),
			)
			result.Failed += 1
			continue
		}
		stored(newCert)
		result.Renewed += 1
	}
}

// The hold on new certificates (C5, C7, amendment 24). An order the weekly cap
// counts is for a name with no unexpired certificate, so when the CA refuses
// one under a rate limit, the limit is the registered domain's -- 50 new
// certificates a week for bringyour.com, which its other names draw on too --
// or the account's on new orders, not the name's
// (lag.dev/ops/letsencrypt.md §5.1), and the CA would refuse every other new
// name the same way. A new name's own limit, five failed validations an hour,
// is beyond the backoff, which tries a failing name four times in its first
// hour. Deferred one row at a time, as the refusal of one name's order is,
// the refusals would come at once, up to ExtenderMeasurementMaxOrdersPerPass
// a pass, until the whole capped queue waited out one Retry-After, and then
// all of it would come due within the same minutes and be refused again. So
// the refusal also holds every order the cap counts until its own next
// attempt (extenderMeasurementRetryTime): the pass places no more of them, and
// while the hold lasts each capped request is noted waiting with the CA's
// limit and the time, neither deferred nor counted a failure. Renewals, orders
// for a name with an unexpired certificate and revocations go on: the CA
// renews outside those limits, and a limit of one name's -- five certificates
// a week for it, its failed validations -- defers only that name's row.
//
// The hold is kept in the process, like the ACME clients, so a restart
// forgets it, which costs at most one more refusal.
type extenderMeasurementCapHold struct {
	until  time.Time
	reason string
}

var extenderMeasurementCapHoldStateLock sync.Mutex
var extenderMeasurementCapHoldState extenderMeasurementCapHold

// Testing_ResetExtenderMeasurementCapHold lifts the hold on new certificates,
// so a test starts with none.
func Testing_ResetExtenderMeasurementCapHold() {
	extenderMeasurementCapHoldStateLock.Lock()
	defer extenderMeasurementCapHoldStateLock.Unlock()
	extenderMeasurementCapHoldState = extenderMeasurementCapHold{}
}

// When `err` is the CA's refusal under a rate limit of an order the cap
// counts, holds new certificates until `retryTime` and returns true. A hold
// that already reaches later is kept.
func holdExtenderMeasurementCapCounted(err error, retryTime time.Time) bool {
	var acmeErr *acme.Error
	if !errors.As(err, &acmeErr) {
		return false
	}
	if _, ok := acme.RateLimit(acmeErr); !ok {
		return false
	}
	limit := acmeErr.Detail
	if limit == "" {
		limit = acmeErr.Error()
	}
	extenderMeasurementCapHoldStateLock.Lock()
	defer extenderMeasurementCapHoldStateLock.Unlock()
	if extenderMeasurementCapHoldState.until.Before(retryTime) {
		extenderMeasurementCapHoldState = extenderMeasurementCapHold{
			until: retryTime,
			reason: fmt.Sprintf(
				"waiting until %s for the CA's limit on new certificates: %s",
				retryTime.UTC().Format(time.RFC3339),
				limit,
			),
		}
	}
	return true
}

// Whether new certificates are held at `now`, and the reason a capped request
// waits while they are.
func extenderMeasurementCapHeld(now time.Time) (reason string, held bool) {
	extenderMeasurementCapHoldStateLock.Lock()
	defer extenderMeasurementCapHoldStateLock.Unlock()
	if now.Before(extenderMeasurementCapHoldState.until) {
		return extenderMeasurementCapHoldState.reason, true
	}
	return "", false
}

// Issues for pending requests (C5, C7): first those the weekly cap does not
// hold, longest waiting first, then the rest, each new certificate where it
// least crowds the more crowded of its country and its network (amendment 24;
// model.GetPendingNetworkExtenderMeasurements). A request is refused when it
// is not a valid request for its extender's measurement name, or when its
// extender may not be measured. A request of an extender that is not active is
// not read: deactivated, it keeps its place in the queue until it activates
// again (amendment 23). A request for a name that still has an
// unexpired certificate is a renewal to the CA and goes past the weekly cap,
// which is how a rotated key and an extender that came back are served; a
// new extender's waits while the cap is spent, and is read again each pass,
// behind the requests the cap does not hold however many wait. It waits too
// while the CA's refusal of a new certificate under a rate limit holds them
// all (extenderMeasurementCapHold).
//
// A request whose certificate ran out un-renewed is put back in the queue
// first, so nothing stays issued with no certificate behind it. So is the
// request of an extender that is active again after its request was withdrawn
// or refused because it was not active (C9), if it may be measured now: the
// stored request is asked again, as if the extender had carried it again (C4,
// amendment 24). Since connect 567d4833 an extender that reads withdrawn asks
// for an activation pass at once, which carries its request; this serves the
// extenders from before, which carry it only with their next activation, up
// to a day after they came back. One that may not be measured now is left
// withdrawn, so it reads withdrawn and drops its revoked certificate: asked
// again, it would be refused in this same pass and read only pending and
// refused, through which an extender keeps an installed certificate. A
// request an owner's opt-out dropped, and one withdrawn for the gated tier or
// a canary, are not asked again.
func issuePendingExtenderMeasurementCerts(
	ctx context.Context,
	config *controller.ExtenderConfig,
	acmeClient *extenderMeasurementAcme,
	zone *extenderMeasurementZone,
	now time.Time,
	mayOrder func() bool,
	orders *int,
	stored func(*model.NetworkExtenderMeasurementCert),
	result *ExtenderMeasurementIssueResult,
) {
	measurementZone := config.Measurement.MeasurementZone()
	issueCap := config.Measurement.WeeklyIssueCap()
	model.ReopenExpiredNetworkExtenderMeasurements(ctx, now)
	model.ReopenReturnedNetworkExtenderMeasurements(
		ctx,
		controller.ExtenderMeasurementInactiveReason,
		now,
		func(extender *model.NetworkExtender, addresses []*model.NetworkExtenderAddress) bool {
			_, reason := controller.ExtenderMeasurementEligibleAddresses(extender, addresses)
			return reason == ""
		},
	)
	issuedThisWeek := model.CountNetworkExtenderMeasurementCertsCountedSince(ctx, now.Add(-ExtenderMeasurementCapWindow))

	for _, target := range model.GetPendingNetworkExtenderMeasurements(ctx, now, ExtenderMeasurementPendingLimit) {
		if ctx.Err() != nil {
			return
		}
		measurement := target.Measurement
		refuse := func(reason string) {
			if model.RefusePendingNetworkExtenderMeasurement(ctx, measurement.ExtenderId, measurement.Csr, reason, server.NowUtc()) {
				result.Refused += 1
			}
		}
		wait := func(reason string) {
			model.NoteNetworkExtenderMeasurementWait(ctx, measurement.ExtenderId, measurement.Csr, reason, server.NowUtc())
			result.Waiting += 1
		}

		if measurement.Csr == nil {
			refuse("the request has no certificate request")
			continue
		}
		if target.Extender != nil && !target.Extender.Active {
			// deactivated since the queue was read, which leaves out the
			// requests of extenders that are not active; it keeps its place
			// for when it activates again (amendment 23)
			continue
		}
		// the request was read at activation; it is read again against the
		// zone as configured now
		request, err := controller.ParseExtenderMeasurementCsrDer(measurement.Csr, measurement.ExtenderId, measurementZone)
		if err != nil {
			refuse(err.Error())
			continue
		}
		if _, reason := controller.ExtenderMeasurementEligibleAddresses(target.Extender, target.Addresses); reason != "" {
			refuse(reason)
			continue
		}
		names := []string{request.Name}

		capCounted := !model.HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx, names, now)
		if capCounted && issueCap <= issuedThisWeek {
			wait(fmt.Sprintf("waiting for the weekly cap of %d new certificates", issueCap))
			continue
		}
		if capCounted {
			// the CA refused a new certificate under the domain's or the
			// account's limit, which holds the rest (C7, amendment 24)
			if reason, held := extenderMeasurementCapHeld(now); held {
				wait(reason)
				continue
			}
		}
		if !mayOrder() {
			return
		}
		// flag first: the CA logs the certificate to certificate transparency
		// during issuance, so the flag must be set before the order (C5, A9)
		if !model.FlagNetworkExtenderMeasurementEverIssued(ctx, measurement.ExtenderId) {
			// moved out of the open tier since it was read; the next pass
			// refuses it
			continue
		}
		*orders += 1
		cert, err := orderExtenderMeasurementCert(ctx, acmeClient, zone, measurement.ExtenderId, names, request.Csr, request.PublicKey, capCounted)
		if err != nil {
			glog.Errorf("[extendermeasurement]%s not issued: %s\n", measurement.ExtenderId, err)
			// a refusal under a rate limit waits out the CA's Retry-After, and
			// any other failure backs off (C5, amendment 24)
			retryTime := extenderMeasurementRetryTime(err, measurement.FailureCount, server.NowUtc())
			model.DeferPendingNetworkExtenderMeasurement(
				ctx,
				measurement.ExtenderId,
				measurement.Csr,
				err.Error(),
				retryTime,
				server.NowUtc(),
			)
			// and a refusal of a new certificate holds every other one until
			// the same time (C7, amendment 24)
			if capCounted && holdExtenderMeasurementCapCounted(err, retryTime) {
				glog.Infof("[extendermeasurement]new certificates are held until %s\n", retryTime.UTC().Format(time.RFC3339))
			}
			result.Failed += 1
			continue
		}
		stored(cert)
		if capCounted {
			issuedThisWeek += 1
		}
		result.Issued += 1
	}
}

// One order, bounded by ExtenderMeasurementOrderTimeout, and the row of the
// certificate it returns.
func orderExtenderMeasurementCert(
	ctx context.Context,
	acmeClient *extenderMeasurementAcme,
	zone *extenderMeasurementZone,
	extenderId server.Id,
	names []string,
	csr []byte,
	publicKey []byte,
	capCounted bool,
) (*model.NetworkExtenderMeasurementCert, error) {
	orderCtx, orderCancel := context.WithTimeout(ctx, ExtenderMeasurementOrderTimeout)
	defer orderCancel()
	chain, err := acmeClient.issue(orderCtx, names, csr, zone)
	if err != nil {
		return nil, err
	}
	return newExtenderMeasurementCert(extenderId, names, csr, publicKey, chain, capCounted, server.NowUtc())
}

type ExtenderMeasurementDnsArgs struct {
}

type ExtenderMeasurementDnsResult struct {
	Withdrawn int `json:"withdrawn"`
	// extenders that serve, each one name
	Names    int `json:"names"`
	Upserted int `json:"upserted"`
	Deleted  int `json:"deleted"`
	// names this pass published
	Published int `json:"published"`
}

func ScheduleExtenderMeasurementDns(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleExtenderMeasurementDnsAt(clientSession, tx, server.NowUtc().Add(ExtenderMeasurementDnsTimeout))
}

func scheduleExtenderMeasurementDnsAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderMeasurementDns,
		&ExtenderMeasurementDnsArgs{},
		clientSession,
		task.RunOnce("extender_measurement_dns"),
		task.RunAt(runAt),
		task.MaxTime(ExtenderMeasurementDnsMaxTime),
	)
}

// Wakes the dns chain to run now rather than at its next minute (C8). The
// pending pass moves to now, or, when a pass is running, another follows it at
// once; either way one pass runs at a time.
func wakeExtenderMeasurementDns(clientSession *session.ClientSession) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		scheduleExtenderMeasurementDnsAt(clientSession, tx, server.NowUtc())
	})
}

// ExtenderMeasurementDns is one pass of the dns chain (C9, C8).
func ExtenderMeasurementDns(
	_ *ExtenderMeasurementDnsArgs,
	clientSession *session.ClientSession,
) (*ExtenderMeasurementDnsResult, error) {
	result := &ExtenderMeasurementDnsResult{}
	config, err := controller.EnvExtenderConfig()
	if err != nil || !config.Measurement.Enabled {
		return result, nil
	}
	now := server.NowUtc()
	result.Withdrawn = withdrawExtenderMeasurementCerts(clientSession.Ctx, now)
	if err := reconcileExtenderMeasurementDns(clientSession.Ctx, config, now, result); err != nil {
		// the next pass tries again; a withdrawal is already stored and
		// revoked whatever dns does
		glog.Errorf("[extendermeasurement]the measurement zone was not reconciled: %s\n", err)
	}
	if 0 < result.Withdrawn+result.Upserted+result.Deleted+result.Published {
		glog.Infof(
			"[extendermeasurement]withdrew %d; %d names, upserted %d, deleted %d, published %d\n",
			result.Withdrawn,
			result.Names,
			result.Upserted,
			result.Deleted,
			result.Published,
		)
	}
	return result, nil
}

func ExtenderMeasurementDnsPost(
	_ *ExtenderMeasurementDnsArgs,
	_ *ExtenderMeasurementDnsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExtenderMeasurementDns(clientSession, tx)
	return nil
}

// Withdraws the live certificates of every extender that is in the gated tier
// or a canary, or that was deactivated and is no longer away (C9, amendment
// 23), and returns how many. An operator's override withdraws at once, in its
// own transaction (A9); this is what finds a deactivation, and anything that
// reached those tiers another way. An away extender is out of the zone
// already, since it may not be measured while it is not active (C8).
//
// The read only picks what to judge. Each certificate is withdrawn only if its
// extender, locked and read again in the withdrawing transaction, must still
// be, and with the reason it has then
// (model.WithdrawNetworkExtenderMeasurementCertIf): an extender that activated
// since the read, as one does that comes back right at the end of its away
// grace, keeps its certificate (amendment 24).
func withdrawExtenderMeasurementCerts(ctx context.Context, now time.Time) int {
	reasonFor := func(extender *model.NetworkExtender) string {
		return controller.ExtenderMeasurementWithdrawReason(extender, now)
	}
	withdrawn := 0
	for _, target := range model.GetLiveNetworkExtenderMeasurementCerts(ctx, now) {
		if reasonFor(target.Extender) == "" {
			continue
		}
		// the extender's other live certificate, a renewal's overlap, goes
		// with this one, and is already withdrawn when the read comes to it
		withdrawn += model.WithdrawNetworkExtenderMeasurementCertIf(ctx, target.Cert.CertId, now, reasonFor)
	}
	return withdrawn
}

// Makes the zone's address records the extenders that serve, and records
// which names are published (C8). A name stops being published before its
// records are deleted, and is published only once the change that wrote them
// is in sync, so the lag.dev sample (A4) never offers a name that dns does not
// answer for everywhere.
func reconcileExtenderMeasurementDns(
	ctx context.Context,
	config *controller.ExtenderConfig,
	now time.Time,
	result *ExtenderMeasurementDnsResult,
) error {
	hosts := controller.GetExtenderMeasurementHostsToPublish(ctx, config, now)
	result.Names = len(hosts)
	names := []string{}
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	// every other name is going, or gone
	model.UnpublishNetworkExtenderMeasurementNamesExcept(ctx, names)

	zone, err := openExtenderMeasurementZone(ctx, config)
	if err != nil {
		return err
	}
	plan, err := zone.plan(ctx, hosts)
	if err != nil {
		return err
	}
	// a published name whose records went another way is written again,
	// and is not offered until that is in sync
	model.UnpublishNetworkExtenderMeasurementNames(ctx, plan.absent)

	changes, err := zone.apply(ctx, plan)
	result.Upserted = changes.upserted
	result.Deleted = changes.deleted
	result.Published = changes.newlyPublished
	// also marks a renewal of a name that stayed published
	model.PublishNetworkExtenderMeasurementNames(ctx, changes.published, server.NowUtc())
	return err
}
