package model

// A delegated DNS-01 challenge's handoff from api to the certs worker (QD-1165; QD-175, QD-027; §12.8, §13.8, §13.15;
// DESIGN-DETAILS 17.2.6, 17.2.9). POST /tls/custom-challenge runs in api, which holds no DNS credential: the certs
// bearer is taskworker's alone. So api captures the write as a receipt, one row of network_tls_custom_challenge per
// domain and claim, and the worker's command (controller.TlsCustomChallengeWork, on taskworker's queue) makes it
// through certs' own DNS client and records the serial the write took:
//
//	queued      a write is owed: a new receipt, another digest under the claim, or a confirmed stale verdict
//	dispatched  the worker sent the write and has not heard; when, and the root's serial then, are kept
//	written     serial is the write's: what POST answers and what GET asks /dns/ready about
//	refused     no write is owed: the claim moved on, the domain lost its acme name, the name's hourly cap was full,
//	            the DNS path refused the write, or DispatchMaxCount dispatches were never made
//
// The capture, one transaction in api: the caller is an orderer of the domain (17.2.6) and holds the account's lock for
// an order, its row custom_ordering under the claim, and the domain is verified with its acme_token. The receipt holds
// the certificate row, the claim sequence, the digest and an expiry ReceiptLifetime on, the challenge's own hour, so
// no receipt outlives the value it records; the worker's command is queued in the same transaction, so no receipt
// waits on a command never queued. A retry under the claim with the same digest recovers the receipt, its serial once
// written and otherwise the write already owed, never a second one. A confirmed stale verdict, which only GET's
// readiness read records, lets the next capture of the same digest owe a new write.
//
// The worker, one step a transaction and its DNS calls outside any (ChallengeWorkStep): it rechecks ownership and
// authority after the account's lock, as a claim reads it: the lock is still the receipt's claim, sidecar and row, the
// row is custom_ordering under the claim, and the domain still holds the receipt's acme name; else the receipt is
// refused, claim or no_account, with nothing written. It reads the name's writes of the cap's window from the
// journal, as the DNS path counts them, and at dns_challenges_per_name_hour refuses budget with the time the window
// frees, charging nothing. Otherwise it records the dispatch and the root's serial, then writes.
//
// The DNS write takes no idempotency key: POST /dns/internal/acme-challenge's body is the name and the values, and
// every write takes a new serial, which §13.8's rewrite after stale needs. A lost acknowledgement, a write whose
// answer never came, a 503 (which a failed commit answers too) or a worker stopped mid-call, is recovered from the
// zone journal: every commit of the dispatch has a serial above the one kept, so the first journal row above it that
// upserts the digest in the name's TXT is the write, adopted with its serial and never charged again. The read takes
// the zone's lock, which a write of <root>'s own rows holds through its commit, so a write inside its transaction
// has ended. Only SettleAfter past the dispatch, longer than certs' DNS client waits for an answer, with no such row,
// is the write read as never made and owed again: an interrupted write is settled before another is charged against
// the ten-per-name hourly cap. A request the DNS API read but had not begun committing by then ends with the client's
// request; a write that still commits past it costs one more charge, never a wrong value, since it writes the same
// digest.
//
// Every transaction here is read committed: the account's lock orders captures and steps of one domain, and each
// statement after it reads what committed before it. Locks are taken account first, then the receipt, as a claim
// takes the account before its row. The tables are MG5's and the acme names MG6's: a binary that does not link what
// custom hosts read answers ErrDnsUnlinked.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
)

// A receipt's states (see the file header).
const (
	TlsCustomChallengeStateQueued     = "queued"
	TlsCustomChallengeStateDispatched = "dispatched"
	TlsCustomChallengeStateWritten    = "written"
	TlsCustomChallengeStateRefused    = "refused"
)

