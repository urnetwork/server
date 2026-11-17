package model

// Team custom-domain certificates (TLS-M6-05; DESIGN-DETAILS 17.2.3 to 17.2.14; §13.8, §13.15; cert-1): the
// customer's ACME account row with its lock (network_tls_account), the budgets' spends
// (network_tls_custom_spend), and the custom rows of network_tls_cert in the four states only this file moves:
//
//	custom_queued     the CSR waits for an ordering desktop
//	custom_ordering   a desktop took it under the lock; it stays here while its order is at the CA, lock or no lock
//	custom_issued     its chain is registered
//	custom_revoking   a revocation is due, which only an ordering desktop can send
//
// Every operation is one transaction, and the lock's clock is the database's now() and nothing else (17.2.6): a
// claim runs under FOR UPDATE on the account row, raises lock_seq, and every later write made for an order, a
// revocation or a key change applies only while lock_seq is the one its claim returned (17.3's C2, C4). The grant
// of a newOrder checks and spends the budgets in the claim's own transaction, after an advisory lock on the CA and
// registered domain, so two accounts of one registered domain grant in turn (C5). A row with an order recorded
// never simply fails: it goes to custom_revoking, whose pass settles the order before the account orders again
// (C10). Every custom move goes through CertsTransition, with its At and, while a row is custom_revoking, its
// RevocationReason (QD-741, QD-742).
//
// The orderer rule is the controller's §8 read, passed in as TlsCustomCaller.PublishGranted, applied here to the
// owners of the domain's active custom hosts as this transaction reads them. The account key is never here: only
// its public RFC 7638 thumbprint and the secret repo that holds it. Nothing here names a host in a log (I-13).
//
// The tables are MG5's: a binary that does not link what custom hosts read answers ErrDnsUnlinked.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/publicsuffix"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// The admin events of custom certificates (17.2.14), the network as the target, no host said (I-13).
const (
	AdminEventActionTlsCustom       = "tls.custom"
	AdminEventTargetTlsCustomDomain = "tls_custom"
	// a desktop saw a key change due it may not make
	AdminEventResultTlsAccountRolloverDue = "tls_account_rollover_due"
	AdminEventResultTlsCustomRenewalDue   = "tls_custom_renewal_due"
	AdminEventResultTlsCustomRenewalLate  = "tls_custom_renewal_late"
	AdminEventResultTlsCustomOneOrderer   = "tls_custom_one_orderer"
	AdminEventResultTlsCustomQueueStalled = "tls_custom_queue_stalled"
	// tls_custom_<reason> for a progress event carrying a reason
	adminEventResultTlsCustomPrefix = "tls_custom_"
)

// Who calls a custom route, from its certs token and its network's verified projection.
type TlsCustomCaller struct {
	NetworkId server.Id
	ClientId  server.Id
	SidecarId server.Id
	// Whether §8's gate holds an active approved publish grant for the caller on a tls/ name of the owner label
	// ownerLabel now. nil holds none.
	PublishGranted func(ownerLabel string) bool
	// Whether the caller's user is a network admin.
	Admin bool
	// The caller's identity key from the projection, for the custom branch's attestation.
	IdentityPublicKey []byte
}

// Options of the store.
type TlsCustomStoreOptions struct {
	Settings *certsservice.CertsCustomSettings
	// certs' roots, under none of which a custom host sits. Required.
	Roots []string
	// The time a transaction runs at. nil: the database's now(). Tests set a fake clock's.
	Now func() time.Time
}

// See the file header. Safe for concurrent use.
type TlsCustomStore struct {
	settings *certsservice.CertsCustomSettings
	roots    []string
	now      func() time.Time
}

func NewTlsCustomStore(options *TlsCustomStoreOptions) (*TlsCustomStore, error) {
	settings := options.Settings
	if settings == nil {
		settings = certsservice.DefaultCertsCustomSettings()
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if len(options.Roots) == 0 {
		return nil, errors.New("the custom store needs certs' roots")
	}
	roots := []string{}
	for _, root := range options.Roots {
		roots = append(roots, strings.ToLower(strings.TrimSuffix(root, ".")))
	}
	return &TlsCustomStore{settings: settings, roots: roots, now: options.Now}, nil
}

// The transaction's now: the database's, or the test's clock.
func (self *TlsCustomStore) nowInTx(ctx context.Context, tx server.PgTx) time.Time {
	if self.now != nil {
		return self.now().UTC()
	}
	var now time.Time
	server.Raise(tx.QueryRow(ctx, `SELECT now() AT TIME ZONE 'UTC'`).Scan(&now))
	return now
}

// Runs callback, answering a raise, a database error or a canceled context among them, as an error, so a route's
// caller reads it as the store's failure rather than a panic.
func tlsCustomRun(callback func()) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
				return
			}
			returnErr = fmt.Errorf("tls custom store: %v", r)
		}
	}()
	callback()
	return nil
}

// A refusal with its body.
func tlsCustomRefusal(status int, reason string) *sidecar.TlsCustomError {
	return &sidecar.TlsCustomError{Status: status, Refusal: sidecar.TlsCustomRefusal{Reason: reason}}
}

// Milliseconds of d from the answer, at least 1 for a positive wait, saturating.
func tlsCustomMs(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	return uint32(min(ms, time.Duration(^uint32(0))))
}

// One account row.
type tlsCustomAccountRow struct {
	networkId         server.Id
	domain            string
	ca                string
	accountUri        string
	keySeq            uint64
	keyThumbprint     []byte
	keyRepoId         server.Id
	nextKeyThumbprint []byte
	nextKeyRepoId     *server.Id
	lockSeq           uint64
	lockSidecarId     *server.Id
	lockCertId        *server.Id
	lockPurpose       *string
	lockUntil         *time.Time
	holdNewUntil      *time.Time
	holdAllUntil      *time.Time
	ordererSeen       map[string]time.Time
}

const tlsCustomAccountColumns = `
	network_id, domain, ca, account_uri, key_seq, key_thumbprint, key_repo_id, next_key_thumbprint, next_key_repo_id,
	lock_seq, lock_sidecar_id, lock_cert_id, lock_purpose, lock_until, hold_new_until, hold_all_until, orderer_seen
`

func scanTlsCustomAccount(row pgx.Row) (*tlsCustomAccountRow, error) {
	account := &tlsCustomAccountRow{}
	var keySeq int64
	var lockSeq int64
	var ordererSeen []byte
	err := row.Scan(
		&account.networkId,
		&account.domain,
		&account.ca,
		&account.accountUri,
		&keySeq,
		&account.keyThumbprint,
		&account.keyRepoId,
		&account.nextKeyThumbprint,
		&account.nextKeyRepoId,
		&lockSeq,
		&account.lockSidecarId,
		&account.lockCertId,
		&account.lockPurpose,
		&account.lockUntil,
		&account.holdNewUntil,
		&account.holdAllUntil,
		&ordererSeen,
	)
	if err != nil {
		return nil, err
	}
	account.keySeq = uint64(keySeq)
	account.lockSeq = uint64(lockSeq)
	account.ordererSeen = map[string]time.Time{}
	if 0 < len(ordererSeen) {
		// a stamp map that does not parse is an empty one: it only counts desktops for a warning
		json.Unmarshal(ordererSeen, &account.ordererSeen)
	}
	return account, nil
}

// The account row, locked for the transaction when forUpdate, nil for none.
func getTlsCustomAccountInTx(ctx context.Context, tx server.PgTx, networkId server.Id, domain string, forUpdate bool) *tlsCustomAccountRow {
	sql := `SELECT ` + tlsCustomAccountColumns + ` FROM network_tls_account WHERE network_id = $1 AND domain = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	account, err := scanTlsCustomAccount(tx.QueryRow(ctx, sql, networkId, domain))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	server.Raise(err)
	return account
}

// Whether caller holds the account's lock under claimSeq for certId: compare-and-move's check (17.2.5). The lease
// is not read: a holder whose lease lapsed and that nobody has taken over from still writes.
func (self *tlsCustomAccountRow) heldBy(caller *TlsCustomCaller, claimSeq uint64, certId *server.Id) bool {
	if self.lockSidecarId == nil || *self.lockSidecarId != caller.SidecarId || self.lockSeq != claimSeq || claimSeq == 0 {
		return false
	}
	if certId == nil {
		return self.lockCertId == nil
	}
	return self.lockCertId != nil && *self.lockCertId == *certId
}

// Frees the lock in tx.
func freeTlsCustomLockInTx(ctx context.Context, tx server.PgTx, networkId server.Id, domain string) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_tls_account
			SET lock_sidecar_id = NULL, lock_cert_id = NULL, lock_purpose = NULL, lock_until = NULL
			WHERE network_id = $1 AND domain = $2
		`,
		networkId,
		domain,
	))
}

// One custom row.
type tlsCustomCertRow struct {
	certId              server.Id
	networkId           server.Id
	clientId            server.Id
	sidecarId           server.Id
	identifierSetSha256 []byte
	spkiSha256          []byte
	attestation         []byte
	csr                 []byte
	state               sidecar.TlsCertState
	stateTime           time.Time
	successorCertId     *server.Id
	chain               [][]byte
	notBefore           *time.Time
	notAfter            *time.Time
	ariWindowStart      *time.Time
	ariWindowEnd        *time.Time
	customHost          string
	customDomain        string
	orderUrl            *string
	claimSeq            uint64
	exempt              bool
	noReplaces          bool
	customReason        *string
	serial              *string
	accountUri          *string
	revocationReason    *string
}

