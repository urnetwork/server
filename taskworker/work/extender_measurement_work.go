package work

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The lag.dev measurement certificate tasks (lag.dev/PLAN.md C5-C9). Two
// chains, each one task a minute:
//
//   - ExtenderMeasurementDns withdraws the live certificates of extenders that
//     were deactivated or moved to the gated tier or a canary channel (C9),
//     then makes the zone's address records exactly the extenders that serve,
//     each name at its extender's active addresses, and records which names
//     are published (C8). It is quick -- reads and one Route 53 listing -- so
//     a deactivation leaves dns, and an address change reaches it, within
//     about a minute. Only a pass that publishes a name waits, until Route 53
//     reports the name's change in sync, since the lag.dev sample offers a
//     name only once it is published (A4).
//   - ExtenderMeasurementIssue revokes what was withdrawn (C9), renews what is
//     due (C6) and issues for pending requests under the weekly cap (C5,
//     C7). An order waits on Route 53 and the CA for a minute or more, so this
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
// backs off on its own row, and the chain re-arms either way.

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
	// (lag.dev/ops/letsencrypt.md).
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
			model.DeferNetworkExtenderMeasurementCertRevocation(
				ctx,
				cert.CertId,
				server.NowUtc().Add(extenderMeasurementRetryTimeout(cert.RevokeFailureCount)),
			)
			result.Failed += 1
			continue
		}
		model.MarkNetworkExtenderMeasurementCertRevoked(ctx, cert.CertId, server.NowUtc())
		result.Revoked += 1
	}
}

// Renews the live certificates that are due (C6): two thirds of the way
// through their lifetime, since the CA's renewal information is not read. A
// renewal reuses the stored request -- the extender's latest when that is for
// the same name, so a key the extender rotated is followed, else the
// certificate's own -- and is a renewal to the CA too, outside the weekly cap
// (C7). A certificate of an extender that may not be measured now, or that is
// for another zone's name, is not renewed; a deactivated extender's is
// withdrawn by the dns chain.
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
		if _, reason := controller.ExtenderMeasurementEligibleAddresses(target.Extender, target.Addresses); reason != "" {
			continue
		}
		names := []string{controller.ExtenderMeasurementName(cert.ExtenderId, measurementZone)}
		if !slices.Equal(cert.Names, names) {
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
			model.DeferNetworkExtenderMeasurementCertRenewal(
				ctx,
				cert.CertId,
				server.NowUtc().Add(extenderMeasurementRetryTimeout(cert.RenewFailureCount)),
			)
			result.Failed += 1
			continue
		}
		stored(newCert)
		result.Renewed += 1
	}
}

// Issues for pending requests (C5, C7): first those the weekly cap does not
// hold, then the rest, each longest waiting first
// (model.GetPendingNetworkExtenderMeasurements). A request is refused when it
// is not a valid request for its extender's measurement name, or when its
// extender may not be measured. A request for a name that still has an
// unexpired certificate is a renewal to the CA and goes past the weekly cap,
// which is how a rotated key and an extender that came back are served; a
// new extender's waits while the cap is spent, and is read again each pass,
// behind the requests the cap does not hold however many wait.
//
// A request whose certificate ran out un-renewed is put back in the queue
// first, so nothing stays issued with no certificate behind it.
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
			model.DeferPendingNetworkExtenderMeasurement(
				ctx,
				measurement.ExtenderId,
				measurement.Csr,
				err.Error(),
				server.NowUtc().Add(extenderMeasurementRetryTimeout(measurement.FailureCount)),
				server.NowUtc(),
			)
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

// Withdraws the live certificates of every extender that was deactivated, or
// that is in the gated tier or a canary (C9), and returns how many. An
// operator's override withdraws at once, in its own transaction (A9); this is
// what finds a deactivation, and anything that reached those tiers another
// way.
func withdrawExtenderMeasurementCerts(ctx context.Context, now time.Time) int {
	withdrawn := 0
	for _, target := range model.GetLiveNetworkExtenderMeasurementCerts(ctx, now) {
		reason := controller.ExtenderMeasurementWithdrawReason(target.Extender)
		if reason == "" {
			continue
		}
		if model.WithdrawNetworkExtenderMeasurementCert(ctx, target.Cert.CertId, reason, now) {
			withdrawn += 1
		}
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