// Why a receipt was refused.
const (
	// The account's lock is no longer the receipt's claim, sidecar and row, or the row left custom_ordering under it.
	TlsCustomChallengeRefusalClaim = "claim"
	// The domain no longer holds the receipt's acme name.
	TlsCustomChallengeRefusalNoAccount = "no_account"
	// The name's writes of the window reached dns_challenges_per_name_hour; the window frees at the retry time.
	TlsCustomChallengeRefusalBudget = "budget"
	// The DNS path refused the write for another reason.
	TlsCustomChallengeRefusalRefused = "refused"
	// DispatchMaxCount dispatches of one capture were settled as never made.
	TlsCustomChallengeRefusalAttempts = "attempts"
)

// GET /dns/ready's answer a written receipt records as final for its serial (§13.8: stale, a restore's).
const TlsCustomChallengeReadyStale = string(certsservice.CertsDnsStale)

// Tunables of the handoff (QD-702). Every field must be positive.
type TlsCustomChallengeSettings struct {
	// How long a receipt is kept after its capture: the challenge's own lifetime, DnsModelSettings.ChallengeLifetime,
	// so no receipt outlives the TXT it records (QD-1165).
	ReceiptLifetime time.Duration
	// How long POST waits for the worker's serial before it answers 503, the receipt kept for a retry: under the
	// lb's 30 s.
	WriteWait time.Duration
	// How long GET waits for the worker's fresh readiness answer before it answers waiting.
	ReadyWait time.Duration
	// How often a waiting POST or GET reads the receipt again.
	PollInterval time.Duration
	// A readiness answer younger than this is answered again without another read of /dns/ready: about the
	// ordering desktop's own poll interval.
	ReadyFreshness time.Duration
	// How long after a dispatch whose answer never came its absence from the journal reads as no write: past certs'
	// DNS client's request timeout (server.CertsDnsClientSettings.RequestTimeout, 15 s), so the request has ended.
	SettleAfter time.Duration
	// The most dispatches one capture makes; each one settled as never made counts.
	DispatchMaxCount int
	// The worker command's task max time: past a write's and a ready read's timeouts together.
	WorkMaxTime time.Duration
	// The most steps one run of the command takes, each one transaction and at most one DNS call.
	WorkStepMaxCount int
	// The most expired receipts one purge pass deletes; a full pass asks for the next sooner.
	PurgeBatchSize int
}

func DefaultTlsCustomChallengeSettings() *TlsCustomChallengeSettings {
	return &TlsCustomChallengeSettings{
		ReceiptLifetime:  DefaultDnsModelSettings().ChallengeLifetime,
		WriteWait:        20 * time.Second,
		ReadyWait:        10 * time.Second,
		PollInterval:     250 * time.Millisecond,
		ReadyFreshness:   2 * time.Second,
		SettleAfter:      1 * time.Minute,
		DispatchMaxCount: 3,
		WorkMaxTime:      2 * time.Minute,
		WorkStepMaxCount: 8,
		PurgeBatchSize:   1000,
	}
}

// Whether every field can run.
func (self *TlsCustomChallengeSettings) Validate() error {
	if self.ReceiptLifetime <= 0 || self.WriteWait <= 0 || self.ReadyWait <= 0 || self.PollInterval <= 0 ||
		self.ReadyFreshness <= 0 || self.SettleAfter <= 0 || self.DispatchMaxCount <= 0 || self.WorkMaxTime <= 0 ||
		self.WorkStepMaxCount <= 0 || self.PurgeBatchSize <= 0 {
		return errors.New("tls custom challenge settings: every field must be positive")
	}
	return nil
}

// A receipt as POST and GET read it.
type TlsCustomChallengeReceipt struct {
	ChallengeId server.Id
	NetworkId   server.Id
	Domain      string
	ClaimSeq    uint64
	CertId      server.Id
	SidecarId   server.Id
	State       string
	// The write's serial, once written.
	Serial uint64
	// A refused receipt's reason.
	Refusal string
	// A budget refusal's wait from Now until the window frees, zero for none known.
	RetryAfter time.Duration
	// The newest answer of GET /dns/ready for Serial, "" for none.
	ReadyAnswer string
	// When ReadyAnswer was read, zero for none.
	ReadyTime time.Time
	// When the read was made, on the store's clock.
	Now time.Time
}

