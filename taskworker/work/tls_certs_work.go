package work

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// certs' taskworker chains (TLS-M5-17; §13.8, §19.4), one run-once chain each,
// every run one bounded pass of the engine (certsservice's run.go) that the
// next run takes up from its cursor:
//
//   - orders: what an earlier pass left under way, then the queue under the
//     policy, each order to its CA;
//   - renewals: issued rows past their validity expired, due ones queued from
//     their stored CSR, a year-old account key rolled over;
//   - revocations: the sweep, which reads from Postgres which issued
//     certificates must go (tlsCertRevocationSource), with no call from any
//     other area, and takes every revoking row to its CA;
//   - purge: the rows of network_tls_cert that ended more than the row
//     retention ago, 30 days: an expired row by its validity's end, and a
//     revoked or failed one by its state's time (CertsStore.PurgeEnded;
//     QD-742). Before that it drops the chains of rows whose certificate
//     expired or was revoked more than the chain retention ago, 30 days
//     (CertsStore.PurgeChains; QD-741).
//
// What the first three run on is installed per process (SetTlsCertChains), as
// the routes' is (controller.SetTlsCerts): the engine over this process's
// store, Redis, Push, DNS client and ACME client. A process with nothing
// installed runs an empty pass and schedules the next, so nothing reaches a CA
// or a zone until its host has built the engine. A binary that does not link
// MG5 neither schedules a chain nor, should an older row run, touches
// anything.
//
// A run's task max time bounds its pass: the engine leaves an order its CA has
// not answered under way, and the next run orders it again.

// What the order, renewal and revocation chains run on.
type TlsCertChainDeps struct {
	// The engine, on this process's store and seams. Required.
	Service *certsservice.CertsService
	// What the sweep reads. nil takes the Postgres source.
	RevocationSource certsservice.CertsRevocationSource
}

var tlsCertChainDeps atomic.Pointer[TlsCertChainDeps]

// Installs what this process's certs chains run on; nil, or deps with no
// service, removes it, and each chain runs empty passes.
func SetTlsCertChains(deps *TlsCertChainDeps) {
	if deps == nil || deps.Service == nil {
		tlsCertChainDeps.Store(nil)
		return
	}
	installed := *deps
	if installed.RevocationSource == nil {
		installed.RevocationSource = NewTlsCertRevocationSource(nil)
	}
	tlsCertChainDeps.Store(&installed)
}

// Cadences of the order, renewal and revocation chains.
type TlsCertChainSettings struct {
	// The order chain runs again this long after a pass, MoreInterval after
	// one that left work waiting, and RetryInterval after one that failed.
	OrdersRunInterval time.Duration
	// A pass may wait a whole order out: past the engine's AcmeOrderTimeout.
	OrdersMaxTime time.Duration
	// A scan of the issued rows starts this long after the last ended.
	RenewalsRunInterval time.Duration
	RenewalsMaxTime     time.Duration
	// I-15: a removed device's certificate is marked within one interval and
	// one scan.
	RevocationsRunInterval time.Duration
	RevocationsMaxTime     time.Duration
	// The wait before the next batch of a scan, or the next pass of orders
	// when more wait.
	MoreInterval  time.Duration
	RetryInterval time.Duration
}

func DefaultTlsCertChainSettings() *TlsCertChainSettings {
	return &TlsCertChainSettings{
		OrdersRunInterval:      30 * time.Second,
		OrdersMaxTime:          certsservice.DefaultCertsSettings().AcmeOrderTimeout + 15*time.Minute,
		RenewalsRunInterval:    1 * time.Hour,
		RenewalsMaxTime:        30 * time.Minute,
		RevocationsRunInterval: 5 * time.Minute,
		RevocationsMaxTime:     30 * time.Minute,
		MoreInterval:           1 * time.Second,
		RetryInterval:          1 * time.Minute,
	}
}

// A cursor as a task's args carry it: an id's text, empty for the start.
func tlsCertCursorText(certId sidecar.Id) string {
	if certId == (sidecar.Id{}) {
		return ""
	}
	return certId.String()
}

// The cursor args carry. Text that is no id reads as the start, so a scan
// begins again rather than stops.
func tlsCertCursor(text string) sidecar.Id {
	if text == "" {
		return sidecar.Id{}
	}
	certId, err := sidecar.ParseId(text)
	if err != nil {
		return sidecar.Id{}
	}
	return certId
}