const tlsCustomCertColumns = `
	cert_id, network_id, client_id, sidecar_id, identifier_set_sha256, spki_sha256, attestation, csr, state, state_time,
	successor_cert_id, chain, not_before, not_after, ari_window_start, ari_window_end, custom_host, custom_domain,
	order_url, claim_seq, exempt, no_replaces, custom_reason, serial, account_uri, revocation_reason
`

func scanTlsCustomCert(row pgx.Row) (*tlsCustomCertRow, error) {
	cert := &tlsCustomCertRow{}
	var state string
	var claimSeq int64
	err := row.Scan(
		&cert.certId,
		&cert.networkId,
		&cert.clientId,
		&cert.sidecarId,
		&cert.identifierSetSha256,
		&cert.spkiSha256,
		&cert.attestation,
		&cert.csr,
		&state,
		&cert.stateTime,
		&cert.successorCertId,
		&cert.chain,
		&cert.notBefore,
		&cert.notAfter,
		&cert.ariWindowStart,
		&cert.ariWindowEnd,
		&cert.customHost,
		&cert.customDomain,
		&cert.orderUrl,
		&claimSeq,
		&cert.exempt,
		&cert.noReplaces,
		&cert.customReason,
		&cert.serial,
		&cert.accountUri,
		&cert.revocationReason,
	)
	if err != nil {
		return nil, err
	}
	cert.state = sidecar.TlsCertState(state)
	cert.claimSeq = uint64(claimSeq)
	return cert, nil
}

// The custom rows a where clause selects, oldest first.
func getTlsCustomCertsInTx(ctx context.Context, query server.PgCanQuery, where string, args ...any) []*tlsCustomCertRow {
	return getTlsCustomCertsLimitInTx(ctx, query, where, 0, args...)
}

// getTlsCustomCertsInTx, at most count rows when count is positive.
func getTlsCustomCertsLimitInTx(ctx context.Context, query server.PgCanQuery, where string, count int, args ...any) []*tlsCustomCertRow {
	rows := []*tlsCustomCertRow{}
	limit := ""
	if 0 < count {
		limit = fmt.Sprintf(" LIMIT %d", count)
	}
	result, err := query.Query(ctx, `SELECT `+tlsCustomCertColumns+` FROM network_tls_cert WHERE custom_host IS NOT NULL AND `+where+` ORDER BY cert_id`+limit, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			cert, err := scanTlsCustomCert(result)
			server.Raise(err)
			rows = append(rows, cert)
		}
	})
	return rows
}

// The custom rows a where clause selects, oldest first, locked for the transaction.
func getTlsCustomCertsForUpdateInTx(ctx context.Context, tx server.PgTx, where string, args ...any) []*tlsCustomCertRow {
	rows := []*tlsCustomCertRow{}
	result, err := tx.Query(ctx, `SELECT `+tlsCustomCertColumns+` FROM network_tls_cert WHERE custom_host IS NOT NULL AND `+where+` ORDER BY cert_id FOR UPDATE`, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			cert, err := scanTlsCustomCert(result)
			server.Raise(err)
			rows = append(rows, cert)
		}
	})
	return rows
}

// The account's row certId, locked for the transaction, nil for none.
func getTlsCustomCertForUpdateInTx(ctx context.Context, tx server.PgTx, networkId server.Id, domain string, certId server.Id) *tlsCustomCertRow {
	cert, err := scanTlsCustomCert(tx.QueryRow(
		ctx,
		`SELECT `+tlsCustomCertColumns+` FROM network_tls_cert WHERE cert_id = $1 AND network_id = $2 AND custom_domain = $3 FOR UPDATE`,
		certId,
		networkId,
		domain,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	server.Raise(err)
	return cert
}

// The row as the wire carries it: TlsCertRecord's fields, the custom ones among them.
func (self *tlsCustomCertRow) record() *sidecar.TlsCertRecord {
	record := &sidecar.TlsCertRecord{
		CertId:              sidecar.Id(self.certId),
		NetworkId:           sidecar.Id(self.networkId),
		ClientId:            sidecar.Id(self.clientId),
		SidecarId:           sidecar.Id(self.sidecarId),
		IdentifierSetSha256: self.identifierSetSha256,
		SpkiSha256:          self.spkiSha256,
		Attestation:         self.attestation,
		Csr:                 self.csr,
		State:               self.state,
		StateTime:           self.stateTime,
		Chain:               self.chain,
		CustomHost:          self.customHost,
		CustomDomain:        self.customDomain,
	}
	if self.successorCertId != nil {
		record.SuccessorCertId = sidecar.Id(*self.successorCertId)
	}
	if self.notBefore != nil {
		record.NotBefore = *self.notBefore
	}
	if self.notAfter != nil {
		record.NotAfter = *self.notAfter
	}
	if self.ariWindowStart != nil && self.ariWindowEnd != nil {
		record.AriWindowStart = *self.ariWindowStart
		record.AriWindowEnd = *self.ariWindowEnd
	}
	if self.customReason != nil {
		record.CustomReason = *self.customReason
	}
	if self.serial != nil {
		record.Serial = *self.serial
	}
	if self.accountUri != nil {
		record.AccountUri = *self.accountUri
	}
	return record
}

// Moves a custom row by CertsTransition's compare-and-move in tx (QD-741, QD-742), and sets extra columns in the
// same statement's transaction. Whether it moved.
func tlsCustomMoveInTx(ctx context.Context, tx server.PgTx, transition *certsservice.CertsTransition) bool {
	moved, err := transitionCertInTx(ctx, tx, transition)
	server.Raise(err)
	return moved
}

// The owner labels of the domain's active custom hosts, each once.
func tlsCustomDomainOwnersInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, domain string) map[string]string {
	hostOwners := map[string]string{}
	for _, row := range getDnsCustomHostRowsInTx(
		ctx,
		query,
		`network_id = $1 AND state = $2 AND (host = $3 OR host LIKE '%.' || $3)`,
		networkId,
		sidecar.DnsCustomHostStateActive,
		domain,
	) {
		if row.StrictMx() {
			continue
		}
		label, _, _ := strings.Cut(row.Owner, ".")
		hostOwners[row.Host] = label
	}
	return hostOwners
}

// 17.2.6's orderer(caller, domain): a sidecar of the network holding an active approved publish grant on a tls/
// name of the owner of an active custom host of the domain; an admin besides, when admin is asked.
func (self *TlsCustomStore) ordererInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, domain string, admin bool) bool {
	if caller == nil || caller.SidecarId == (server.Id{}) || caller.PublishGranted == nil || (admin && !caller.Admin) {
		return false
	}
	for _, ownerLabel := range tlsCustomDomainOwnersInTx(ctx, tx, caller.NetworkId, domain) {
		if caller.PublishGranted(ownerLabel) {
			return true
		}
	}
	return false
}

// The registered domain a custom host counts in for the CA's per-domain limit (17.2.2's rd(c)).
func tlsCustomRegisteredDomain(host string) string {
	registered, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return registered
}

// The times of spends of kind for a key, after since, leaving out excludeCertId's.
func tlsCustomSpendTimesInTx(ctx context.Context, tx server.PgTx, where string, args ...any) []time.Time {
	times := []time.Time{}
	result, err := tx.Query(ctx, `SELECT at FROM network_tls_custom_spend WHERE `+where, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var at time.Time
			server.Raise(result.Scan(&at))
			times = append(times, at)
		}
	})
	return times
}

// Writes a spend of kind for r.
func (self *TlsCustomStore) spendInTx(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow, r *tlsCustomCertRow, kind string, now time.Time) {
	var certId *server.Id
	if kind == certsservice.CertsCustomSpendCertificate {
		certId = &r.certId
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_tls_custom_spend (
				spend_id, network_id, domain, custom_host, registered_domain, ca, account_uri, kind, cert_id, at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (cert_id) WHERE kind = 'certificate' DO UPDATE SET at = EXCLUDED.at
		`,
		server.NewId(),
		account.networkId,
		account.domain,
		r.customHost,
		tlsCustomRegisteredDomain(r.customHost),
		account.ca,
		account.accountUri,
		kind,
		certId,
		now,
	))
}

// Deletes r's certificate spend: the CA issued nothing on its order.
func deleteTlsCustomCertificateSpendInTx(ctx context.Context, tx server.PgTx, certId server.Id) {
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_tls_custom_spend WHERE cert_id = $1 AND kind = 'certificate'`, certId))
}