// One row of network_tls_custom_challenge.
type tlsCustomChallengeRow struct {
	challengeId      server.Id
	networkId        server.Id
	domain           string
	claimSeq         uint64
	certId           server.Id
	sidecarId        server.Id
	root             string
	name             string
	value            string
	state            string
	captureTime      time.Time
	expireTime       time.Time
	dispatchCount    int
	dispatchTime     *time.Time
	floorSerial      *int64
	serial           *int64
	writeTime        *time.Time
	refusal          *string
	retryTime        *time.Time
	readyAnswer      *string
	readyTime        *time.Time
	readyRequestTime *time.Time
}

const tlsCustomChallengeColumns = `
	challenge_id, network_id, domain, claim_seq, cert_id, sidecar_id, root, name, value, state, capture_time,
	expire_time, dispatch_count, dispatch_time, floor_serial, serial, write_time, refusal, retry_time, ready_answer,
	ready_time, ready_request_time
`

func scanTlsCustomChallenge(row pgx.Row) (*tlsCustomChallengeRow, error) {
	challenge := &tlsCustomChallengeRow{}
	var claimSeq int64
	err := row.Scan(
		&challenge.challengeId,
		&challenge.networkId,
		&challenge.domain,
		&claimSeq,
		&challenge.certId,
		&challenge.sidecarId,
		&challenge.root,
		&challenge.name,
		&challenge.value,
		&challenge.state,
		&challenge.captureTime,
		&challenge.expireTime,
		&challenge.dispatchCount,
		&challenge.dispatchTime,
		&challenge.floorSerial,
		&challenge.serial,
		&challenge.writeTime,
		&challenge.refusal,
		&challenge.retryTime,
		&challenge.readyAnswer,
		&challenge.readyTime,
		&challenge.readyRequestTime,
	)
	if err != nil {
		return nil, err
	}
	challenge.claimSeq = uint64(claimSeq)
	return challenge, nil
}

// The receipt a where clause selects, the newest claim first, locked for the transaction when forUpdate; nil for none.
func getTlsCustomChallengeInTx(ctx context.Context, tx server.PgTx, where string, forUpdate bool, args ...any) *tlsCustomChallengeRow {
	sql := `SELECT ` + tlsCustomChallengeColumns + ` FROM network_tls_custom_challenge WHERE ` + where + ` ORDER BY claim_seq DESC LIMIT 1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	challenge, err := scanTlsCustomChallenge(tx.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	server.Raise(err)
	return challenge
}

// The row as POST and GET read it at now.
func (self *tlsCustomChallengeRow) receipt(now time.Time) *TlsCustomChallengeReceipt {
	receipt := &TlsCustomChallengeReceipt{
		ChallengeId: self.challengeId,
		NetworkId:   self.networkId,
		Domain:      self.domain,
		ClaimSeq:    self.claimSeq,
		CertId:      self.certId,
		SidecarId:   self.sidecarId,
		State:       self.state,
		Now:         now,
	}
	if self.serial != nil {
		receipt.Serial = uint64(*self.serial)
	}
	if self.refusal != nil {
		receipt.Refusal = *self.refusal
	}
	if self.retryTime != nil && now.Before(*self.retryTime) {
		receipt.RetryAfter = self.retryTime.Sub(now)
	}
	if self.readyAnswer != nil {
		receipt.ReadyAnswer = *self.readyAnswer
	}
	if self.readyTime != nil {
		receipt.ReadyTime = *self.readyTime
	}
	return receipt
}

// Whether a stale verdict for its serial was recorded: final for the serial (§13.8).
func (self *tlsCustomChallengeRow) stale() bool {
	return self.readyAnswer != nil && *self.readyAnswer == TlsCustomChallengeReadyStale
}

// Sets columns of the receipt in tx.
func updateTlsCustomChallengeInTx(ctx context.Context, tx server.PgTx, challengeId server.Id, sql string, values ...any) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_tls_custom_challenge SET `+sql+` WHERE challenge_id = $1`,
		append([]any{challengeId}, values...)...,
	))
}