// When a chain's next run is: soon when the pass left more or a scan has
// batches left, after RetryInterval when it failed, else a whole interval on.
func tlsCertChainNextRun(settings *TlsCertChainSettings, runInterval time.Duration, more bool, failed bool) time.Time {
	now := server.NowUtc()
	switch {
	case failed:
		return now.Add(settings.RetryInterval)
	case more:
		return now.Add(settings.MoreInterval)
	}
	return now.Add(runInterval)
}

type TlsCertOrdersArgs struct {
	// Where the queue is read from: the last pass's NextCertId.
	AfterCertId string `json:"after_cert_id,omitempty"`
}

type TlsCertOrdersResult struct {
	// Nothing is installed, or the engine has no ACME client.
	Idle             bool   `json:"idle"`
	ResumedCount     int    `json:"resumed_count"`
	AdmittedCount    int    `json:"admitted_count"`
	WaitingCount     int    `json:"waiting_count"`
	IssuedCount      int    `json:"issued_count"`
	RequeuedCount    int    `json:"requeued_count"`
	FailedCount      int    `json:"failed_count"`
	InterruptedCount int    `json:"interrupted_count"`
	NextCertId       string `json:"next_cert_id,omitempty"`
	More             bool   `json:"more"`
	Failed           bool   `json:"failed"`
}

// Schedules the order chain's first run. A binary that does not link MG5
// schedules nothing.
func ScheduleTlsCertOrders(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsCertOrders(clientSession, tx, &TlsCertOrdersArgs{}, server.NowUtc().Add(DefaultTlsCertChainSettings().OrdersRunInterval))
}

func scheduleTlsCertOrders(clientSession *session.ClientSession, tx server.PgTx, args *TlsCertOrdersArgs, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsCertOrders,
		args,
		clientSession,
		task.RunOnce("tls_cert_orders"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsCertChainSettings().OrdersMaxTime),
	)
}

// One pass of the order chain.
func TlsCertOrders(
	args *TlsCertOrdersArgs,
	clientSession *session.ClientSession,
) (*TlsCertOrdersResult, error) {
	result := &TlsCertOrdersResult{Idle: true}
	deps := tlsCertChainDeps.Load()
	if !server.SidecarMg5Linked() || deps == nil {
		return result, nil
	}
	report, err := deps.Service.RunOrders(clientSession.Ctx, tlsCertCursor(args.AfterCertId))
	if err != nil {
		glog.Warningf("[tls]cert orders: %v\n", err)
		result.Failed = true
	}
	result.Idle = report.Unavailable
	result.ResumedCount = len(report.Resumed)
	result.AdmittedCount = len(report.Admitted)
	result.WaitingCount = len(report.Waiting)
	result.IssuedCount = len(report.Issued)
	result.RequeuedCount = len(report.Requeued)
	result.FailedCount = len(report.Failed)
	result.InterruptedCount = len(report.Interrupted)
	result.NextCertId = tlsCertCursorText(report.NextCertId)
	result.More = report.More
	if 0 < result.IssuedCount+result.RequeuedCount+result.FailedCount+result.InterruptedCount {
		glog.Infof(
			"[tls]cert orders: issued %d, queued again %d, failed %d, cut short %d, waiting %d\n",
			result.IssuedCount,
			result.RequeuedCount,
			result.FailedCount,
			result.InterruptedCount,
			result.WaitingCount,
		)
	}
	return result, nil
}

// Schedules the next pass from where this one stopped in the queue.
func TlsCertOrdersPost(
	_ *TlsCertOrdersArgs,
	result *TlsCertOrdersResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsCertChainSettings()
	scheduleTlsCertOrders(
		clientSession,
		tx,
		&TlsCertOrdersArgs{AfterCertId: result.NextCertId},
		tlsCertChainNextRun(settings, settings.OrdersRunInterval, result.More, result.Failed),
	)
	return nil
}

type TlsCertRenewalsArgs struct {
	// Where the issued rows are read from: the last pass's NextCertId.
	AfterCertId string `json:"after_cert_id,omitempty"`
}