// The row whose successor is certId, nil for none.
func getTlsCustomPredecessorInTx(ctx context.Context, tx server.PgTx, certId server.Id) *tlsCustomCertRow {
	rows := getTlsCustomCertsInTx(ctx, tx, `successor_cert_id = $1`, certId)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// What grant answered.
type tlsCustomGrant struct {
	exempt       bool
	replacesLeaf []byte
}

// 17.2.8's checks of r at now, spending nothing: the grant, or the refusal and its wait.
func (self *TlsCustomStore) grantCheckInTx(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow, r *tlsCustomCertRow, now time.Time) (*tlsCustomGrant, string, time.Duration) {
	settings := self.settings
	predecessor := getTlsCustomPredecessorInTx(ctx, tx, r.certId)
	predecessorLive := predecessor != nil && predecessor.state == sidecar.TlsCertStateCustomIssued && predecessor.notAfter != nil && now.Before(*predecessor.notAfter)
	exempt := predecessorLive && !r.noReplaces && predecessor.ariWindowStart != nil && predecessor.ariWindowEnd != nil &&
		!now.Before(*predecessor.ariWindowStart) && !now.After(*predecessor.ariWindowEnd)
	// the holds: 429 hold
	if account.holdAllUntil != nil && now.Before(*account.holdAllUntil) {
		return nil, sidecar.TlsCustomRefusalHold, account.holdAllUntil.Sub(now)
	}
	if !exempt && account.holdNewUntil != nil && now.Before(*account.holdNewUntil) {
		return nil, sidecar.TlsCustomRefusalHold, account.holdNewUntil.Sub(now)
	}
	waits := []time.Duration{}
	// failures since the host's newest register
	var newestRegister time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT COALESCE(max(not_before), '0001-01-01') FROM network_tls_cert WHERE custom_host = $1 AND network_id = $2 AND not_before IS NOT NULL`,
		r.customHost,
		account.networkId,
	).Scan(&newestRegister))
	failures := tlsCustomSpendTimesInTx(
		ctx,
		tx,
		`ca = $1 AND account_uri = $2 AND custom_host = $3 AND kind IN ('authz_failed', 'no_route') AND $4 < at`,
		account.ca,
		account.accountUri,
		r.customHost,
		newestRegister,
	)
	waits = append(waits, settings.FailureWait(failures, now))
	// each bucket reads its key's spends of the last SpendRetention, 16 d, all the purge keeps, from which its replay
	// finds where to start (QD-994, QD-996)
	orderBucket := settings.OrderBucket()
	orders := tlsCustomSpendTimesInTx(
		ctx,
		tx,
		`ca = $1 AND account_uri = $2 AND kind = 'new_order' AND $3 < at`,
		account.ca,
		account.accountUri,
		orderBucket.ReplayStart(now),
	)
	waits = append(waits, orderBucket.Wait(orders, now, 1))
	if !exempt {
		need := settings.SetReserve + 1
		if predecessor != nil && (predecessor.state == sidecar.TlsCertStateCustomRevoking || predecessor.state == sidecar.TlsCertStateRevoked) {
			need = 1
		}
		setBucket := settings.SetBucket()
		set := tlsCustomSpendTimesInTx(
			ctx,
			tx,
			`ca = $1 AND custom_host = $2 AND kind = 'certificate' AND $3 < at AND cert_id IS DISTINCT FROM $4`,
			account.ca,
			r.customHost,
			setBucket.ReplayStart(now),
			r.certId,
		)
		waits = append(waits, setBucket.Wait(set, now, need))
		domainBucket := settings.DomainBucket()
		domainSpends := tlsCustomSpendTimesInTx(
			ctx,
			tx,
			`ca = $1 AND registered_domain = $2 AND kind = 'certificate' AND $3 < at AND cert_id IS DISTINCT FROM $4`,
			account.ca,
			tlsCustomRegisteredDomain(r.customHost),
			domainBucket.ReplayStart(now),
			r.certId,
		)
		waits = append(waits, domainBucket.Wait(domainSpends, now, 1))
	}
	if wait := slices.Max(waits); 0 < wait {
		return nil, sidecar.TlsCustomRefusalBudget, wait
	}
	grant := &tlsCustomGrant{exempt: exempt}
	if predecessorLive && !r.noReplaces && 0 < len(predecessor.chain) {
		grant.replacesLeaf = predecessor.chain[0]
	}
	return grant, "", 0
}

// 17.2.8's grant(r) in the claim's or order_invalid's transaction: under an advisory lock on the CA and registered
// domain, the checks, then the spends: a new_order row and, unless exempt, r's certificate row, kept across
// takeovers. The grant, or the refusal and its wait.
func (self *TlsCustomStore) grantInTx(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow, r *tlsCustomCertRow, now time.Time) (*tlsCustomGrant, string, time.Duration) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, account.ca+" "+tlsCustomRegisteredDomain(r.customHost)))
	grant, reason, wait := self.grantCheckInTx(ctx, tx, account, r, now)
	if grant == nil {
		return nil, reason, wait
	}
	self.spendInTx(ctx, tx, account, r, certsservice.CertsCustomSpendNewOrder, now)
	if !grant.exempt {
		self.spendInTx(ctx, tx, account, r, certsservice.CertsCustomSpendCertificate, now)
	}
	server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET exempt = $2 WHERE cert_id = $1`, r.certId, grant.exempt))
	return grant, "", 0
}

// The account's row with an order open: its custom_ordering row, or a custom_revoking row that ended under its
// order, the oldest; nil for none (17.2.6 step 3's f).
func getTlsCustomOpenOrderInTx(ctx context.Context, tx server.PgTx, networkId server.Id, domain string) *server.Id {
	rows := getTlsCustomCertsInTx(
		ctx,
		tx,
		`network_id = $1 AND custom_domain = $2 AND (state = $3 OR (state = $4 AND order_url IS NOT NULL AND chain IS NULL))`,
		networkId,
		domain,
		string(sidecar.TlsCertStateCustomOrdering),
		string(sidecar.TlsCertStateCustomRevoking),
	)
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		if row.state == sidecar.TlsCertStateCustomOrdering {
			return &row.certId
		}
	}
	return &rows[0].certId
}

// POST /tls/custom-claim (17.2.6). See the file header.
func (self *TlsCustomStore) Claim(ctx context.Context, caller *TlsCustomCaller, args *sidecar.TlsCustomClaimArgs) (result *sidecar.TlsCustomClaimResult, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if args == nil || args.Domain == "" {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	if err := tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = self.claimInTx(ctx, tx, caller, args)
		})
	}); err != nil {
		return nil, err
	}
	return
}

func (self *TlsCustomStore) claimInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, args *sidecar.TlsCustomClaimArgs) (*sidecar.TlsCustomClaimResult, error) {
	now := self.nowInTx(ctx, tx)
	account := getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, args.Domain, true)
	if account == nil {
		return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
	}
	if !self.ordererInTx(ctx, tx, caller, args.Domain, args.Purpose == sidecar.TlsCustomPurposeRollover) {
		return nil, tlsCustomRefusal(http.StatusForbidden, sidecar.TlsCustomRefusalOrderer)
	}
	if args.KeySeq != account.keySeq && !(account.nextKeyThumbprint != nil && args.KeySeq == account.keySeq+1) {
		refusal := tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalKeySeq)
		refusal.Refusal.KeySeq = account.keySeq
		return nil, refusal
	}
	var certId *server.Id
	switch args.Purpose {
	case sidecar.TlsCustomPurposeOrder, sidecar.TlsCustomPurposeRevoke:
		if args.CertId == (sidecar.Id{}) {
			return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
		}
		id := server.Id(args.CertId)
		certId = &id
	case sidecar.TlsCustomPurposeRollover:
		if args.CertId != (sidecar.Id{}) {
			return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
		}
	default:
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	leaseMs := tlsCustomMs(self.settings.LockLease)

	// 2: a renewal, or a lock someone else holds
	held := account.lockSidecarId != nil && account.lockUntil != nil && now.Before(*account.lockUntil)
	if held {
		samePurpose := account.lockPurpose != nil && *account.lockPurpose == args.Purpose
		sameRow := (certId == nil && account.lockCertId == nil) || (certId != nil && account.lockCertId != nil && *account.lockCertId == *certId)
		if *account.lockSidecarId == caller.SidecarId && samePurpose && sameRow {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_tls_account SET lock_until = $3 WHERE network_id = $1 AND domain = $2`,
				account.networkId,
				account.domain,
				now.Add(self.settings.LockLease),
			))
			result := &sidecar.TlsCustomClaimResult{ClaimSeq: account.lockSeq, LeaseMs: leaseMs}
			if certId != nil {
				if r := getTlsCustomCertForUpdateInTx(ctx, tx, account.networkId, account.domain, *certId); r != nil {
					result.Record = r.record()
					if r.orderUrl != nil {
						result.OrderUrl = *r.orderUrl
					}
				}
			}
			return result, nil
		}
		refusal := tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalLocked)
		refusal.Refusal.RetryAfterMs = tlsCustomMs(account.lockUntil.Sub(now))
		return nil, refusal
	}

	// 3: the row
	var r *tlsCustomCertRow
	var grant *tlsCustomGrant
	switch args.Purpose {
	case sidecar.TlsCustomPurposeOrder:
		if account.holdAllUntil != nil && now.Before(*account.holdAllUntil) {
			refusal := tlsCustomRefusal(http.StatusTooManyRequests, sidecar.TlsCustomRefusalHold)
			refusal.Refusal.RetryAfterMs = tlsCustomMs(account.holdAllUntil.Sub(now))
			return nil, refusal
		}
		if f := getTlsCustomOpenOrderInTx(ctx, tx, account.networkId, account.domain); f != nil && *f != *certId {
			refusal := tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalInFlight)
			refusal.Refusal.CertId = sidecar.Id(*f)
			return nil, refusal
		}
		r = getTlsCustomCertForUpdateInTx(ctx, tx, account.networkId, account.domain, *certId)
		if r == nil || (r.state != sidecar.TlsCertStateCustomQueued && r.state != sidecar.TlsCertStateCustomOrdering) {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		if r.orderUrl == nil {
			var reason string
			var wait time.Duration
			grant, reason, wait = self.grantInTx(ctx, tx, account, r, now)
			if grant == nil {
				// a row no one may order holds no one up
				if r.state == sidecar.TlsCertStateCustomOrdering {
					tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
						CertId: sidecar.Id(r.certId),
						From:   []sidecar.TlsCertState{sidecar.TlsCertStateCustomOrdering},
						To:     sidecar.TlsCertStateCustomQueued,
						At:     now,
					})
				}
				refusal := tlsCustomRefusal(http.StatusTooManyRequests, reason)
				refusal.Refusal.RetryAfterMs = tlsCustomMs(wait)
				return nil, refusal
			}
		}
		if r.state == sidecar.TlsCertStateCustomQueued {
			if !tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
				CertId: sidecar.Id(r.certId),
				From:   []sidecar.TlsCertState{sidecar.TlsCertStateCustomQueued},
				To:     sidecar.TlsCertStateCustomOrdering,
				At:     now,
			}) {
				return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
			}
			r.state = sidecar.TlsCertStateCustomOrdering
		}
	case sidecar.TlsCustomPurposeRevoke:
		r = getTlsCustomCertForUpdateInTx(ctx, tx, account.networkId, account.domain, *certId)
		if r == nil || r.state != sidecar.TlsCertStateCustomRevoking {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
	}

	// 4: the take
	lockSeq := account.lockSeq + 1
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_tls_account
			SET lock_seq = $3, lock_sidecar_id = $4, lock_cert_id = $5, lock_purpose = $6, lock_until = $7
			WHERE network_id = $1 AND domain = $2
		`,
		account.networkId,
		account.domain,
		int64(lockSeq),
		caller.SidecarId,
		certId,
		args.Purpose,
		now.Add(self.settings.LockLease),
	))
	result := &sidecar.TlsCustomClaimResult{ClaimSeq: lockSeq, LeaseMs: leaseMs}
	if r != nil {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET claim_seq = $2 WHERE cert_id = $1`, r.certId, int64(lockSeq)))
		r.claimSeq = lockSeq
		result.Record = r.record()
		if r.orderUrl != nil {
			result.OrderUrl = *r.orderUrl
		} else if args.Purpose == sidecar.TlsCustomPurposeOrder {
			result.MayOrder = true
			if grant != nil {
				result.ReplacesLeaf = grant.replacesLeaf
			}
		}
	}
	return result, nil
}

// An admin event of the network about one of its custom domains, at most once each EventRepeatInterval for the
// same result, unless always.
func (self *TlsCustomStore) adminEventInTx(ctx context.Context, tx server.PgTx, networkId server.Id, result string, now time.Time, always bool) bool {
	if !always {
		var recent bool
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1 FROM network_admin_event
					WHERE network_id = $1 AND action = $2 AND result = $3 AND event_time > $4
				)
			`,
			networkId,
			AdminEventActionTlsCustom,
			result,
			now.Add(-self.settings.EventRepeatInterval),
		).Scan(&recent))
		if recent {
			return false
		}
	}
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSystem,
		Action:     AdminEventActionTlsCustom,
		TargetKind: AdminEventTargetTlsCustomDomain,
		TargetId:   &targetId,
		Result:     result,
	})
	return true
}