// Owes a new write of value under the receipt's claim: everything of the earlier write is cleared, and the expiry
// counts from now.
func requeueTlsCustomChallengeInTx(ctx context.Context, tx server.PgTx, challengeId server.Id, value string, now time.Time, settings *TlsCustomChallengeSettings) {
	updateTlsCustomChallengeInTx(
		ctx,
		tx,
		challengeId,
		`
			value = $2, state = $3, capture_time = $4, expire_time = $5, dispatch_count = 0, dispatch_time = NULL,
			floor_serial = NULL, serial = NULL, write_time = NULL, refusal = NULL, retry_time = NULL, ready_answer = NULL,
			ready_time = NULL, ready_request_time = NULL
		`,
		value,
		TlsCustomChallengeStateQueued,
		now,
		now.Add(settings.ReceiptLifetime),
	)
}

// Refuses the receipt for reason, retryTime set for a budget refusal, nil otherwise.
func refuseTlsCustomChallengeInTx(ctx context.Context, tx server.PgTx, challengeId server.Id, reason string, retryTime *time.Time) {
	updateTlsCustomChallengeInTx(ctx, tx, challengeId, `state = $2, refusal = $3, retry_time = $4`, TlsCustomChallengeStateRefused, reason, retryTime)
}

// The budget refusal of the name's window at now: its retry time, nil while the window is not full or holds nothing.
func tlsCustomChallengeBudgetInTx(ctx context.Context, tx server.PgTx, row *tlsCustomChallengeRow, now time.Time) (full bool, retryTime *time.Time) {
	count, freeTime := GetDnsAcmeChallengeWindowInTx(ctx, tx, row.root, row.name, now)
	if int64(count) < DnsChallengesPerNameHour() {
		return false, nil
	}
	if freeTime.IsZero() {
		// a cap of zero refuses every write: no time frees it
		return true, nil
	}
	return true, &freeTime
}

// Runs callback in one read committed transaction, answering a raise as an error.
func tlsCustomChallengeTx(ctx context.Context, callback func(tx server.PgTx)) error {
	return tlsCustomRun(func() {
		server.Tx(ctx, callback, server.TxReadCommitted)
	})
}

// POST /tls/custom-challenge's capture (see the file header): the receipt as it stands after it, a recovered serial
// among it. schedule queues the worker's command for the receipt in the capture's transaction, called whenever a
// write is owed or is still being made. A refusal is a *sidecar.TlsCustomError: 403 grant, 404 no_account, 409 claim
// for a caller not holding the account's lock for an order, and 409 in_flight while another digest's write under
// the claim is unheard.
func (self *TlsCustomStore) CaptureChallenge(ctx context.Context, caller *TlsCustomCaller, domain string, value string, settings *TlsCustomChallengeSettings, schedule func(tx server.PgTx, challengeId server.Id)) (receipt *TlsCustomChallengeReceipt, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if caller == nil || domain == "" || value == "" {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	if err := tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		receipt, returnErr = self.captureChallengeInTx(ctx, tx, caller, domain, value, settings, schedule)
	}); err != nil {
		return nil, err
	}
	return
}