type TlsCertRenewalsResult struct {
	// Nothing is installed.
	Idle          bool     `json:"idle"`
	ExpiredCount  int      `json:"expired_count"`
	QueuedCount   int      `json:"queued_count"`
	WindowsRead   int      `json:"windows_read"`
	FailedCount   int      `json:"failed_count"`
	RolledOverCas []string `json:"rolled_over_cas,omitempty"`
	// Accounts whose key is due and whose file holds no next key, or whose
	// rollover failed: an operator's to act on.
	RolloverDueCas []string `json:"rollover_due_cas,omitempty"`
	NextCertId     string   `json:"next_cert_id,omitempty"`
	Failed         bool     `json:"failed"`
}

// Schedules the renewal chain's first run. A binary that does not link MG5
// schedules nothing.
func ScheduleTlsCertRenewals(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsCertRenewals(clientSession, tx, &TlsCertRenewalsArgs{}, server.NowUtc().Add(DefaultTlsCertChainSettings().RenewalsRunInterval))
}

func scheduleTlsCertRenewals(clientSession *session.ClientSession, tx server.PgTx, args *TlsCertRenewalsArgs, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsCertRenewals,
		args,
		clientSession,
		task.RunOnce("tls_cert_renewals"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsCertChainSettings().RenewalsMaxTime),
	)
}

// One pass of the renewal chain.
func TlsCertRenewals(
	args *TlsCertRenewalsArgs,
	clientSession *session.ClientSession,
) (*TlsCertRenewalsResult, error) {
	result := &TlsCertRenewalsResult{Idle: true}
	deps := tlsCertChainDeps.Load()
	if !server.SidecarMg5Linked() || deps == nil {
		return result, nil
	}
	result.Idle = false
	report, err := deps.Service.RunRenewals(clientSession.Ctx, tlsCertCursor(args.AfterCertId))
	if err != nil {
		glog.Warningf("[tls]cert renewals: %v\n", err)
		result.Failed = true
	}
	result.ExpiredCount = len(report.Expired)
	result.QueuedCount = len(report.Queued)
	result.WindowsRead = report.WindowsRead
	result.FailedCount = report.FailedCount
	result.NextCertId = tlsCertCursorText(report.NextCertId)
	for _, rollover := range report.Rollovers {
		if rollover.RolledOver {
			result.RolledOverCas = append(result.RolledOverCas, string(rollover.Ca))
			glog.Infof("[tls]cert renewals: CA %s's account key rolled over\n", rollover.Ca)
			continue
		}
		result.RolloverDueCas = append(result.RolloverDueCas, string(rollover.Ca))
		if rollover.Err != nil {
			glog.Warningf("[tls]cert renewals: CA %s's account key did not roll over: %v\n", rollover.Ca, rollover.Err)
		} else {
			glog.Warningf("[tls]cert renewals: CA %s's account key is due to roll over and its file holds no next key\n", rollover.Ca)
		}
	}
	if 0 < result.ExpiredCount+result.QueuedCount+result.FailedCount {
		glog.Infof("[tls]cert renewals: expired %d, queued %d, failed %d\n", result.ExpiredCount, result.QueuedCount, result.FailedCount)
	}
	return result, nil
}

// Schedules the next batch of the scan, or the next scan.
func TlsCertRenewalsPost(
	_ *TlsCertRenewalsArgs,
	result *TlsCertRenewalsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsCertChainSettings()
	scheduleTlsCertRenewals(
		clientSession,
		tx,
		&TlsCertRenewalsArgs{AfterCertId: result.NextCertId},
		tlsCertChainNextRun(settings, settings.RenewalsRunInterval, result.NextCertId != "", result.Failed),
	)
	return nil
}

type TlsCertRevocationsArgs struct {
	// Where the sweep and the revoking rows are read from: the last pass's.
	AfterIssuedCertId   string `json:"after_issued_cert_id,omitempty"`
	AfterRevokingCertId string `json:"after_revoking_cert_id,omitempty"`
}

type TlsCertRevocationsResult struct {
	// Nothing is installed.
	Idle                bool   `json:"idle"`
	MarkedCount         int    `json:"marked_count"`
	RevokedCount        int    `json:"revoked_count"`
	ExpiredCount        int    `json:"expired_count"`
	WaitingCount        int    `json:"waiting_count"`
	FailedCount         int    `json:"failed_count"`
	AfterIssuedCertId   string `json:"after_issued_cert_id,omitempty"`
	AfterRevokingCertId string `json:"after_revoking_cert_id,omitempty"`
	Failed              bool   `json:"failed"`
}