// Clears the predecessor's link to a successor that ended with no chain, so the predecessor is renewed again
// (17.2.11).
func unlinkTlsCustomPredecessorInTx(ctx context.Context, tx server.PgTx, certId server.Id) {
	server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET successor_cert_id = NULL WHERE successor_cert_id = $1`, certId))
}

// POST /tls/custom-progress (17.2.5): what the lock's holder learned at the CA. An order_invalid answers the claim
// as it stands; every other event answers nil.
func (self *TlsCustomStore) Progress(ctx context.Context, caller *TlsCustomCaller, args *sidecar.TlsCustomProgressArgs) (result *sidecar.TlsCustomClaimResult, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if args == nil || args.Domain == "" {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	if err := tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = self.progressInTx(ctx, tx, caller, args)
		})
	}); err != nil {
		return nil, err
	}
	return
}

func (self *TlsCustomStore) progressInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, args *sidecar.TlsCustomProgressArgs) (*sidecar.TlsCustomClaimResult, error) {
	now := self.nowInTx(ctx, tx)
	account := getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, args.Domain, true)
	if account == nil {
		return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
	}
	if !self.ordererInTx(ctx, tx, caller, args.Domain, false) {
		return nil, tlsCustomRefusal(http.StatusForbidden, sidecar.TlsCustomRefusalOrderer)
	}
	switch args.Event {
	case sidecar.TlsCustomEventAri:
		for _, ari := range args.Ari {
			if ari == nil || !ari.AriWindowStart.Before(ari.AriWindowEnd) {
				continue
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_tls_cert SET ari_window_start = $4, ari_window_end = $5
					WHERE cert_id = $1 AND network_id = $2 AND custom_domain = $3 AND state = 'custom_issued'
				`,
				server.Id(ari.CertId),
				account.networkId,
				account.domain,
				ari.AriWindowStart.UTC(),
				ari.AriWindowEnd.UTC(),
			))
		}
		return nil, nil
	case sidecar.TlsCustomEventRolloverDue:
		self.adminEventInTx(ctx, tx, account.networkId, AdminEventResultTlsAccountRolloverDue, now, false)
		return nil, nil
	}
	certId := server.Id(args.CertId)
	if !account.heldBy(caller, args.ClaimSeq, &certId) {
		return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	r := getTlsCustomCertForUpdateInTx(ctx, tx, account.networkId, account.domain, certId)
	if r == nil {
		return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	free := func() {
		freeTlsCustomLockInTx(ctx, tx, account.networkId, account.domain)
	}
	queue := func(reason string) {
		var customReason *string
		if reason != "" {
			customReason = &reason
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET custom_reason = $2 WHERE cert_id = $1`, r.certId, customReason))
		tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
			CertId: sidecar.Id(r.certId),
			From:   []sidecar.TlsCertState{sidecar.TlsCertStateCustomOrdering},
			To:     sidecar.TlsCertStateCustomQueued,
			At:     now,
		})
	}
	setReason := func(reason string) {
		if reason == "" {
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET custom_reason = $2 WHERE cert_id = $1`, r.certId, reason))
		self.adminEventInTx(ctx, tx, account.networkId, adminEventResultTlsCustomPrefix+reason, now, false)
	}
	switch args.Event {
	case sidecar.TlsCustomEventOrderRecorded:
		if r.state != sidecar.TlsCertStateCustomOrdering || args.OrderUrl == "" {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		if r.orderUrl != nil {
			if *r.orderUrl == args.OrderUrl {
				return nil, nil
			}
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET order_url = $2 WHERE cert_id = $1`, r.certId, args.OrderUrl))
		return nil, nil
	case sidecar.TlsCustomEventOrderInvalid:
		if r.state != sidecar.TlsCertStateCustomOrdering {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		if args.Reason == sidecar.TlsCustomReasonNoRoute {
			self.spendInTx(ctx, tx, account, r, certsservice.CertsCustomSpendNoRoute, now)
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET order_url = NULL WHERE cert_id = $1`, r.certId))
		deleteTlsCustomCertificateSpendInTx(ctx, tx, r.certId)
		r.orderUrl = nil
		grant, reason, wait := self.grantInTx(ctx, tx, account, r, now)
		if grant == nil {
			queue(args.Reason)
			free()
			refusal := tlsCustomRefusal(http.StatusTooManyRequests, reason)
			refusal.Refusal.RetryAfterMs = tlsCustomMs(wait)
			return nil, refusal
		}
		return &sidecar.TlsCustomClaimResult{
			ClaimSeq:     account.lockSeq,
			LeaseMs:      tlsCustomMs(account.lockUntil.Sub(now)),
			Record:       r.record(),
			MayOrder:     true,
			ReplacesLeaf: grant.replacesLeaf,
		}, nil
	case sidecar.TlsCustomEventAuthzFailed:
		self.spendInTx(ctx, tx, account, r, certsservice.CertsCustomSpendAuthzFailed, now)
		setReason(args.Reason)
		return nil, nil
	case sidecar.TlsCustomEventRateLimited:
		hold := min(max(time.Duration(args.RetryAfterMs)*time.Millisecond, self.settings.HoldDefault), self.settings.HoldMax)
		column := "hold_all_until"
		if r.orderUrl == nil && !r.exempt {
			column = "hold_new_until"
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_tls_account SET `+column+` = GREATEST(COALESCE(`+column+`, $3), $3) WHERE network_id = $1 AND domain = $2`,
			account.networkId,
			account.domain,
			now.Add(hold),
		))
		if r.orderUrl == nil {
			if r.exempt {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET exempt = false WHERE cert_id = $1`, r.certId))
			}
			queue("")
			free()
		}
		return nil, nil
	case sidecar.TlsCustomEventNotExempt:
		if r.orderUrl != nil || r.state != sidecar.TlsCertStateCustomOrdering {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET no_replaces = true, exempt = false WHERE cert_id = $1`, r.certId))
		queue("")
		free()
		return nil, nil
	case sidecar.TlsCustomEventRelease:
		if r.state == sidecar.TlsCertStateCustomOrdering && r.orderUrl == nil {
			queue("")
		}
		free()
		return nil, nil
	case sidecar.TlsCustomEventFailed:
		if r.state != sidecar.TlsCertStateCustomOrdering {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		setReason(args.Reason)
		if r.orderUrl == nil {
			tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
				CertId: sidecar.Id(r.certId),
				From:   []sidecar.TlsCertState{sidecar.TlsCertStateCustomOrdering},
				To:     sidecar.TlsCertStateFailed,
				At:     now,
			})
			deleteTlsCustomCertificateSpendInTx(ctx, tx, r.certId)
			unlinkTlsCustomPredecessorInTx(ctx, tx, r.certId)
		} else {
			// a row with an order recorded never simply fails (17.2.4, C10)
			tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
				CertId:           sidecar.Id(r.certId),
				From:             []sidecar.TlsCertState{sidecar.TlsCertStateCustomOrdering},
				To:               sidecar.TlsCertStateCustomRevoking,
				At:               now,
				RevocationReason: sidecar.TlsCertRevocationReasonCessationOfOperation,
			})
		}
		free()
		return nil, nil
	case sidecar.TlsCustomEventRevoked:
		if r.state != sidecar.TlsCertStateCustomRevoking || account.lockPurpose == nil || *account.lockPurpose != sidecar.TlsCustomPurposeRevoke {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
			CertId: sidecar.Id(r.certId),
			From:   []sidecar.TlsCertState{sidecar.TlsCertStateCustomRevoking},
			To:     sidecar.TlsCertStateRevoked,
			At:     now,
		})
		if len(r.chain) == 0 {
			unlinkTlsCustomPredecessorInTx(ctx, tx, r.certId)
			// no certificate came of its order
			deleteTlsCustomCertificateSpendInTx(ctx, tx, r.certId)
		}
		free()
		return nil, nil
	}
	return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
}

// Checks a chain for r (17.2.5's cert-register): 1 to ChainMaxCount certificates, each signed by the next, the
// leaf on the row's key, naming exactly its one host, and unexpired at now.
func (self *TlsCustomStore) checkChain(r *tlsCustomCertRow, chainDer [][]byte, now time.Time) (*x509.Certificate, bool) {
	if len(chainDer) == 0 || self.settings.ChainMaxCount < len(chainDer) {
		return nil, false
	}
	certificates := []*x509.Certificate{}
	for _, der := range chainDer {
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, false
		}
		certificates = append(certificates, certificate)
	}
	for index := 0; index+1 < len(certificates); index += 1 {
		if certificates[index].CheckSignatureFrom(certificates[index+1]) != nil {
			return nil, false
		}
	}
	leaf := certificates[0]
	spkiSha256 := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if !bytes.Equal(spkiSha256[:], r.spkiSha256) {
		return nil, false
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != r.customHost || 0 < len(leaf.IPAddresses) || 0 < len(leaf.EmailAddresses) || 0 < len(leaf.URIs) {
		return nil, false
	}
	if !now.Before(leaf.NotAfter) {
		return nil, false
	}
	return leaf, true
}

// POST /tls/cert-register (17.2.5): the chain of the row the caller holds the lock for.
func (self *TlsCustomStore) Register(ctx context.Context, caller *TlsCustomCaller, args *sidecar.TlsCertRegisterArgs) (returnErr error) {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	if args == nil || args.CertId == (sidecar.Id{}) {
		return tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	if err := tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			returnErr = self.registerInTx(ctx, tx, caller, args)
		})
	}); err != nil {
		return err
	}
	return
}

func (self *TlsCustomStore) registerInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, args *sidecar.TlsCertRegisterArgs) error {
	now := self.nowInTx(ctx, tx)
	certId := server.Id(args.CertId)
	// the row first, for its domain
	rows := getTlsCustomCertsInTx(ctx, tx, `cert_id = $1 AND network_id = $2`, certId, caller.NetworkId)
	if len(rows) == 0 {
		return tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	account := getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, rows[0].customDomain, true)
	if account == nil || !account.heldBy(caller, args.ClaimSeq, &certId) {
		return tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	r := getTlsCustomCertForUpdateInTx(ctx, tx, account.networkId, account.domain, certId)
	if r == nil || r.state != sidecar.TlsCertStateCustomOrdering {
		return tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
	}
	leaf, ok := self.checkChain(r, args.Chain, now)
	if !ok || args.AccountUri != account.accountUri {
		return tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalChain)
	}
	transition := &certsservice.CertsTransition{
		CertId:     sidecar.Id(r.certId),
		From:       []sidecar.TlsCertState{sidecar.TlsCertStateCustomOrdering},
		To:         sidecar.TlsCertStateCustomIssued,
		At:         now,
		Ca:         certsservice.CertsCa(account.ca),
		AccountUri: account.accountUri,
		Serial:     certsservice.CertsSerialText(leaf.SerialNumber),
		NotBefore:  leaf.NotBefore.UTC(),
		NotAfter:   leaf.NotAfter.UTC(),
		Chain:      args.Chain,
	}
	if args.AriWindowStart.Before(args.AriWindowEnd) {
		transition.AriWindowStart = args.AriWindowStart.UTC()
		transition.AriWindowEnd = args.AriWindowEnd.UTC()
	}
	if !tlsCustomMoveInTx(ctx, tx, transition) {
		return tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
	}
	server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET order_url = NULL, custom_reason = NULL WHERE cert_id = $1`, r.certId))
	server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_custom_spend SET at = $2 WHERE cert_id = $1 AND kind = 'certificate'`, r.certId, now))
	freeTlsCustomLockInTx(ctx, tx, account.networkId, account.domain)
	return nil
}

// The account as an ordering desktop sees it at now.
func (self *TlsCustomStore) accountView(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow, now time.Time) *sidecar.TlsCustomAccount {
	view := &sidecar.TlsCustomAccount{
		Domain:            account.domain,
		Ca:                account.ca,
		AccountUri:        account.accountUri,
		KeySeq:            account.keySeq,
		KeyThumbprint:     account.keyThumbprint,
		KeyRepoId:         sidecar.Id(account.keyRepoId),
		NextKeyThumbprint: account.nextKeyThumbprint,
	}
	if account.nextKeyRepoId != nil {
		view.NextKeyRepoId = sidecar.Id(*account.nextKeyRepoId)
	}
	if account.lockSidecarId != nil && account.lockUntil != nil && now.Before(*account.lockUntil) {
		view.LockSidecarId = sidecar.Id(*account.lockSidecarId)
		view.LockRetryAfterMs = tlsCustomMs(account.lockUntil.Sub(now))
	}
	if account.holdNewUntil != nil {
		view.HoldNewMs = tlsCustomMs(account.holdNewUntil.Sub(now))
	}
	if account.holdAllUntil != nil {
		view.HoldAllMs = tlsCustomMs(account.holdAllUntil.Sub(now))
	}
	if acmeToken := getTlsCustomAcmeTokenInTx(ctx, tx, account.networkId, account.domain); acmeToken != "" {
		view.AcmeName = DnsAcmeChallengeTarget(self.roots[0], acmeToken)
	}
	return view
}

// The verified domain's acme_token (QD-175), "" for none.
func getTlsCustomAcmeTokenInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, domain string) string {
	acmeToken := ""
	result, err := query.Query(
		ctx,
		`SELECT acme_token FROM network_domain WHERE network_id = $1 AND domain = $2 AND verify_time IS NOT NULL AND acme_token IS NOT NULL`,
		networkId,
		domain,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&acmeToken))
		}
	})
	return acmeToken
}

// POST /tls/custom-account (17.2.5, 17.2.13): network admins' ordering desktops only.
func (self *TlsCustomStore) AccountOp(ctx context.Context, caller *TlsCustomCaller, args *sidecar.TlsCustomAccountArgs) (view *sidecar.TlsCustomAccount, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if args == nil || args.Domain == "" {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	if err := tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			view, returnErr = self.accountOpInTx(ctx, tx, caller, args)
		})
	}); err != nil {
		return nil, err
	}
	return
}

func (self *TlsCustomStore) accountOpInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, args *sidecar.TlsCustomAccountArgs) (*sidecar.TlsCustomAccount, error) {
	now := self.nowInTx(ctx, tx)
	if !self.ordererInTx(ctx, tx, caller, args.Domain, true) {
		return nil, tlsCustomRefusal(http.StatusForbidden, sidecar.TlsCustomRefusalOrderer)
	}
	if args.Op == sidecar.TlsCustomAccountCreate {
		if args.AccountUri == "" || len(args.KeyThumbprint) != sha256.Size || args.KeyRepoId == (sidecar.Id{}) {
			return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
		}
		var verified bool
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_domain WHERE network_id = $1 AND domain = $2 AND verify_time IS NOT NULL)`,
			caller.NetworkId,
			args.Domain,
		).Scan(&verified))
		if !verified {
			return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_tls_account (network_id, domain, ca, account_uri, key_seq, key_thumbprint, key_repo_id, create_time)
				VALUES ($1, $2, $3, $4, 1, $5, $6, $7)
				ON CONFLICT (network_id, domain) DO NOTHING
			`,
			caller.NetworkId,
			args.Domain,
			certsservice.CertsCustomCaLetsEncrypt,
			args.AccountUri,
			args.KeyThumbprint,
			server.Id(args.KeyRepoId),
			now,
		))
		if tag.RowsAffected() == 0 {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalExists)
		}
		return self.accountView(ctx, tx, getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, args.Domain, false), now), nil
	}
	account := getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, args.Domain, true)
	if account == nil {
		return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
	}
	if !account.heldBy(caller, args.ClaimSeq, nil) || account.lockPurpose == nil || *account.lockPurpose != sidecar.TlsCustomPurposeRollover {
		return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	}
	if args.KeySeq != account.keySeq {
		refusal := tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalKeySeq)
		refusal.Refusal.KeySeq = account.keySeq
		return nil, refusal
	}
	update := func(sql string, values ...any) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_account SET `+sql+` WHERE network_id = $1 AND domain = $2`, append([]any{account.networkId, account.domain}, values...)...))
	}
	switch args.Op {
	case sidecar.TlsCustomAccountRollBegin:
		if account.nextKeyThumbprint != nil || len(args.KeyThumbprint) != sha256.Size || args.KeyRepoId == (sidecar.Id{}) {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		update(`next_key_thumbprint = $3, next_key_repo_id = $4`, args.KeyThumbprint, server.Id(args.KeyRepoId))
	case sidecar.TlsCustomAccountRollEnd:
		if account.nextKeyThumbprint == nil || !bytes.Equal(account.nextKeyThumbprint, args.KeyThumbprint) {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		update(
			`key_seq = $3, key_thumbprint = next_key_thumbprint, key_repo_id = next_key_repo_id, next_key_thumbprint = NULL, next_key_repo_id = NULL,
			lock_sidecar_id = NULL, lock_cert_id = NULL, lock_purpose = NULL, lock_until = NULL`,
			int64(account.keySeq+1),
		)
	case sidecar.TlsCustomAccountRollAbort:
		update(`next_key_thumbprint = NULL, next_key_repo_id = NULL, lock_sidecar_id = NULL, lock_cert_id = NULL, lock_purpose = NULL, lock_until = NULL`)
	case sidecar.TlsCustomAccountRepair:
		if account.nextKeyThumbprint != nil || len(args.KeyThumbprint) != sha256.Size || args.KeyRepoId == (sidecar.Id{}) {
			return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalState)
		}
		update(`key_seq = $3, key_thumbprint = $4, key_repo_id = $5`, int64(account.keySeq+1), args.KeyThumbprint, server.Id(args.KeyRepoId))
	default:
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalRequest)
	}
	return self.accountView(ctx, tx, getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, args.Domain, false), now), nil
}