func (self *TlsCustomStore) captureChallengeInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, domain string, value string, settings *TlsCustomChallengeSettings, schedule func(tx server.PgTx, challengeId server.Id)) (*TlsCustomChallengeReceipt, error) {
	now := self.nowInTx(ctx, tx)
	if !self.ordererInTx(ctx, tx, caller, domain, false) {
		return nil, tlsCustomRefusal(http.StatusForbidden, sidecar.TlsCustomRefusalGrant)
	}
	account := getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, domain, true)
	if account == nil {
		return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
	}
	// the caller's claim: the account's lock taken for an order, its row ordering under it (17.2.6); the lease is not
	// read, as compare-and-move does not read it
	if account.lockSidecarId == nil || *account.lockSidecarId != caller.SidecarId || account.lockSeq == 0 || account.lockCertId == nil ||
		account.lockPurpose == nil || *account.lockPurpose != sidecar.TlsCustomPurposeOrder {
		return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	rows := getTlsCustomCertsInTx(ctx, tx, `cert_id = $1 AND network_id = $2 AND custom_domain = $3`, *account.lockCertId, account.networkId, account.domain)
	if len(rows) == 0 || rows[0].state != sidecar.TlsCertStateCustomOrdering || rows[0].claimSeq != account.lockSeq {
		return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	acmeToken := getTlsCustomAcmeTokenInTx(ctx, tx, account.networkId, account.domain)
	if acmeToken == "" {
		return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
	}
	root := dnsLabelRoot(self.roots[0])
	name := DnsAcmeChallengeTarget(root, acmeToken)

	row := getTlsCustomChallengeInTx(ctx, tx, `network_id = $1 AND domain = $2 AND claim_seq = $3`, true, account.networkId, account.domain, int64(account.lockSeq))
	if row != nil && !now.Before(row.expireTime) {
		// past its retention, a receipt is no one's: the claim's next write starts afresh
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_tls_custom_challenge WHERE challenge_id = $1`, row.challengeId))
		row = nil
	}
	if row == nil {
		challengeId := server.NewId()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_tls_custom_challenge (
					challenge_id, network_id, domain, claim_seq, cert_id, sidecar_id, root, name, value, state, capture_time,
					expire_time
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			`,
			challengeId,
			account.networkId,
			account.domain,
			int64(account.lockSeq),
			*account.lockCertId,
			caller.SidecarId,
			root,
			name,
			value,
			TlsCustomChallengeStateQueued,
			now,
			now.Add(settings.ReceiptLifetime),
		))
		schedule(tx, challengeId)
		return getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, false, challengeId).receipt(now), nil
	}
	owed := false
	switch {
	case row.value == value && row.state == TlsCustomChallengeStateWritten:
		// a retry recovers the write; only a confirmed stale verdict owes another of the same digest
		owed = row.stale()
	case row.value == value && (row.state == TlsCustomChallengeStateQueued || row.state == TlsCustomChallengeStateDispatched):
		// the write is owed already: asking again only wakes a command a restart may have left
		schedule(tx, row.challengeId)
	case row.value == value && row.state == TlsCustomChallengeStateRefused:
		// a full window keeps its refusal until it frees; any other refusal is retried by a new capture
		owed = !(row.refusal != nil && *row.refusal == TlsCustomChallengeRefusalBudget && row.retryTime != nil && now.Before(*row.retryTime))
	case row.state == TlsCustomChallengeStateDispatched:
		// another digest under the claim waits on a write not yet settled, which may still be charged
		refusal := tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalInFlight)
		refusal.Refusal.CertId = sidecar.Id(row.certId)
		if row.dispatchTime != nil {
			refusal.Refusal.RetryAfterMs = tlsCustomMs(row.dispatchTime.Add(settings.SettleAfter).Sub(now))
		}
		return nil, refusal
	default:
		// another digest, a new order's under the claim, with nothing of the earlier write in flight
		owed = true
	}
	if owed {
		requeueTlsCustomChallengeInTx(ctx, tx, row.challengeId, value, now, settings)
		schedule(tx, row.challengeId)
	}
	return getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, false, row.challengeId).receipt(now), nil
}

// The receipt challengeId as it stands, nil for none or one past its retention.
func (self *TlsCustomStore) ChallengeReceipt(ctx context.Context, challengeId server.Id) (receipt *TlsCustomChallengeReceipt, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	returnErr = tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		now := self.nowInTx(ctx, tx)
		if row := getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, false, challengeId); row != nil && now.Before(row.expireTime) {
			receipt = row.receipt(now)
		}
	})
	return
}