// Schedules the revocation chain's first run. A binary that does not link MG5
// schedules nothing.
func ScheduleTlsCertRevocations(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsCertRevocations(clientSession, tx, &TlsCertRevocationsArgs{}, server.NowUtc().Add(DefaultTlsCertChainSettings().RevocationsRunInterval))
}

func scheduleTlsCertRevocations(clientSession *session.ClientSession, tx server.PgTx, args *TlsCertRevocationsArgs, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsCertRevocations,
		args,
		clientSession,
		task.RunOnce("tls_cert_revocations"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsCertChainSettings().RevocationsMaxTime),
	)
}

// One pass of the revocation chain.
func TlsCertRevocations(
	args *TlsCertRevocationsArgs,
	clientSession *session.ClientSession,
) (*TlsCertRevocationsResult, error) {
	result := &TlsCertRevocationsResult{Idle: true}
	deps := tlsCertChainDeps.Load()
	if !server.SidecarMg5Linked() || deps == nil {
		return result, nil
	}
	result.Idle = false
	report, err := deps.Service.RunRevocations(clientSession.Ctx, deps.RevocationSource, &certsservice.CertsRevocationCursor{
		AfterIssuedCertId:   tlsCertCursor(args.AfterIssuedCertId),
		AfterRevokingCertId: tlsCertCursor(args.AfterRevokingCertId),
	})
	if err != nil {
		glog.Warningf("[tls]cert revocations: %v\n", err)
		result.Failed = true
	}
	result.MarkedCount = len(report.Marked)
	result.RevokedCount = len(report.Revoked)
	result.ExpiredCount = len(report.Expired)
	result.WaitingCount = report.WaitingCount
	result.FailedCount = report.FailedCount
	result.AfterIssuedCertId = tlsCertCursorText(report.Next.AfterIssuedCertId)
	result.AfterRevokingCertId = tlsCertCursorText(report.Next.AfterRevokingCertId)
	if 0 < result.MarkedCount+result.RevokedCount+result.ExpiredCount+result.FailedCount {
		glog.Infof(
			"[tls]cert revocations: marked %d, revoked %d, expired %d, failed %d, waiting %d\n",
			result.MarkedCount,
			result.RevokedCount,
			result.ExpiredCount,
			result.FailedCount,
			result.WaitingCount,
		)
	}
	return result, nil
}

// Schedules the next batch of the sweep, or the next sweep.
func TlsCertRevocationsPost(
	_ *TlsCertRevocationsArgs,
	result *TlsCertRevocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsCertChainSettings()
	more := result.AfterIssuedCertId != "" || result.AfterRevokingCertId != ""
	scheduleTlsCertRevocations(
		clientSession,
		tx,
		&TlsCertRevocationsArgs{AfterIssuedCertId: result.AfterIssuedCertId, AfterRevokingCertId: result.AfterRevokingCertId},
		tlsCertChainNextRun(settings, settings.RevocationsRunInterval, more, result.Failed),
	)
	return nil
}

// Tunables of the sweep's reads.
type TlsCertRevocationSourceSettings struct {
	// The longest one read waits on Postgres, under the caller's deadline.
	CallTimeout time.Duration
}

func DefaultTlsCertRevocationSourceSettings() *TlsCertRevocationSourceSettings {
	return &TlsCertRevocationSourceSettings{
		CallTimeout: 10 * time.Second,
	}
}

// The sweep's source needs MG1's identities and MG5's labels beside the base
// tables, and this binary links one of them not.
var ErrTlsCertRevocationSourceUnavailable = errors.New("the revocation sweep's tables are not linked in this binary")

// What the revocation sweep reads of other areas' rows (§13.8's Revocation;
// I-14, I-15), so none of them calls certs:
//
//   - a network that is gone: no network row (CertsRevocationCauseDeletion);
//   - a device that is gone: no active network_client among the row's client
//     and every client its sidecar has bound, so a new client generation keeps
//     its certificate while a removed or reaped device, ephemeral ones
//     included, loses it; or a sidecar_identity that is revoked
//     (CertsRevocationCauseRemoval);
//   - a retired alias: a name of the certificate at or under a dns_label alias
//     that is no longer live (CertsRevocationCauseAliasRetired).
//
// A takedown's certificates come marked (model.RequestTlsCertRevocation), not
// through here, with their reason in the row, as a device's or an admin's
// POST /tls/cert-revoke leaves its own; the engine stores the reason of each
// cause found here the same way. A revoked sidecar reads as a removal:
// sidecar_identity holds no word of why, so its CA is told
// cessationOfOperation unless the row was marked keyCompromise first.
type TlsCertRevocationSource struct {
	settings *TlsCertRevocationSourceSettings
}