// GET /tls/certs?work=1 (17.2.5, 17.2.8): the caller's accounts, the domains it is an ordering desktop of, and the
// work list in plan order, each row with its CSR, attestation and wait. The poll stamps the caller in each
// account's orderer_seen when its stamp is older than OrdererSeenStamp.
func (self *TlsCustomStore) Work(ctx context.Context, caller *TlsCustomCaller) (result *sidecar.TlsCertsResult, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	result = &sidecar.TlsCertsResult{Certs: []*sidecar.TlsCertRecord{}}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			now := self.nowInTx(ctx, tx)
			domains := []string{}
			result2, err := tx.Query(ctx, `SELECT domain FROM network_tls_account WHERE network_id = $1 ORDER BY domain`, caller.NetworkId)
			server.WithPgResult(result2, err, func() {
				for result2.Next() {
					var domain string
					server.Raise(result2.Scan(&domain))
					domains = append(domains, domain)
				}
			})
			for _, domain := range domains {
				if !self.ordererInTx(ctx, tx, caller, domain, false) {
					continue
				}
				account := getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, domain, true)
				if account == nil {
					continue
				}
				self.stampOrdererInTx(ctx, tx, account, caller.SidecarId, now)
				result.Accounts = append(result.Accounts, self.accountView(ctx, tx, account, now))
				result.QueuedCsrs = append(result.QueuedCsrs, self.workRowsInTx(ctx, tx, account, now)...)
			}
		})
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return
}