// GET /tls/custom-challenge/<id>'s receipt: the caller's own, of its network and sidecar, written at serial and not
// past its retention, the newest claim's should two adopt one write; nil for none.
func (self *TlsCustomStore) ChallengeReceiptBySerial(ctx context.Context, caller *TlsCustomCaller, serial uint64) (receipt *TlsCustomChallengeReceipt, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if caller == nil || serial == 0 || math.MaxInt64 < serial {
		return nil, nil
	}
	returnErr = tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		now := self.nowInTx(ctx, tx)
		row := getTlsCustomChallengeInTx(
			ctx,
			tx,
			`network_id = $1 AND serial = $2 AND sidecar_id = $3 AND state = $4 AND $5 < expire_time`,
			false,
			caller.NetworkId,
			int64(serial),
			caller.SidecarId,
			TlsCustomChallengeStateWritten,
			now,
		)
		if row != nil {
			receipt = row.receipt(now)
		}
	})
	return
}

// GET's ask for a fresh readiness answer of the receipt's serial: stamps the request and, in the same transaction,
// queues the worker's command through schedule. Answers the request's time, zero when the receipt is no longer
// written at serial or holds its final stale answer, and nothing was asked.
func (self *TlsCustomStore) RequestChallengeReadiness(ctx context.Context, challengeId server.Id, serial uint64, schedule func(tx server.PgTx, challengeId server.Id)) (requestTime time.Time, returnErr error) {
	if !dnsCustomHostLinked() {
		return time.Time{}, ErrDnsUnlinked
	}
	returnErr = tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		now := self.nowInTx(ctx, tx)
		row := getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, true, challengeId)
		if row == nil || row.state != TlsCustomChallengeStateWritten || row.serial == nil || uint64(*row.serial) != serial || row.stale() || !now.Before(row.expireTime) {
			return
		}
		updateTlsCustomChallengeInTx(ctx, tx, challengeId, `ready_request_time = $2`, now)
		schedule(tx, challengeId)
		requestTime = now
	})
	return
}

// What the worker does next for a receipt (ChallengeWorkStep). Nothing set: nothing more is owed this run.
type TlsCustomChallengeStep struct {
	// A write to send through certs' DNS client, the digest at the name, and the dispatch it is, for
	// RecordChallengeWrite. Empty for none.
	WriteName     string
	WriteValue    string
	DispatchCount int
	// A serial to ask GET /dns/ready about, for RecordChallengeReadiness. Zero for none.
	ReadySerial uint64
	// Nothing is owed before this time: an unheard dispatch waiting out SettleAfter. Zero for none.
	WaitUntil time.Time
}

// The worker's next step for challengeId, one transaction (see the file header): it settles an unheard dispatch, and
// for a write owed rechecks the claim and the name's window, then records the dispatch it answers; for a written
// receipt whose readiness GET asked for since the last answer, the serial to ask about.
func (self *TlsCustomStore) ChallengeWorkStep(ctx context.Context, challengeId server.Id, settings *TlsCustomChallengeSettings) (step *TlsCustomChallengeStep, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	returnErr = tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		step = self.challengeWorkStepInTx(ctx, tx, challengeId, settings)
	})
	return
}