var _ certsservice.CertsRevocationSource = (*TlsCertRevocationSource)(nil)

// nil settings take the defaults.
func NewTlsCertRevocationSource(settings *TlsCertRevocationSourceSettings) *TlsCertRevocationSource {
	if settings == nil {
		settings = DefaultTlsCertRevocationSourceSettings()
	}
	return &TlsCertRevocationSource{settings: settings}
}

// Each (label, root) a name sits at or under: every split of the name, less a
// leading wildcard, at one of its dots.
func tlsCertNameLabels(name string) (labels []string, roots []string) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(name, ".")), "*.")
	for {
		label, root, found := strings.Cut(name, ".")
		if !found || label == "" || root == "" {
			return
		}
		labels = append(labels, label)
		roots = append(roots, root)
		name = root
	}
}

func (self *TlsCertRevocationSource) RevocationCauses(ctx context.Context, candidates []*certsservice.CertsRevocationCandidate) (causes map[sidecar.Id]certsservice.CertsRevocationCause, returnErr error) {
	if !server.SidecarMg1Linked() || !server.SidecarMg5Linked() {
		return nil, ErrTlsCertRevocationSourceUnavailable
	}
	causes = map[sidecar.Id]certsservice.CertsRevocationCause{}
	if len(candidates) == 0 {
		return causes, nil
	}
	certIds := make([]server.Id, 0, len(candidates))
	networkIds := make([]server.Id, 0, len(candidates))
	clientIds := make([]server.Id, 0, len(candidates))
	sidecarIds := make([]server.Id, 0, len(candidates))
	nameCertIds := []server.Id{}
	nameLabels := []string{}
	nameRoots := []string{}
	for _, candidate := range candidates {
		row := candidate.Cert
		certIds = append(certIds, server.Id(row.CertId))
		networkIds = append(networkIds, server.Id(row.NetworkId))
		clientIds = append(clientIds, server.Id(row.ClientId))
		sidecarIds = append(sidecarIds, server.Id(row.SidecarId))
		for _, name := range candidate.Names {
			labels, roots := tlsCertNameLabels(name)
			for index := range labels {
				nameCertIds = append(nameCertIds, server.Id(row.CertId))
				nameLabels = append(nameLabels, labels[index])
				nameRoots = append(nameRoots, roots[index])
			}
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, self.settings.CallTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			causes = nil
			if err := callCtx.Err(); err != nil {
				returnErr = err
			} else if recoveredErr, ok := r.(error); ok {
				returnErr = recoveredErr
			} else {
				returnErr = errors.New("the revocation sweep's read failed")
			}
		}
	}()
	server.Db(callCtx, func(conn server.PgConn) {
		result, err := conn.Query(
			callCtx,
			`
				SELECT
					candidate.cert_id,
					NOT EXISTS (
						SELECT 1 FROM network WHERE network.network_id = candidate.network_id
					) AS network_gone,
					NOT EXISTS (
						SELECT 1
						FROM network_client
						WHERE
							network_client.active = true AND
							(
								network_client.client_id = candidate.client_id OR
								network_client.client_id IN (
									SELECT sidecar_identity_binding.client_id
									FROM sidecar_identity_binding
									WHERE sidecar_identity_binding.sidecar_id = candidate.sidecar_id
								)
							)
					) AS client_gone,
					EXISTS (
						SELECT 1
						FROM sidecar_identity
						WHERE
							sidecar_identity.sidecar_id = candidate.sidecar_id AND
							sidecar_identity.status = $5
					) AS sidecar_revoked
				FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::uuid[]) AS candidate(cert_id, network_id, client_id, sidecar_id)
			`,
			certIds,
			networkIds,
			clientIds,
			sidecarIds,
			model.SidecarIdentityStatusRevoked,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var certId server.Id
				var networkGone, clientGone, sidecarRevoked bool
				server.Raise(result.Scan(&certId, &networkGone, &clientGone, &sidecarRevoked))
				switch {
				case networkGone:
					causes[sidecar.Id(certId)] = certsservice.CertsRevocationCauseDeletion
				case clientGone, sidecarRevoked:
					causes[sidecar.Id(certId)] = certsservice.CertsRevocationCauseRemoval
				}
			}
		})
		if len(nameCertIds) == 0 {
			return
		}
		result, err = conn.Query(
			callCtx,
			`
				SELECT DISTINCT name.cert_id
				FROM unnest($1::uuid[], $2::text[], $3::text[]) AS name(cert_id, label, root)
				INNER JOIN dns_label ON
					dns_label.root = name.root AND
					dns_label.label = name.label
				WHERE
					dns_label.kind = $4 AND
					dns_label.state <> $5
			`,
			nameCertIds,
			nameLabels,
			nameRoots,
			model.DnsLabelKindAlias,
			model.DnsLabelStateLive,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var certId server.Id
				server.Raise(result.Scan(&certId))
				if _, found := causes[sidecar.Id(certId)]; !found {
					causes[sidecar.Id(certId)] = certsservice.CertsRevocationCauseAliasRetired
				}
			}
		})
	})
	return causes, nil
}