// Stamps sidecarId's poll in orderer_seen, pruning stamps past OrdererSeenWindow.
func (self *TlsCustomStore) stampOrdererInTx(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow, sidecarId server.Id, now time.Time) {
	seen := account.ordererSeen
	if stamp, ok := seen[sidecarId.String()]; ok && now.Sub(stamp) < self.settings.OrdererSeenStamp {
		return
	}
	seen[sidecarId.String()] = now
	for key, stamp := range seen {
		if self.settings.OrdererSeenWindow < now.Sub(stamp) {
			delete(seen, key)
		}
	}
	seenJson, err := json.Marshal(seen)
	server.Raise(err)
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_tls_account SET orderer_seen = $3 WHERE network_id = $1 AND domain = $2`,
		account.networkId,
		account.domain,
		seenJson,
	))
}

// The account's work rows in 17.2.8's plan order, each with its wait at now.
func (self *TlsCustomStore) workRowsInTx(ctx context.Context, tx server.PgTx, account *tlsCustomAccountRow, now time.Time) []*sidecar.TlsCertRecord {
	rows := getTlsCustomCertsInTx(
		ctx,
		tx,
		`network_id = $1 AND custom_domain = $2 AND state IN ($3, $4, $5)`,
		account.networkId,
		account.domain,
		string(sidecar.TlsCertStateCustomQueued),
		string(sidecar.TlsCertStateCustomOrdering),
		string(sidecar.TlsCertStateCustomRevoking),
	)
	hostOwners := tlsCustomDomainOwnersInTx(ctx, tx, account.networkId, account.domain)
	readyHosts := map[string]bool{}
	for _, row := range getDnsCustomHostRowsInTx(ctx, tx, `network_id = $1 AND state = $2 AND ready_time IS NOT NULL`, account.networkId, sidecar.DnsCustomHostStateActive) {
		readyHosts[row.Host] = true
	}
	issuedCounts := map[string]int{}
	countResult, err := tx.Query(
		ctx,
		`SELECT custom_host, count(*) FROM network_tls_cert WHERE network_id = $1 AND custom_domain = $2 AND state = 'custom_issued' AND $3 < not_after GROUP BY custom_host`,
		account.networkId,
		account.domain,
		now,
	)
	server.WithPgResult(countResult, err, func() {
		for countResult.Next() {
			var host string
			var count int
			server.Raise(countResult.Scan(&host, &count))
			issuedCounts[host] = count
		}
	})
	type planned struct {
		rank   int
		key    time.Time
		count  int
		row    *tlsCustomCertRow
		record *sidecar.TlsCertRecord
	}
	plan := []*planned{}
	for _, row := range rows {
		record := row.record()
		record.HostReady = readyHosts[row.customHost]
		record.Owner = hostOwners[row.customHost]
		// oldest first among the revoking rows; the stable sort keeps the cert_id order of equal keys
		entry := &planned{row: row, record: record, key: row.stateTime}
		predecessor := getTlsCustomPredecessorInTx(ctx, tx, row.certId)
		switch {
		case row.state == sidecar.TlsCertStateCustomRevoking:
			entry.rank = 1
		case row.state == sidecar.TlsCertStateCustomOrdering:
			entry.rank = 2
		case predecessor != nil && (predecessor.state == sidecar.TlsCertStateCustomRevoking || predecessor.state == sidecar.TlsCertStateRevoked):
			entry.rank = 3
		case predecessor != nil && predecessor.state == sidecar.TlsCertStateCustomIssued && predecessor.notAfter != nil && now.Before(*predecessor.notAfter):
			entry.rank = 4
			entry.key = *predecessor.notAfter
		default:
			// the host with the fewest certified devices first, then the row's creation, the cert_id order kept
			entry.rank = 5
			entry.count = issuedCounts[row.customHost]
			entry.key = time.Time{}
		}
		// a revocation waits on no hold; a recorded order is resumed, spending nothing
		if row.state != sidecar.TlsCertStateCustomRevoking && row.orderUrl == nil {
			if grant, _, wait := self.grantCheckInTx(ctx, tx, account, row, now); grant == nil {
				record.WaitMs = tlsCustomMs(wait)
			}
		}
		plan = append(plan, entry)
	}
	slices.SortStableFunc(plan, func(a *planned, b *planned) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		if a.count != b.count {
			return a.count - b.count
		}
		return a.key.Compare(b.key)
	})
	records := []*sidecar.TlsCertRecord{}
	for _, entry := range plan {
		records = append(records, entry.record)
	}
	return records
}

// The custom branch of POST /tls/cert-order (17.2.5): a device's CSR for one custom host of its network, queued
// custom_queued with its predecessor linked, in one transaction. newCertId is the row's id.
func (self *TlsCustomStore) QueueOrder(ctx context.Context, caller *TlsCustomCaller, args *sidecar.TlsCertOrderArgs, newCertId server.Id, retryAfter time.Duration) (result *sidecar.TlsCertOrderResult, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if args == nil || caller == nil || caller.SidecarId == (server.Id{}) {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalCsr)
	}
	host, ok := certsservice.CertsCustomCsrHost(args.Csr, self.roots)
	if !ok {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalCsr)
	}
	if err := tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = self.queueOrderInTx(ctx, tx, caller, args, host, newCertId, retryAfter)
		})
	}); err != nil {
		return nil, err
	}
	return
}

func (self *TlsCustomStore) queueOrderInTx(ctx context.Context, tx server.PgTx, caller *TlsCustomCaller, args *sidecar.TlsCertOrderArgs, host string, newCertId server.Id, retryAfter time.Duration) (*sidecar.TlsCertOrderResult, error) {
	now := self.nowInTx(ctx, tx)
	hostRows := getDnsCustomHostRowsInTx(ctx, tx, `host = $1 AND network_id = $2 AND state = $3`, host, caller.NetworkId, sidecar.DnsCustomHostStateActive)
	if len(hostRows) == 0 || hostRows[0].StrictMx() {
		return nil, tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalHost)
	}
	ownerLabel, _, _ := strings.Cut(hostRows[0].Owner, ".")
	if caller.PublishGranted == nil || !caller.PublishGranted(ownerLabel) {
		return nil, tlsCustomRefusal(http.StatusForbidden, sidecar.TlsCustomRefusalGrant)
	}
	checked, err := certsservice.CertsCheckCustomOrder(args.Csr, args.Attestation, host, self.roots, caller.IdentityPublicKey, sidecar.Id(caller.NetworkId), sidecar.Id(caller.SidecarId))
	if err != nil {
		return nil, tlsCustomRefusal(http.StatusBadRequest, sidecar.TlsCustomRefusalCsr)
	}
	domain := getNetworkDnsCustomHostDomainInTx(ctx, tx, caller.NetworkId, host)
	if domain == "" || getTlsCustomAccountInTx(ctx, tx, caller.NetworkId, domain, false) == nil {
		return nil, tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalNoAccount)
	}
	retryAfterMs := tlsCustomMs(retryAfter)
	for _, open := range getTlsCustomCertsForUpdateInTx(
		ctx,
		tx,
		`sidecar_id = $1 AND custom_host = $2 AND state IN ($3, $4)`,
		caller.SidecarId,
		host,
		string(sidecar.TlsCertStateCustomQueued),
		string(sidecar.TlsCertStateCustomOrdering),
	) {
		if bytes.Equal(open.spkiSha256, checked.SpkiSha256[:]) {
			// the same CSR posted again finds its row
			return &sidecar.TlsCertOrderResult{CertId: sidecar.Id(open.certId), State: open.state, RetryAfterMs: retryAfterMs}, nil
		}
		if open.state == sidecar.TlsCertStateCustomOrdering {
			refusal := tlsCustomRefusal(http.StatusConflict, sidecar.TlsCustomRefusalInFlight)
			refusal.Refusal.CertId = sidecar.Id(open.certId)
			return nil, refusal
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET custom_reason = $2 WHERE cert_id = $1`, open.certId, sidecar.TlsCustomReasonSuperseded))
		tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
			CertId: sidecar.Id(open.certId),
			From:   []sidecar.TlsCertState{sidecar.TlsCertStateCustomQueued},
			To:     sidecar.TlsCertStateFailed,
			At:     now,
		})
		unlinkTlsCustomPredecessorInTx(ctx, tx, open.certId)
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_tls_cert (
				cert_id, network_id, client_id, sidecar_id, identifier_set_sha256, spki_sha256, attestation, csr,
				state, state_time, custom_host, custom_domain
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`,
		newCertId,
		caller.NetworkId,
		caller.ClientId,
		caller.SidecarId,
		checked.IdentifierSetSha256[:],
		checked.SpkiSha256[:],
		args.Attestation,
		args.Csr,
		string(sidecar.TlsCertStateCustomQueued),
		now,
		host,
		domain,
	))
	// the newest ended or live row of this device and host with no successor takes the new row as its successor
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_tls_cert SET successor_cert_id = $4
			WHERE cert_id = (
				SELECT cert_id FROM network_tls_cert
				WHERE sidecar_id = $1 AND custom_host = $2 AND successor_cert_id IS NULL AND cert_id <> $4 AND state = ANY($3)
				ORDER BY cert_id DESC
				LIMIT 1
			)
		`,
		caller.SidecarId,
		host,
		[]string{string(sidecar.TlsCertStateCustomIssued), string(sidecar.TlsCertStateCustomRevoking), string(sidecar.TlsCertStateRevoked)},
		newCertId,
	))
	return &sidecar.TlsCertOrderResult{CertId: sidecar.Id(newCertId), State: sidecar.TlsCertStateCustomQueued, RetryAfterMs: retryAfterMs}, nil
}