func (self *TlsCustomStore) challengeWorkStepInTx(ctx context.Context, tx server.PgTx, challengeId server.Id, settings *TlsCustomChallengeSettings) *TlsCustomChallengeStep {
	now := self.nowInTx(ctx, tx)
	peek := getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, false, challengeId)
	if peek == nil || !now.Before(peek.expireTime) {
		return &TlsCustomChallengeStep{}
	}
	// the account's lock first, as a capture and a claim take it, then the receipt
	account := getTlsCustomAccountInTx(ctx, tx, peek.networkId, peek.domain, true)
	row := getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, true, challengeId)
	if row == nil || !now.Before(row.expireTime) {
		return &TlsCustomChallengeStep{}
	}
	if row.state == TlsCustomChallengeStateDispatched {
		// settle the unheard dispatch from the journal before anything is charged again
		floorSerial := uint64(0)
		if row.floorSerial != nil {
			floorSerial = uint64(*row.floorSerial)
		}
		serial := FindDnsAcmeChallengeCommitInTx(ctx, tx, row.root, row.name, row.value, floorSerial)
		switch {
		case serial != 0:
			updateTlsCustomChallengeInTx(ctx, tx, row.challengeId, `state = $2, serial = $3, write_time = $4`, TlsCustomChallengeStateWritten, int64(serial), now)
			serialColumn := int64(serial)
			row.state = TlsCustomChallengeStateWritten
			row.serial = &serialColumn
		case row.dispatchTime != nil && now.Before(row.dispatchTime.Add(settings.SettleAfter)):
			// the request may still commit
			return &TlsCustomChallengeStep{WaitUntil: row.dispatchTime.Add(settings.SettleAfter)}
		default:
			// past SettleAfter with no commit: never made, so nothing was charged
			updateTlsCustomChallengeInTx(ctx, tx, row.challengeId, `state = $2, dispatch_time = NULL, floor_serial = NULL`, TlsCustomChallengeStateQueued)
			row.state = TlsCustomChallengeStateQueued
		}
	}
	switch row.state {
	case TlsCustomChallengeStateQueued:
		if refusal := row.recheckInTx(ctx, tx, account); refusal != "" {
			refuseTlsCustomChallengeInTx(ctx, tx, row.challengeId, refusal, nil)
			return &TlsCustomChallengeStep{}
		}
		if settings.DispatchMaxCount <= row.dispatchCount {
			refuseTlsCustomChallengeInTx(ctx, tx, row.challengeId, TlsCustomChallengeRefusalAttempts, nil)
			return &TlsCustomChallengeStep{}
		}
		if full, retryTime := tlsCustomChallengeBudgetInTx(ctx, tx, row, now); full {
			refuseTlsCustomChallengeInTx(ctx, tx, row.challengeId, TlsCustomChallengeRefusalBudget, retryTime)
			return &TlsCustomChallengeStep{}
		}
		// every commit of the dispatch takes a serial above the root's now
		floorSerial := uint64(0)
		if state := getDnsRootStateInTx(ctx, tx, dnsLabelRoot(row.root)); state != nil {
			floorSerial = state.Serial
		}
		dispatchCount := row.dispatchCount + 1
		updateTlsCustomChallengeInTx(
			ctx,
			tx,
			row.challengeId,
			`state = $2, dispatch_count = $3, dispatch_time = $4, floor_serial = $5`,
			TlsCustomChallengeStateDispatched,
			dispatchCount,
			now,
			dnsSerialColumn(floorSerial),
		)
		return &TlsCustomChallengeStep{WriteName: row.name, WriteValue: row.value, DispatchCount: dispatchCount}
	case TlsCustomChallengeStateWritten:
		asked := row.readyRequestTime != nil && (row.readyTime == nil || row.readyTime.Before(*row.readyRequestTime))
		if asked && !row.stale() && row.serial != nil {
			return &TlsCustomChallengeStep{ReadySerial: uint64(*row.serial)}
		}
	}
	return &TlsCustomChallengeStep{}
}