// Cadences of the purge.
type TlsCertPurgeSettings struct {
	// The purge runs again this long after a run, and RetryInterval after one
	// that failed. The retention is in days, so an hour is exact enough.
	RunInterval   time.Duration
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
	// A row stays this long past its expiry, revocation or failure: the
	// engine's RowRetention, cert-1's 30 d.
	RowRetention time.Duration
	// A row keeps its chain this long past its certificate's expiry or
	// revocation: the engine's ChainRetention, cert-1's 30 d.
	ChainRetention time.Duration
}

func DefaultTlsCertPurgeSettings() *TlsCertPurgeSettings {
	return &TlsCertPurgeSettings{
		RunInterval:    1 * time.Hour,
		RetryInterval:  5 * time.Minute,
		MaxTime:        10 * time.Minute,
		RowRetention:   certsservice.DefaultCertsSettings().RowRetention,
		ChainRetention: certsservice.DefaultCertsSettings().ChainRetention,
	}
}

type TlsCertPurgeArgs struct {
}

type TlsCertPurgeResult struct {
	RemovedCount int `json:"removed_count"`
	// Rows that lost their chain and stay.
	ChainCount int  `json:"chain_count"`
	More       bool `json:"more"`
}

// Schedules the purge's first run. A binary that does not link MG5 schedules
// nothing.
func ScheduleTlsCertPurge(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleTlsCertPurge(clientSession, tx, server.NowUtc().Add(DefaultTlsCertPurgeSettings().RunInterval))
}

func scheduleTlsCertPurge(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		TlsCertPurge,
		&TlsCertPurgeArgs{},
		clientSession,
		task.RunOnce("tls_cert_purge"),
		task.RunAt(runAt),
		task.MaxTime(DefaultTlsCertPurgeSettings().MaxTime),
	)
}

// One run of the purge.
func TlsCertPurge(
	_ *TlsCertPurgeArgs,
	clientSession *session.ClientSession,
) (*TlsCertPurgeResult, error) {
	return tlsCertPurgeAt(clientSession, server.NowUtc())
}

func tlsCertPurgeAt(clientSession *session.ClientSession, now time.Time) (*TlsCertPurgeResult, error) {
	if !server.SidecarMg5Linked() {
		return &TlsCertPurgeResult{}, nil
	}
	settings := DefaultTlsCertPurgeSettings()
	store := model.NewCertsStore(nil)
	result := &TlsCertPurgeResult{}
	chainCount, err := store.PurgeChains(clientSession.Ctx, now.Add(-settings.ChainRetention))
	if err != nil {
		glog.Warningf("[tls]cert purge: chains: %v\n", err)
		result.More = true
	}
	result.ChainCount = chainCount
	// a failure above does not keep the rows past their ttl
	removedCount, err := store.PurgeEnded(clientSession.Ctx, now.Add(-settings.RowRetention))
	if err != nil {
		glog.Warningf("[tls]cert purge: %v\n", err)
		result.More = true
	}
	result.RemovedCount = removedCount
	return result, nil
}

// Schedules the next run, sooner when this one failed.
func TlsCertPurgePost(
	_ *TlsCertPurgeArgs,
	result *TlsCertPurgeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultTlsCertPurgeSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if result.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	scheduleTlsCertPurge(clientSession, tx, runAt)
	return nil
}