// What a removal names (17.2.4, 17.2.12 step 1): a device's sidecar, a host, or a domain, of one network.
type TlsCustomRemoval struct {
	NetworkId server.Id
	SidecarId *server.Id
	Host      string
	Domain    string
}

// 17.2.4's removal moves in the caller's transaction: custom_issued rows, and custom_ordering rows with an order
// recorded, go to custom_revoking with cessationOfOperation; custom_queued rows and custom_ordering rows with no
// order go to failed (removed), freeing a lock that names one. How many rows moved.
func TlsCustomRemoveRowsInTx(ctx context.Context, tx server.PgTx, removal *TlsCustomRemoval) int {
	if removal == nil || (removal.SidecarId == nil && removal.Host == "" && removal.Domain == "") {
		return 0
	}
	if !dnsCustomHostLinked() {
		return 0
	}
	where := `network_id = $1 AND state IN ($2, $3, $4)`
	args := []any{
		removal.NetworkId,
		string(sidecar.TlsCertStateCustomQueued),
		string(sidecar.TlsCertStateCustomOrdering),
		string(sidecar.TlsCertStateCustomIssued),
	}
	switch {
	case removal.SidecarId != nil:
		where += ` AND sidecar_id = $5`
		args = append(args, *removal.SidecarId)
	case removal.Host != "":
		where += ` AND custom_host = $5`
		args = append(args, removal.Host)
	default:
		where += ` AND custom_domain = $5`
		args = append(args, removal.Domain)
	}
	now := server.NowUtc()
	movedCount := 0
	for _, row := range getTlsCustomCertsForUpdateInTx(ctx, tx, where, args...) {
		toRevoking := row.state == sidecar.TlsCertStateCustomIssued || (row.state == sidecar.TlsCertStateCustomOrdering && row.orderUrl != nil)
		if toRevoking {
			if tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
				CertId:           sidecar.Id(row.certId),
				From:             []sidecar.TlsCertState{row.state},
				To:               sidecar.TlsCertStateCustomRevoking,
				At:               now,
				RevocationReason: sidecar.TlsCertRevocationReasonCessationOfOperation,
			}) {
				movedCount += 1
			}
		} else {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_tls_cert SET custom_reason = $2 WHERE cert_id = $1`, row.certId, sidecar.TlsCustomReasonRemoved))
			if tlsCustomMoveInTx(ctx, tx, &certsservice.CertsTransition{
				CertId: sidecar.Id(row.certId),
				From:   []sidecar.TlsCertState{row.state},
				To:     sidecar.TlsCertStateFailed,
				At:     now,
			}) {
				movedCount += 1
				deleteTlsCustomCertificateSpendInTx(ctx, tx, row.certId)
				unlinkTlsCustomPredecessorInTx(ctx, tx, row.certId)
			}
		}
		// a removal that ends a row the lock names frees the lock
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_tls_account
				SET lock_sidecar_id = NULL, lock_cert_id = NULL, lock_purpose = NULL, lock_until = NULL
				WHERE network_id = $1 AND lock_cert_id = $2 AND $3
			`,
			removal.NetworkId,
			row.certId,
			!toRevoking,
		))
	}
	return movedCount
}

// What the network's listen facts read of the custom tables (17.2.10): every sidecar's custom_issued rows whose host
// is active, on the owner label the host maps to, with not_after; and every account whose order lock a sidecar
// holds, unlapsed at now, for a row of an active host, with the lock's sequence.
func getTlsCustomListenFactsInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, now time.Time) ([]*socketsservice.TlsListenCustomName, []*socketsservice.TlsListenAcmeClaim) {
	certified := []*socketsservice.TlsListenCustomName{}
	acme := []*socketsservice.TlsListenAcmeClaim{}
	result, err := query.Query(
		ctx,
		`
			SELECT c.sidecar_id, split_part(h.owner, '.', 1), c.custom_host, c.not_after
			FROM network_tls_cert c
			JOIN network_dns_custom_host h ON h.host = c.custom_host AND h.network_id = c.network_id
			WHERE c.network_id = $1 AND c.state = 'custom_issued' AND c.not_after IS NOT NULL AND h.state = 'active'
			ORDER BY c.cert_id
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			name := &socketsservice.TlsListenCustomName{}
			var sidecarId server.Id
			server.Raise(result.Scan(&sidecarId, &name.Owner, &name.Name, &name.NotAfter))
			name.SidecarId = connect.Id(sidecarId)
			certified = append(certified, name)
		}
	})
	result, err = query.Query(
		ctx,
		`
			SELECT a.lock_sidecar_id, split_part(h.owner, '.', 1), c.custom_host, a.lock_seq
			FROM network_tls_account a
			JOIN network_tls_cert c ON c.cert_id = a.lock_cert_id
			JOIN network_dns_custom_host h ON h.host = c.custom_host AND h.network_id = c.network_id
			WHERE a.network_id = $1 AND a.lock_purpose = 'order' AND $2 < a.lock_until AND h.state = 'active'
		`,
		networkId,
		now.UTC(),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			claim := &socketsservice.TlsListenAcmeClaim{}
			var sidecarId server.Id
			var lockSeq int64
			server.Raise(result.Scan(&sidecarId, &claim.Owner, &claim.Name, &lockSeq))
			claim.SidecarId = connect.Id(sidecarId)
			claim.ClaimSeq = uint64(lockSeq)
			acme = append(acme, claim)
		}
	})
	return certified, acme
}

// The name a delegated DNS-01 value for domain is written at, <acme_token>.acme.<root> (QD-175), for a caller
// holding an active publish grant on a host mapped under the domain; 403 grant otherwise.
func (self *TlsCustomStore) ChallengeName(ctx context.Context, caller *TlsCustomCaller, domain string) (name string, returnErr error) {
	if !dnsCustomHostLinked() {
		return "", ErrDnsUnlinked
	}
	if err := tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			if !self.ordererInTx(ctx, tx, caller, domain, false) {
				returnErr = tlsCustomRefusal(http.StatusForbidden, sidecar.TlsCustomRefusalGrant)
				return
			}
			acmeToken := getTlsCustomAcmeTokenInTx(ctx, tx, caller.NetworkId, domain)
			if acmeToken == "" {
				returnErr = tlsCustomRefusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
				return
			}
			name = DnsAcmeChallengeTarget(self.roots[0], acmeToken)
		})
	}); err != nil {
		return "", err
	}
	return
}

var _ certsservice.CertsCustomStore = (*TlsCustomStore)(nil)

// The sweep's renewal candidates (17.2.11): custom_issued rows with no successor whose device's sidecar is active,
// whose host is active and whose domain has its account, and no row of whose sidecar and host failed after
// failedSince.
func (self *TlsCustomStore) ListCustomRenewalCandidates(ctx context.Context, failedSince time.Time, afterCertId sidecar.Id, count int) (records []*sidecar.TlsCertRecord, returnErr error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	returnErr = tlsCustomRun(func() {
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getTlsCustomCertsLimitInTx(
				ctx,
				conn,
				`
					state = 'custom_issued' AND successor_cert_id IS NULL AND cert_id > $1 AND
					EXISTS (SELECT 1 FROM network_dns_custom_host h WHERE h.host = network_tls_cert.custom_host AND h.network_id = network_tls_cert.network_id AND h.state = 'active') AND
					EXISTS (SELECT 1 FROM network_tls_account a WHERE a.network_id = network_tls_cert.network_id AND a.domain = network_tls_cert.custom_domain) AND
					EXISTS (SELECT 1 FROM sidecar_identity s WHERE s.sidecar_id = network_tls_cert.sidecar_id AND s.status = $2) AND
					NOT EXISTS (
						SELECT 1 FROM network_tls_cert f
						WHERE f.sidecar_id = network_tls_cert.sidecar_id AND f.custom_host = network_tls_cert.custom_host AND f.state = 'failed' AND $3 < f.state_time
					)
				`,
				count,
				server.Id(afterCertId),
				SidecarIdentityStatusActive,
				failedSince.UTC(),
			) {
				records = append(records, row.record())
			}
		})
	})
	return
}

// Inserts the successor with the predecessor's CSR, attestation, sidecar, host and domain, and links it.
func (self *TlsCustomStore) QueueCustomSuccessor(ctx context.Context, predecessorCertId sidecar.Id, successorCertId sidecar.Id, now time.Time) (queued bool, returnErr error) {
	if !dnsCustomHostLinked() {
		return false, ErrDnsUnlinked
	}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			rows := getTlsCustomCertsForUpdateInTx(ctx, tx, `cert_id = $1 AND state = 'custom_issued' AND successor_cert_id IS NULL`, server.Id(predecessorCertId))
			if len(rows) == 0 {
				return
			}
			predecessor := rows[0]
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_tls_cert (
						cert_id, network_id, client_id, sidecar_id, identifier_set_sha256, spki_sha256, attestation, csr,
						state, state_time, custom_host, custom_domain
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
					ON CONFLICT DO NOTHING
				`,
				server.Id(successorCertId),
				predecessor.networkId,
				predecessor.clientId,
				predecessor.sidecarId,
				predecessor.identifierSetSha256,
				predecessor.spkiSha256,
				predecessor.attestation,
				predecessor.csr,
				string(sidecar.TlsCertStateCustomQueued),
				now.UTC(),
				predecessor.customHost,
				predecessor.customDomain,
			))
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_tls_cert SET successor_cert_id = $2 WHERE cert_id = $1 AND successor_cert_id IS NULL`,
				predecessor.certId,
				server.Id(successorCertId),
			))
			queued = tag.RowsAffected() == 1
		})
	})
	return
}

// custom_issued and custom_revoking rows past not_after go to expired: nothing is left to revoke on one.
func (self *TlsCustomStore) ExpireCustomRows(ctx context.Context, now time.Time) (expiredCount int, returnErr error) {
	if !dnsCustomHostLinked() {
		return 0, ErrDnsUnlinked
	}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_tls_cert SET state = 'expired', state_time = $1, revocation_reason = NULL
					WHERE state IN ('custom_issued', 'custom_revoking') AND not_after IS NOT NULL AND not_after <= $1
				`,
				now.UTC(),
			))
			expiredCount = int(tag.RowsAffected())
		})
	})
	return
}