// Whether the receipt's claim still holds in tx, account read under its lock: its refusal, "" when it holds.
func (self *tlsCustomChallengeRow) recheckInTx(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow) string {
	if account == nil {
		return TlsCustomChallengeRefusalNoAccount
	}
	if account.lockSidecarId == nil || *account.lockSidecarId != self.sidecarId || account.lockSeq != self.claimSeq ||
		account.lockCertId == nil || *account.lockCertId != self.certId || account.lockPurpose == nil ||
		*account.lockPurpose != sidecar.TlsCustomPurposeOrder {
		return TlsCustomChallengeRefusalClaim
	}
	rows := getTlsCustomCertsInTx(ctx, tx, `cert_id = $1 AND network_id = $2 AND custom_domain = $3`, self.certId, self.networkId, self.domain)
	if len(rows) == 0 || rows[0].state != sidecar.TlsCertStateCustomOrdering || rows[0].claimSeq != self.claimSeq {
		return TlsCustomChallengeRefusalClaim
	}
	acmeToken := getTlsCustomAcmeTokenInTx(ctx, tx, self.networkId, self.domain)
	if acmeToken == "" || DnsAcmeChallengeTarget(self.root, acmeToken) != self.name {
		return TlsCustomChallengeRefusalNoAccount
	}
	return ""
}

// What the dispatch's write answered: its serial, or the error certs' DNS client gave. A refusal of the DNS path
// (certsservice.ErrCertsDnsWriteRefused, a 400 or 429) wrote nothing, and the receipt is refused: budget while the
// name's window is full, refused otherwise. Any other error, an answer lost or a 503, leaves the dispatch unheard for
// the next step to settle. An answer for a dispatch the receipt has moved past changes nothing.
func (self *TlsCustomStore) RecordChallengeWrite(ctx context.Context, challengeId server.Id, dispatchCount int, serial uint64, writeErr error) error {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	return tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		now := self.nowInTx(ctx, tx)
		row := getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, true, challengeId)
		if row == nil || row.state != TlsCustomChallengeStateDispatched || row.dispatchCount != dispatchCount {
			return
		}
		switch {
		case writeErr == nil && serial != 0 && serial <= math.MaxInt64:
			updateTlsCustomChallengeInTx(ctx, tx, challengeId, `state = $2, serial = $3, write_time = $4`, TlsCustomChallengeStateWritten, int64(serial), now)
		case errors.Is(writeErr, certsservice.ErrCertsDnsWriteRefused):
			if full, retryTime := tlsCustomChallengeBudgetInTx(ctx, tx, row, now); full {
				refuseTlsCustomChallengeInTx(ctx, tx, challengeId, TlsCustomChallengeRefusalBudget, retryTime)
			} else {
				refuseTlsCustomChallengeInTx(ctx, tx, challengeId, TlsCustomChallengeRefusalRefused, nil)
			}
		}
	})
}

// GET /dns/ready's answer for the receipt's serial, when the receipt is still written at it. stale is final for the
// serial, and lets the next capture of the same digest owe a new write.
func (self *TlsCustomStore) RecordChallengeReadiness(ctx context.Context, challengeId server.Id, serial uint64, answer string) error {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	if answer == "" {
		return fmt.Errorf("tls custom challenge: no readiness answer")
	}
	return tlsCustomChallengeTx(ctx, func(tx server.PgTx) {
		now := self.nowInTx(ctx, tx)
		row := getTlsCustomChallengeInTx(ctx, tx, `challenge_id = $1`, true, challengeId)
		if row == nil || row.state != TlsCustomChallengeStateWritten || row.serial == nil || uint64(*row.serial) != serial || row.stale() {
			return
		}
		updateTlsCustomChallengeInTx(ctx, tx, challengeId, `ready_answer = $2, ready_time = $3`, answer, now)
	})
}

// Deletes receipts past their retention at now, at most batchSize: how many went, and whether a full batch left more.
func PurgeExpiredTlsCustomChallenges(ctx context.Context, now time.Time, batchSize int) (purgedCount int, more bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return 0, false, nil
	}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					DELETE FROM network_tls_custom_challenge
					WHERE challenge_id IN (
						SELECT challenge_id FROM network_tls_custom_challenge
						WHERE expire_time <= $1
						ORDER BY expire_time
						LIMIT $2
					)
				`,
				now.UTC(),
				batchSize,
			))
			purgedCount = int(tag.RowsAffected())
		}, server.TxReadCommitted)
	})
	return purgedCount, purgedCount == batchSize, returnErr
}