// Spends older than before go. The sweep passes its now less CertsCustomSettings.SpendRetention (cert-1: 16 d), where
// every budget's replay starts reading, so the grant never misses a spend it reads, and a host's busy stretch of up to
// 214 h keeps the quiet window before it, which paces its tenth device (QD-994, QD-996).
func (self *TlsCustomStore) PurgeCustomSpends(ctx context.Context, before time.Time) (purgedCount int, returnErr error) {
	if !dnsCustomHostLinked() {
		return 0, ErrDnsUnlinked
	}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_tls_custom_spend WHERE at < $1`, before.UTC()))
			purgedCount = int(tag.RowsAffected())
		})
	})
	return
}

// 17.2.14's warnings the sweep raises, each at most once a network and kind each EventRepeatInterval:
// tls_custom_renewal_due and tls_custom_renewal_late for custom_issued rows with no issued successor past
// LapseWarnFraction and RenewalLifetimeFraction of their life, tls_custom_one_orderer for an account with live rows
// and fewer than OrdererMinCount desktops seen in OrdererSeenWindow, and tls_custom_queue_stalled for a custom_queued
// row unclaimed for QueueStallAfter on an account with no lock and no hold.
func (self *TlsCustomStore) RaiseCustomWarnings(ctx context.Context, now time.Time, settings *certsservice.CertsCustomSettings) (raisedCount int, returnErr error) {
	if !dnsCustomHostLinked() {
		return 0, ErrDnsUnlinked
	}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			warnings := map[server.Id][]string{}
			add := func(networkId server.Id, result string) {
				if !slices.Contains(warnings[networkId], result) {
					warnings[networkId] = append(warnings[networkId], result)
				}
			}
			lifeResult, err := tx.Query(
				ctx,
				`
					SELECT c.network_id, (EXTRACT(EPOCH FROM ($1 - c.not_before)) / NULLIF(EXTRACT(EPOCH FROM (c.not_after - c.not_before)), 0))::float8
					FROM network_tls_cert c
					WHERE c.state = 'custom_issued' AND c.not_before IS NOT NULL AND c.not_after > $1 AND NOT EXISTS (
						SELECT 1 FROM network_tls_cert s WHERE s.cert_id = c.successor_cert_id AND s.state = 'custom_issued'
					)
				`,
				now.UTC(),
			)
			server.WithPgResult(lifeResult, err, func() {
				for lifeResult.Next() {
					var networkId server.Id
					var lived *float64
					server.Raise(lifeResult.Scan(&networkId, &lived))
					switch {
					case lived == nil:
					case settings.RenewalLifetimeFraction <= *lived:
						add(networkId, AdminEventResultTlsCustomRenewalLate)
					case settings.LapseWarnFraction <= *lived:
						add(networkId, AdminEventResultTlsCustomRenewalDue)
					}
				}
			})
			accountResult, err := tx.Query(
				ctx,
				`
					SELECT a.network_id, a.orderer_seen, a.lock_until, a.hold_new_until, a.hold_all_until,
						EXISTS (SELECT 1 FROM network_tls_cert c WHERE c.network_id = a.network_id AND c.custom_domain = a.domain AND c.state IN ('custom_queued', 'custom_ordering', 'custom_issued', 'custom_revoking')),
						EXISTS (SELECT 1 FROM network_tls_cert c WHERE c.network_id = a.network_id AND c.custom_domain = a.domain AND c.state = 'custom_queued' AND c.state_time < $2)
					FROM network_tls_account a
				`,
				now.UTC(),
				now.Add(-settings.QueueStallAfter).UTC(),
			)
			server.WithPgResult(accountResult, err, func() {
				for accountResult.Next() {
					var networkId server.Id
					var seenJson []byte
					var lockUntil, holdNewUntil, holdAllUntil *time.Time
					var live, stalled bool
					server.Raise(accountResult.Scan(&networkId, &seenJson, &lockUntil, &holdNewUntil, &holdAllUntil, &live, &stalled))
					seen := map[string]time.Time{}
					json.Unmarshal(seenJson, &seen)
					recent := 0
					for _, stamp := range seen {
						if now.Sub(stamp) <= settings.OrdererSeenWindow {
							recent += 1
						}
					}
					if live && recent < settings.OrdererMinCount {
						add(networkId, AdminEventResultTlsCustomOneOrderer)
					}
					free := (lockUntil == nil || !now.Before(*lockUntil)) && (holdNewUntil == nil || !now.Before(*holdNewUntil)) && (holdAllUntil == nil || !now.Before(*holdAllUntil))
					if stalled && free {
						add(networkId, AdminEventResultTlsCustomQueueStalled)
					}
				}
			})
			for networkId, results := range warnings {
				for _, result := range results {
					if self.adminEventInTx(ctx, tx, networkId, result, now, false) {
						raisedCount += 1
					}
				}
			}
		})
	})
	return
}

// 17.2.4's removal moves for the rows of devices that are gone (I-15): no active client among the row's client and
// every client its sidecar has bound, or a revoked sidecar identity, as certs' revocation sweep reads a device's
// removal for its own rows.
func (self *TlsCustomStore) MoveRemovedDevices(ctx context.Context, now time.Time) (movedCount int, returnErr error) {
	if !dnsCustomHostLinked() {
		return 0, ErrDnsUnlinked
	}
	returnErr = tlsCustomRun(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			type gone struct {
				networkId server.Id
				sidecarId server.Id
			}
			goneDevices := []gone{}
			result, err := tx.Query(
				ctx,
				`
					SELECT DISTINCT c.network_id, c.sidecar_id
					FROM network_tls_cert c
					WHERE
						c.custom_host IS NOT NULL AND
						c.state IN ('custom_queued', 'custom_ordering', 'custom_issued') AND
						(
							NOT EXISTS (
								SELECT 1 FROM network_client
								WHERE
									network_client.active = true AND
									(
										network_client.client_id = c.client_id OR
										network_client.client_id IN (
											SELECT b.client_id FROM sidecar_identity_binding b WHERE b.sidecar_id = c.sidecar_id
										)
									)
							) OR
							EXISTS (SELECT 1 FROM sidecar_identity s WHERE s.sidecar_id = c.sidecar_id AND s.status = $1)
						)
				`,
				SidecarIdentityStatusRevoked,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					device := gone{}
					server.Raise(result.Scan(&device.networkId, &device.sidecarId))
					goneDevices = append(goneDevices, device)
				}
			})
			for _, device := range goneDevices {
				sidecarId := device.sidecarId
				movedCount += TlsCustomRemoveRowsInTx(ctx, tx, &TlsCustomRemoval{NetworkId: device.networkId, SidecarId: &sidecarId})
			}
		})
	})
	return
}
