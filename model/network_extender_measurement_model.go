package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The operator's records of lag.dev measurement certificates
// (lag.dev/PLAN.md C5-C10, A8-A10).
//
//   - network_extender_measurement is one extender's latest certificate
//     request: the CSR an activation last carried, the names it asks for, and
//     where the request stands. pending waits for the issuance task (C5);
//     issued is answered by a live certificate for the same names and key;
//     refused says why not; withdrawn is a request whose certificate was
//     withdrawn (C9), which stands until the extender asks again. A request
//     is known by its names and its key, never by its bytes. An extender has
//     one measurement name, its id under the zone (N1), which the controller
//     checks; these tables keep a list and do not depend on its shape.
//   - network_extender_measurement_cert is one certificate the CA issued. It
//     is live until it expires or is withdrawn (C9), and a renewal (C6) marks
//     the certificate it replaces as renewed without withdrawing it, since
//     the replaced one is still valid for the same names until it expires.
//     publish_time is when the dns reconcile found its names published, in
//     sync on every authoritative server, and is cleared when they stop being
//     published (C8). A name is published while any live certificate for it
//     says so, which carries a name across a renewal; the lag.dev sample
//     offers only published names (A4). deliver_time is when A8 first
//     answered the extender with the certificate's chain, which the extender
//     installs only when it reads it there: up to half an hour after its
//     name is published. The lag.dev sample (A4) and the records'
//     MeasurementId (C10) wait for a delivered certificate for the key of
//     the extender's latest request.
//   - network_extender.measurement_ever_issued is set before an order is
//     placed and never cleared, and measurement_exposure_accepted is an
//     operator's override (A9). A check constraint keeps every writer from
//     leaving an extender that ever held a certificate in the gated tier or a
//     canary channel without the override, so the refusal holds for writers
//     that do not know about measurement at all.
//
// Nothing here talks to the CA or to dns, and nothing here holds a key: the
// taskworker orders and revokes (C5, C9) and reconciles the zone (C8), the
// controller parses requests. Names are stored as one comma-separated value,
// sorted, so two requests for the same names compare equal in SQL.
//
// Timestamps are naive columns holding utc, as everywhere else in this schema.

// The states of a request, which are A8's answers, and A8's answer for an
// extender with none.
const (
	NetworkExtenderMeasurementStateNone      = "none"
	NetworkExtenderMeasurementStatePending   = "pending"
	NetworkExtenderMeasurementStateIssued    = "issued"
	NetworkExtenderMeasurementStateRefused   = "refused"
	NetworkExtenderMeasurementStateWithdrawn = "withdrawn"
)

// ErrNetworkExtenderMeasurementExposure refuses moving an extender that ever
// held a measurement certificate into the gated tier or a canary channel
// without the override (lag.dev/PLAN.md §2, A9). The certificate logs list
// its measurement name for good, and the name resolved to its address while
// it served, so its address is no secret a gated tier or a canary could rely
// on.
var ErrNetworkExtenderMeasurementExposure = errors.New(
	"the extender held a measurement certificate, so its address was public while its measurement name resolved, and it cannot be gated or a canary unless that exposure is accepted",
)

// One extender's latest certificate request (A10).
type NetworkExtenderMeasurement struct {
	ExtenderId server.Id
	// the CSR, DER; nil for a request refused as unreadable, and after an
	// owner's opt-out
	Csr []byte
	// the CSR's SubjectPublicKeyInfo, DER, which a certificate is matched to
	// and A8 names (key_sha256); nil only for a request refused as unreadable
	PublicKey []byte
	// sorted
	Names       []string
	State       string
	Reason      string
	RequestTime time.Time
	// when the extender began waiting, kept across requests while it waits:
	// the order the weekly cap admits in (C7)
	PendingTime  *time.Time
	RetryTime    *time.Time
	FailureCount int
	UpdateTime   time.Time
}

// One certificate the CA issued (C5).
type NetworkExtenderMeasurementCert struct {
	CertId     server.Id
	ExtenderId server.Id
	// sorted
	Names []string
	// the CSR it was issued for, DER, which a renewal reuses (C6)
	Csr       []byte
	PublicKey []byte
	// the leaf first, then the issuers
	ChainPem  string
	SerialHex string
	NotBefore time.Time
	NotAfter  time.Time
	IssueTime time.Time
	// whether it counts against the weekly cap (C7)
	CapCounted bool
	// two thirds of the way through its lifetime, or later after a failed
	// renewal (C6)
	RenewTime         time.Time
	RenewFailureCount int
	RenewedTime       *time.Time
	WithdrawTime      *time.Time
	WithdrawReason    string
	RevokeTime        *time.Time
	// failed revocations so far, which space the next attempt (C9)
	RevokeFailureCount int
	// when the dns reconcile found its names published, in sync on every
	// authoritative server; nil while they are not (C8, A4)
	PublishTime *time.Time
	// when A8 first answered the extender with this certificate's chain; nil
	// until then. The extender installs a chain only when it reads it there
	// (A8, A4, C10)
	DeliverTime *time.Time
}

// A request or a certificate with the extender it belongs to, which is what
// eligibility is decided on (C5, C8, C9): the extender row and its active
// addresses. Extender is nil for an extender that is gone.
type NetworkExtenderMeasurementTarget struct {
	Measurement *NetworkExtenderMeasurement
	Cert        *NetworkExtenderMeasurementCert
	Extender    *NetworkExtender
	Addresses   []*NetworkExtenderAddress
}

// The column form of a name list: lower case, deduplicated, sorted, joined.
func joinNetworkExtenderMeasurementNames(names []string) string {
	kept := []string{}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || slices.Contains(kept, name) {
			continue
		}
		kept = append(kept, name)
	}
	slices.Sort(kept)
	return strings.Join(kept, ",")
}

// The inverse, tolerant of the empty column.
func splitNetworkExtenderMeasurementNames(names string) []string {
	kept := []string{}
	for _, name := range strings.Split(names, ",") {
		if name = strings.TrimSpace(name); name != "" {
			kept = append(kept, name)
		}
	}
	return kept
}

const networkExtenderMeasurementColumns = `
	extender_id,
	csr,
	public_key,
	names,
	state,
	reason,
	request_time,
	pending_time,
	retry_time,
	failure_count,
	update_time
`

// Scans a row that starts with networkExtenderMeasurementColumns, and its
// columns after those into `extra`.
func scanNetworkExtenderMeasurement(result server.PgResult, extra ...any) *NetworkExtenderMeasurement {
	measurement := &NetworkExtenderMeasurement{}
	var names string
	server.Raise(result.Scan(append(
		[]any{
			&measurement.ExtenderId,
			&measurement.Csr,
			&measurement.PublicKey,
			&names,
			&measurement.State,
			&measurement.Reason,
			&measurement.RequestTime,
			&measurement.PendingTime,
			&measurement.RetryTime,
			&measurement.FailureCount,
			&measurement.UpdateTime,
		},
		extra...,
	)...))
	measurement.Names = splitNetworkExtenderMeasurementNames(names)
	return measurement
}

const networkExtenderMeasurementCertColumns = `
	cert_id,
	extender_id,
	names,
	csr,
	public_key,
	chain_pem,
	serial_hex,
	not_before,
	not_after,
	issue_time,
	cap_counted,
	renew_time,
	renew_failure_count,
	renewed_time,
	withdraw_time,
	withdraw_reason,
	revoke_time,
	revoke_failure_count,
	publish_time,
	deliver_time
`

func scanNetworkExtenderMeasurementCert(result server.PgResult) *NetworkExtenderMeasurementCert {
	cert := &NetworkExtenderMeasurementCert{}
	var names string
	server.Raise(result.Scan(
		&cert.CertId,
		&cert.ExtenderId,
		&names,
		&cert.Csr,
		&cert.PublicKey,
		&cert.ChainPem,
		&cert.SerialHex,
		&cert.NotBefore,
		&cert.NotAfter,
		&cert.IssueTime,
		&cert.CapCounted,
		&cert.RenewTime,
		&cert.RenewFailureCount,
		&cert.RenewedTime,
		&cert.WithdrawTime,
		&cert.WithdrawReason,
		&cert.RevokeTime,
		&cert.RevokeFailureCount,
		&cert.PublishTime,
		&cert.DeliverTime,
	))
	cert.Names = splitNetworkExtenderMeasurementNames(names)
	return cert
}

// SetNetworkExtenderMeasurementRequest stores the certificate request an
// activation carried (A10) and returns the state it leaves.
//
// A request is known by its names and its key, not by its bytes: an extender
// signs its request anew when it restarts, and most activations carry none at
// all (C4). So a request for the names and key the operator already holds --
// pending, or issued with a live certificate -- changes nothing: there is no
// new order, and a pending one keeps its place in the queue and the backoff
// of its failed orders (A8). A request a live certificate already answers is
// issued at once, and the certificate is renewed in its time (C6).
//
// Anything else is pending: other names or another key, or the same request
// again after it was refused or its certificate was withdrawn (C9), which is
// how an extender that comes back is given a certificate again. An extender
// that was already waiting keeps its place in the queue (C7), and the same
// names and key keep the backoff of their failed orders, so asking again
// cannot hurry the CA.
func SetNetworkExtenderMeasurementRequest(
	ctx context.Context,
	extenderId server.Id,
	csr []byte,
	publicKey []byte,
	names []string,
	requestTime time.Time,
) (state string) {
	namesValue := joinNetworkExtenderMeasurementNames(names)
	server.Tx(ctx, func(tx server.PgTx) {
		state = NetworkExtenderMeasurementStatePending

		// the request held now, locked so that two activations of one
		// extender judge it one after the other
		heldState := ""
		heldSame := false
		result, err := tx.Query(
			ctx,
			`
			SELECT
				state,
				COALESCE(names = $2 AND public_key = $3, false)
			FROM network_extender_measurement
			WHERE extender_id = $1
			FOR UPDATE
			`,
			extenderId,
			namesValue,
			publicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&heldState, &heldSame))
			}
		})

		live := false
		result, err = tx.Query(
			ctx,
			`
			SELECT 1
			FROM network_extender_measurement_cert
			WHERE
				extender_id = $1 AND
				names = $2 AND
				public_key = $3 AND
				withdraw_time IS NULL AND
				$4 < not_after
			LIMIT 1
			`,
			extenderId,
			namesValue,
			publicKey,
			requestTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			live = result.Next()
		})

		switch {
		case heldSame && heldState == NetworkExtenderMeasurementStatePending:
			// already waiting for exactly this
			return
		case heldSame && heldState == NetworkExtenderMeasurementStateIssued && live:
			state = NetworkExtenderMeasurementStateIssued
			return
		}

		if live {
			state = NetworkExtenderMeasurementStateIssued
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_extender_measurement (
					extender_id,
					csr,
					public_key,
					names,
					state,
					reason,
					request_time,
					pending_time,
					retry_time,
					failure_count,
					update_time
				)
				VALUES ($1, $2, $3, $4, $5, '', $6, NULL, NULL, 0, $6)
				ON CONFLICT (extender_id) DO UPDATE
				SET
					csr = $2,
					public_key = $3,
					names = $4,
					state = $5,
					reason = '',
					request_time = $6,
					pending_time = NULL,
					retry_time = NULL,
					failure_count = 0,
					update_time = $6
				`,
				extenderId,
				csr,
				publicKey,
				namesValue,
				NetworkExtenderMeasurementStateIssued,
				requestTime.UTC(),
			))
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender_measurement (
				extender_id,
				csr,
				public_key,
				names,
				state,
				reason,
				request_time,
				pending_time,
				retry_time,
				failure_count,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, '', $6, $6, NULL, 0, $6)
			ON CONFLICT (extender_id) DO UPDATE
			SET
				csr = $2,
				public_key = $3,
				names = $4,
				state = $5,
				reason = '',
				request_time = $6,
				pending_time = CASE
					WHEN network_extender_measurement.state = $5
					THEN COALESCE(network_extender_measurement.pending_time, $6)
					ELSE $6
				END,
				retry_time = CASE
					WHEN
						network_extender_measurement.names = $4 AND
						network_extender_measurement.public_key = $3
					THEN network_extender_measurement.retry_time
					ELSE NULL
				END,
				failure_count = CASE
					WHEN
						network_extender_measurement.names = $4 AND
						network_extender_measurement.public_key = $3
					THEN network_extender_measurement.failure_count
					ELSE 0
				END,
				update_time = $6
			`,
			extenderId,
			csr,
			publicKey,
			namesValue,
			NetworkExtenderMeasurementStatePending,
			requestTime.UTC(),
		))
	})
	return
}

// RefuseNetworkExtenderMeasurementRequest stores a request an activation
// carried that is refused as it arrives (A10): one that is not a valid request
// for the extender's own name, so the extender learns why from A8 rather than
// waiting. What could be read of it is kept -- its DER, its key and the names
// it asks for, each nil for a request that could not be read that far -- so
// A8 names the key it answers for (key_sha256), and the extender knows the
// refusal is of its own request rather than taking it for nothing held and
// asking again at once (C4). A live certificate is not touched: it stays
// until it is withdrawn or expires.
func RefuseNetworkExtenderMeasurementRequest(
	ctx context.Context,
	extenderId server.Id,
	csr []byte,
	publicKey []byte,
	names []string,
	reason string,
	requestTime time.Time,
) {
	if len(csr) == 0 {
		csr = nil
	}
	if len(publicKey) == 0 {
		publicKey = nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender_measurement (
				extender_id,
				csr,
				public_key,
				names,
				state,
				reason,
				request_time,
				pending_time,
				retry_time,
				failure_count,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, NULL, 0, $7)
			ON CONFLICT (extender_id) DO UPDATE
			SET
				csr = $2,
				public_key = $3,
				names = $4,
				state = $5,
				reason = $6,
				request_time = $7,
				pending_time = NULL,
				retry_time = NULL,
				failure_count = 0,
				update_time = $7
			`,
			extenderId,
			csr,
			publicKey,
			joinNetworkExtenderMeasurementNames(names),
			NetworkExtenderMeasurementStateRefused,
			reason,
			requestTime.UTC(),
		))
	})
}

// GetPendingNetworkExtenderMeasurements reads the issuance queue (C5, C7):
// pending requests whose backoff has passed, each with its extender. First
// come the requests the weekly cap does not hold, those for names with an
// unexpired certificate (HasUnexpiredNetworkExtenderMeasurementCertForNames),
// which the CA renews outside its weekly budget: a returning extender, a
// rotated key, an owner turning measurement back on. Then come the rest. Each
// part is longest waiting first. A pass notes a capped request and leaves it
// pending, so while the cap is spent the same capped requests come back every
// pass; without this order, `limit` of them waiting longer would keep every
// request the cap does not hold from ever being read.
func GetPendingNetworkExtenderMeasurements(
	ctx context.Context,
	now time.Time,
	limit int,
) []*NetworkExtenderMeasurementTarget {
	targets := []*NetworkExtenderMeasurementTarget{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementColumns+`
			FROM network_extender_measurement
			WHERE
				state = $1 AND
				(retry_time IS NULL OR retry_time <= $2)
			ORDER BY
				EXISTS (
					SELECT 1
					FROM network_extender_measurement_cert
					WHERE
						network_extender_measurement_cert.names = network_extender_measurement.names AND
						$2 < network_extender_measurement_cert.not_after
				) DESC,
				pending_time ASC,
				extender_id ASC
			LIMIT $3
			`,
			NetworkExtenderMeasurementStatePending,
			now.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				targets = append(targets, &NetworkExtenderMeasurementTarget{
					Measurement: scanNetworkExtenderMeasurement(result),
				})
			}
		})
		fillNetworkExtenderMeasurementTargetsInConn(ctx, conn, targets)
	})
	return targets
}

// Loads the extender and the active addresses of every target, on the
// caller's connection.
func fillNetworkExtenderMeasurementTargetsInConn(
	ctx context.Context,
	conn server.PgConn,
	targets []*NetworkExtenderMeasurementTarget,
) {
	extenderIds := []server.Id{}
	for _, target := range targets {
		extenderId := target.extenderId()
		if !slices.Contains(extenderIds, extenderId) {
			extenderIds = append(extenderIds, extenderId)
		}
	}
	if len(extenderIds) == 0 {
		return
	}

	extenders := map[server.Id]*NetworkExtender{}
	result, err := conn.Query(
		ctx,
		`
		SELECT
			extender_id,
			network_id,
			client_id,
			public_key,
			create_time,
			tcp_port,
			udp_port,
			dns_port,
			dns_tld,
			country_code,
			active,
			revoke_time,
			record_issue_time,
			directory_tier,
			COALESCE(canary_channel, '')
		FROM network_extender
		WHERE extender_id = ANY($1)
		`,
		extenderIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			extender := &NetworkExtender{}
			server.Raise(result.Scan(
				&extender.ExtenderId,
				&extender.NetworkId,
				&extender.ClientId,
				&extender.PublicKey,
				&extender.CreateTime,
				&extender.TcpPort,
				&extender.UdpPort,
				&extender.DnsPort,
				&extender.DnsTld,
				&extender.CountryCode,
				&extender.Active,
				&extender.RevokeTime,
				&extender.RecordIssueTime,
				&extender.DirectoryTier,
				&extender.CanaryChannel,
			))
			extenders[extender.ExtenderId] = extender
		}
	})

	addresses := map[server.Id][]*NetworkExtenderAddress{}
	result, err = conn.Query(
		ctx,
		`
		SELECT
			extender_id,
			ip_version,
			ip,
			carriers,
			dns_ports,
			activate_time
		FROM network_extender_address
		WHERE extender_id = ANY($1) AND active
		ORDER BY extender_id, ip_version
		`,
		extenderIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			address := &NetworkExtenderAddress{Active: true}
			var carriers string
			var dnsPorts string
			server.Raise(result.Scan(
				&address.ExtenderId,
				&address.IpVersion,
				&address.Ip,
				&carriers,
				&dnsPorts,
				&address.ActivateTime,
			))
			address.Carriers = splitExtenderCarriers(carriers)
			address.DnsPorts = splitExtenderDnsPorts(dnsPorts)
			addresses[address.ExtenderId] = append(addresses[address.ExtenderId], address)
		}
	})

	for _, target := range targets {
		extenderId := target.extenderId()
		target.Extender = extenders[extenderId]
		target.Addresses = addresses[extenderId]
		if target.Addresses == nil {
			target.Addresses = []*NetworkExtenderAddress{}
		}
	}
}

func (self *NetworkExtenderMeasurementTarget) extenderId() server.Id {
	if self.Cert != nil {
		return self.Cert.ExtenderId
	}
	return self.Measurement.ExtenderId
}

// RefusePendingNetworkExtenderMeasurement refuses a pending request, the one
// that was read: a request that a newer activation replaced meanwhile is not
// the one that was judged, and is left pending. False when it was replaced.
func RefusePendingNetworkExtenderMeasurement(
	ctx context.Context,
	extenderId server.Id,
	csr []byte,
	reason string,
	now time.Time,
) (refused bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET
				state = $3,
				reason = $4,
				pending_time = NULL,
				retry_time = NULL,
				update_time = $5
			WHERE extender_id = $1 AND csr = $2 AND state = $6
			`,
			extenderId,
			csr,
			NetworkExtenderMeasurementStateRefused,
			reason,
			now.UTC(),
			NetworkExtenderMeasurementStatePending,
		))
		refused = tag.RowsAffected() == 1
	})
	return
}

// NoteNetworkExtenderMeasurementWait records why a pending request waits
// without counting a failure: the weekly cap, or the other family's
// activation still to come. A8 shows the reason.
func NoteNetworkExtenderMeasurementWait(
	ctx context.Context,
	extenderId server.Id,
	csr []byte,
	reason string,
	now time.Time,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET reason = $3, update_time = $4
			WHERE extender_id = $1 AND csr = $2 AND state = $5 AND reason != $3
			`,
			extenderId,
			csr,
			reason,
			now.UTC(),
			NetworkExtenderMeasurementStatePending,
		))
	})
}

// DeferPendingNetworkExtenderMeasurement counts a failed order of a pending
// request and puts its next attempt at retryTime. False when a newer request
// replaced it meanwhile, which starts without the failure.
func DeferPendingNetworkExtenderMeasurement(
	ctx context.Context,
	extenderId server.Id,
	csr []byte,
	reason string,
	retryTime time.Time,
	now time.Time,
) (deferred bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET
				reason = $3,
				retry_time = $4,
				failure_count = failure_count + 1,
				update_time = $5
			WHERE extender_id = $1 AND csr = $2 AND state = $6
			`,
			extenderId,
			csr,
			reason,
			retryTime.UTC(),
			now.UTC(),
			NetworkExtenderMeasurementStatePending,
		))
		deferred = tag.RowsAffected() == 1
	})
	return
}

// FlagNetworkExtenderMeasurementEverIssued marks the extender as having held a
// measurement certificate, before the order that issues one is placed (C5).
// The CA logs the certificate to certificate transparency during issuance, so
// a process that dies after ordering must already have left the flag.
//
// The flag is set only on an active open tier extender that is no canary, in
// the same statement that checks it, and false is returned otherwise: the
// extender moved out of the open tier since it was judged, and no order may be
// placed for it. Setting the flag also clears an earlier override (A9), which
// accepted the exposure of the certificates before this one, not of this one.
func FlagNetworkExtenderMeasurementEverIssued(ctx context.Context, extenderId server.Id) (flagged bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET
				measurement_ever_issued = true,
				measurement_exposure_accepted = false
			WHERE
				extender_id = $1 AND
				active AND
				directory_tier = $2 AND
				canary_channel IS NULL
			`,
			extenderId,
			connect.ExtenderDirectoryTierOpen,
		))
		flagged = tag.RowsAffected() == 1
	})
	return
}

// AddNetworkExtenderMeasurementCert stores a certificate the CA issued (C5,
// C6), and reports whether it is live. Every other live certificate of the
// extender is marked renewed, since this one replaces it, and a request for
// exactly these names and this key is issued.
//
// An order takes a minute or more, and the extender may be withdrawn while
// it runs -- deactivated, overridden, or opted out by its owner (C9). So the
// extender's request is read under a lock first, and a certificate that
// arrives for a withdrawn request is stored already withdrawn, with the
// request's reason: it is revoked like any other withdrawn certificate and
// never published or signed, and the request stays withdrawn.
func AddNetworkExtenderMeasurementCert(ctx context.Context, cert *NetworkExtenderMeasurementCert) (live bool) {
	namesValue := joinNetworkExtenderMeasurementNames(cert.Names)
	server.Tx(ctx, func(tx server.PgTx) {
		live = true

		requestState := ""
		requestReason := ""
		result, err := tx.Query(
			ctx,
			`
			SELECT state, reason
			FROM network_extender_measurement
			WHERE extender_id = $1
			FOR UPDATE
			`,
			cert.ExtenderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&requestState, &requestReason))
			}
		})
		var withdrawTime *time.Time
		withdrawReason := ""
		if requestState == NetworkExtenderMeasurementStateWithdrawn {
			live = false
			issueTime := cert.IssueTime.UTC()
			withdrawTime = &issueTime
			withdrawReason = requestReason
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender_measurement_cert (
				cert_id,
				extender_id,
				names,
				csr,
				public_key,
				chain_pem,
				serial_hex,
				not_before,
				not_after,
				issue_time,
				cap_counted,
				renew_time,
				withdraw_time,
				withdraw_reason
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			`,
			cert.CertId,
			cert.ExtenderId,
			namesValue,
			cert.Csr,
			cert.PublicKey,
			cert.ChainPem,
			cert.SerialHex,
			cert.NotBefore.UTC(),
			cert.NotAfter.UTC(),
			cert.IssueTime.UTC(),
			cert.CapCounted,
			cert.RenewTime.UTC(),
			withdrawTime,
			withdrawReason,
		))
		if !live {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET renewed_time = $3
			WHERE
				extender_id = $1 AND
				cert_id != $2 AND
				withdraw_time IS NULL AND
				renewed_time IS NULL
			`,
			cert.ExtenderId,
			cert.CertId,
			cert.IssueTime.UTC(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET
				state = $4,
				reason = '',
				pending_time = NULL,
				retry_time = NULL,
				failure_count = 0,
				update_time = $5
			WHERE extender_id = $1 AND names = $2 AND public_key = $3
			`,
			cert.ExtenderId,
			namesValue,
			cert.PublicKey,
			NetworkExtenderMeasurementStateIssued,
			cert.IssueTime.UTC(),
		))
	})
	return
}

// OptOutNetworkExtenderMeasurement withdraws the extender with this identity
// key from measurement because its owner opted out (lag.dev/PLAN.md C4, C9,
// decision 18), and returns how many live certificates were withdrawn: every
// live certificate is withdrawn with `reason`, which the dns chain takes out
// of the zone and the issuance chain revokes, and the extender's request
// becomes withdrawn, its certificate request dropped, so nothing is issued for
// it. An extender with neither is left as it is.
//
// It is called while the flag keeps arriving, each time withdrawing anything
// that came since. Only a later request without the flag puts the extender
// back in the queue (SetNetworkExtenderMeasurementRequest): the name is the
// same, so its certificate is a renewal.
func OptOutNetworkExtenderMeasurement(
	ctx context.Context,
	publicKey []byte,
	reason string,
	now time.Time,
) (withdrawn int) {
	server.Tx(ctx, func(tx server.PgTx) {
		withdrawn = 0
		var extenderId *server.Id
		result, err := tx.Query(
			ctx,
			`
			SELECT extender_id
			FROM network_extender
			WHERE public_key = $1
			`,
			publicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				extenderId = &id
			}
		})
		if extenderId == nil {
			return
		}
		withdrawn = withdrawNetworkExtenderMeasurementCertsInTx(ctx, tx, nil, extenderId, reason, now)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET
				state = $2,
				reason = $3,
				csr = NULL,
				pending_time = NULL,
				retry_time = NULL,
				update_time = $4
			WHERE extender_id = $1
			`,
			*extenderId,
			NetworkExtenderMeasurementStateWithdrawn,
			reason,
			now.UTC(),
		))
	})
	return
}

// CountNetworkExtenderMeasurementCertsCountedSince counts the certificates
// issued since `since` that count against the weekly cap (C7).
func CountNetworkExtenderMeasurementCertsCountedSince(ctx context.Context, since time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COUNT(*)
			FROM network_extender_measurement_cert
			WHERE cap_counted AND $1 <= issue_time
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// HasUnexpiredNetworkExtenderMeasurementCertForNames reports whether a
// certificate for exactly these names, issued to any extender, is still
// unexpired at `now`, withdrawn and revoked ones included. To the CA an order
// for a set of names it has an earlier certificate for is a renewal, outside
// the registered domain's weekly budget (C7); a revocation does not change
// that, and an expired certificate is not relied on to.
func HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx context.Context, names []string, now time.Time) (unexpired bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT 1
			FROM network_extender_measurement_cert
			WHERE names = $1 AND $2 < not_after
			LIMIT 1
			`,
			joinNetworkExtenderMeasurementNames(names),
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			unexpired = result.Next()
		})
	})
	return
}

// GetNetworkExtenderMeasurementCertsForRenewal reads the live certificates
// whose renewal time has come (C6), soonest due first. Each comes with its
// extender and the extender's latest request, whose key a renewal follows.
func GetNetworkExtenderMeasurementCertsForRenewal(
	ctx context.Context,
	now time.Time,
	limit int,
) []*NetworkExtenderMeasurementTarget {
	targets := []*NetworkExtenderMeasurementTarget{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementCertColumns+`
			FROM network_extender_measurement_cert
			WHERE
				withdraw_time IS NULL AND
				renewed_time IS NULL AND
				$1 < not_after AND
				renew_time <= $1
			ORDER BY renew_time ASC, cert_id ASC
			LIMIT $2
			`,
			now.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				targets = append(targets, &NetworkExtenderMeasurementTarget{
					Cert: scanNetworkExtenderMeasurementCert(result),
				})
			}
		})
		fillNetworkExtenderMeasurementRequestsInConn(ctx, conn, targets)
		fillNetworkExtenderMeasurementTargetsInConn(ctx, conn, targets)
	})
	return targets
}

// Loads the latest request of every target's extender, on the caller's
// connection.
func fillNetworkExtenderMeasurementRequestsInConn(
	ctx context.Context,
	conn server.PgConn,
	targets []*NetworkExtenderMeasurementTarget,
) {
	extenderIds := []server.Id{}
	for _, target := range targets {
		extenderIds = append(extenderIds, target.extenderId())
	}
	if len(extenderIds) == 0 {
		return
	}
	measurements := map[server.Id]*NetworkExtenderMeasurement{}
	result, err := conn.Query(
		ctx,
		`
		SELECT `+networkExtenderMeasurementColumns+`
		FROM network_extender_measurement
		WHERE extender_id = ANY($1)
		`,
		extenderIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			measurement := scanNetworkExtenderMeasurement(result)
			measurements[measurement.ExtenderId] = measurement
		}
	})
	for _, target := range targets {
		target.Measurement = measurements[target.extenderId()]
	}
}

// DeferNetworkExtenderMeasurementCertRenewal counts a failed renewal and puts
// the next attempt at retryTime (C6).
func DeferNetworkExtenderMeasurementCertRenewal(ctx context.Context, certId server.Id, retryTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET renew_time = $2, renew_failure_count = renew_failure_count + 1
			WHERE cert_id = $1
			`,
			certId,
			retryTime.UTC(),
		))
	})
}

// ReopenExpiredNetworkExtenderMeasurements puts an issued request whose
// certificate ran out un-renewed back in the queue, and returns how many. A
// renewal that keeps failing ends when the certificate expires, since only a
// live certificate is renewed (C6), and without this the request would stay
// issued with nothing behind it until the extender happened to ask again. A
// withdrawal never leaves this state: it moves the request itself (C9).
func ReopenExpiredNetworkExtenderMeasurements(ctx context.Context, now time.Time) (reopened int) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET
				state = $2,
				reason = '',
				pending_time = $1,
				update_time = $1
			WHERE
				state = $3 AND
				csr IS NOT NULL AND
				NOT EXISTS (
					SELECT 1
					FROM network_extender_measurement_cert
					WHERE
						network_extender_measurement_cert.extender_id = network_extender_measurement.extender_id AND
						network_extender_measurement_cert.names = network_extender_measurement.names AND
						network_extender_measurement_cert.public_key = network_extender_measurement.public_key AND
						network_extender_measurement_cert.withdraw_time IS NULL AND
						$1 < network_extender_measurement_cert.not_after
				)
			`,
			now.UTC(),
			NetworkExtenderMeasurementStatePending,
			NetworkExtenderMeasurementStateIssued,
		))
		reopened = int(tag.RowsAffected())
	})
	return
}

// GetLiveNetworkExtenderMeasurementCerts reads every certificate that is
// neither withdrawn nor expired at `now`, renewed ones included, each with its
// extender. The dns reconcile publishes from these (C8), the lag.dev sample
// offers the ones published (A4), and the withdrawal sweep judges them (C9).
// The chain is not read.
func GetLiveNetworkExtenderMeasurementCerts(ctx context.Context, now time.Time) []*NetworkExtenderMeasurementTarget {
	targets := []*NetworkExtenderMeasurementTarget{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				cert_id,
				extender_id,
				names,
				not_before,
				not_after,
				issue_time,
				renewed_time,
				publish_time
			FROM network_extender_measurement_cert
			WHERE withdraw_time IS NULL AND $1 < not_after
			ORDER BY extender_id, issue_time
			`,
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				cert := &NetworkExtenderMeasurementCert{}
				var names string
				server.Raise(result.Scan(
					&cert.CertId,
					&cert.ExtenderId,
					&names,
					&cert.NotBefore,
					&cert.NotAfter,
					&cert.IssueTime,
					&cert.RenewedTime,
					&cert.PublishTime,
				))
				cert.Names = splitNetworkExtenderMeasurementNames(names)
				targets = append(targets, &NetworkExtenderMeasurementTarget{Cert: cert})
			}
		})
		fillNetworkExtenderMeasurementTargetsInConn(ctx, conn, targets)
	})
	return targets
}

// The column form of each of several single names.
func networkExtenderMeasurementNameValues(names []string) []string {
	values := []string{}
	for _, name := range names {
		if value := joinNetworkExtenderMeasurementNames([]string{name}); value != "" && !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	return values
}

// PublishNetworkExtenderMeasurementNames records that the dns reconcile found
// these names published, in sync on every authoritative server (C8): each
// live certificate for one of them that does not say so yet takes
// `publishTime`, and one that does keeps its own. That includes a renewal
// that arrived while its name was published. Returns how many certificates it
// marked.
//
// Only the reconcile calls this, and only after Route 53 reported the change
// that wrote the name's records in sync, so a name is never marked published
// before dns answers for it everywhere.
func PublishNetworkExtenderMeasurementNames(ctx context.Context, names []string, publishTime time.Time) (published int) {
	values := networkExtenderMeasurementNameValues(names)
	if len(values) == 0 {
		return 0
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET publish_time = $2
			WHERE
				names = ANY($1) AND
				publish_time IS NULL AND
				withdraw_time IS NULL AND
				$2 < not_after
			`,
			values,
			publishTime.UTC(),
		))
		published = int(tag.RowsAffected())
	})
	return
}

// UnpublishNetworkExtenderMeasurementNames records that these names are not
// published (C8): the reconcile found that they hold none of their records,
// and is about to write them again. Returns how many certificates it changed.
func UnpublishNetworkExtenderMeasurementNames(ctx context.Context, names []string) (unpublished int) {
	values := networkExtenderMeasurementNameValues(names)
	if len(values) == 0 {
		return 0
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET publish_time = NULL
			WHERE names = ANY($1) AND publish_time IS NOT NULL
			`,
			values,
		))
		unpublished = int(tag.RowsAffected())
	})
	return
}

// UnpublishNetworkExtenderMeasurementNamesExcept records that every name but
// `keepNames` is not published (C8). The reconcile calls it with the names it
// is to publish before it deletes any record, so a name that is to go stops
// being offered before its records do (A4), whatever certificate it was
// published for: one withdrawn or expired since, or one whose extender may not
// be measured now. Returns how many certificates it changed.
func UnpublishNetworkExtenderMeasurementNamesExcept(ctx context.Context, keepNames []string) (unpublished int) {
	values := networkExtenderMeasurementNameValues(keepNames)
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET publish_time = NULL
			WHERE publish_time IS NOT NULL AND NOT (names = ANY($1))
			`,
			values,
		))
		unpublished = int(tag.RowsAffected())
	})
	return
}

// MarkNetworkExtenderMeasurementCertDelivered records that A8 answered the
// extender with this certificate's chain, the first time it did (A8). The
// extender installs a chain only when it reads it there, at its first poll
// after the issuance, which comes up to half an hour later (an hour after a
// refusal), while the dns reconcile publishes the name within a minute or
// two. Until then the name answers without the certificate, so the lag.dev
// sample (A4) and the records' MeasurementId (C10) wait for this. A later
// answer keeps the first time. Returns whether this call marked it.
func MarkNetworkExtenderMeasurementCertDelivered(ctx context.Context, certId server.Id, deliverTime time.Time) (marked bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET deliver_time = $2
			WHERE cert_id = $1 AND deliver_time IS NULL
			`,
			certId,
			deliverTime.UTC(),
		))
		marked = tag.RowsAffected() == 1
	})
	return
}

// WithdrawNetworkExtenderMeasurementCert withdraws one live certificate (C9):
// the dns reconcile stops publishing its names at its next pass, and the
// issuance task revokes it. The extender's request for those names becomes
// withdrawn, with the reason, which A8 answers until the extender asks again,
// so the extender drops the certificate. False when it was already
// withdrawn.
func WithdrawNetworkExtenderMeasurementCert(
	ctx context.Context,
	certId server.Id,
	reason string,
	now time.Time,
) (withdrawn bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		withdrawn = withdrawNetworkExtenderMeasurementCertsInTx(ctx, tx, &certId, nil, reason, now) == 1
	})
	return
}

// Withdraws one certificate, or every live certificate of one extender, inside
// the caller's transaction, and the requests for their names with them.
// Returns how many were withdrawn.
func withdrawNetworkExtenderMeasurementCertsInTx(
	ctx context.Context,
	tx server.PgTx,
	certId *server.Id,
	extenderId *server.Id,
	reason string,
	now time.Time,
) int {
	if certId == nil && extenderId == nil {
		// never every certificate at once
		return 0
	}
	type withdrawnCert struct {
		extenderId server.Id
		names      string
	}
	withdrawnCerts := []withdrawnCert{}
	result, err := tx.Query(
		ctx,
		`
		UPDATE network_extender_measurement_cert
		SET withdraw_time = $3, withdraw_reason = $4
		WHERE
			($1::uuid IS NULL OR cert_id = $1) AND
			($2::uuid IS NULL OR extender_id = $2) AND
			withdraw_time IS NULL
		RETURNING extender_id, names
		`,
		certId,
		extenderId,
		now.UTC(),
		reason,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var w withdrawnCert
			server.Raise(result.Scan(&w.extenderId, &w.names))
			withdrawnCerts = append(withdrawnCerts, w)
		}
	})
	for _, w := range withdrawnCerts {
		// the request for those names is withdrawn with it, whatever state it
		// was in, until the extender asks again; a request the extender has
		// since made for other names is another request and stands
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement
			SET
				state = $3,
				reason = $4,
				pending_time = NULL,
				retry_time = NULL,
				update_time = $5
			WHERE extender_id = $1 AND names = $2
			`,
			w.extenderId,
			w.names,
			NetworkExtenderMeasurementStateWithdrawn,
			reason,
			now.UTC(),
		))
	}
	return len(withdrawnCerts)
}

// GetNetworkExtenderMeasurementCertsToRevoke reads withdrawn certificates the
// CA has not yet revoked (C9), oldest withdrawal first. An expired certificate
// needs no revocation and is not read.
func GetNetworkExtenderMeasurementCertsToRevoke(
	ctx context.Context,
	now time.Time,
	limit int,
) []*NetworkExtenderMeasurementCert {
	certs := []*NetworkExtenderMeasurementCert{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementCertColumns+`
			FROM network_extender_measurement_cert
			WHERE
				withdraw_time IS NOT NULL AND
				revoke_time IS NULL AND
				$1 < not_after AND
				(revoke_retry_time IS NULL OR revoke_retry_time <= $1)
			ORDER BY withdraw_time ASC, cert_id ASC
			LIMIT $2
			`,
			now.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				certs = append(certs, scanNetworkExtenderMeasurementCert(result))
			}
		})
	})
	return certs
}

// MarkNetworkExtenderMeasurementCertRevoked records that the CA revoked one
// withdrawn certificate.
func MarkNetworkExtenderMeasurementCertRevoked(ctx context.Context, certId server.Id, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET revoke_time = $2
			WHERE cert_id = $1 AND revoke_time IS NULL
			`,
			certId,
			now.UTC(),
		))
	})
}

// DeferNetworkExtenderMeasurementCertRevocation counts a failed revocation and
// puts the next attempt at retryTime.
func DeferNetworkExtenderMeasurementCertRevocation(ctx context.Context, certId server.Id, retryTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET revoke_retry_time = $2, revoke_failure_count = revoke_failure_count + 1
			WHERE cert_id = $1
			`,
			certId,
			retryTime.UTC(),
		))
	})
}

// GetNetworkExtenderMeasurementForClient reads what A8 answers an extender
// with: the extender the calling client activated -- the one with this key
// when a key is given, else its active one, and of those the one activated
// last -- its latest request, and the newest live certificate that answers
// that request. Every result is nil when the client activated no such
// extender.
//
// The record stamp does not pick the extender: the drip advances it for every
// active extender, so a client that changed its identity key could be
// answered for the extender it left behind until that one's probes run out.
func GetNetworkExtenderMeasurementForClient(
	ctx context.Context,
	clientId server.Id,
	publicKey []byte,
	now time.Time,
) (extenderId *server.Id, measurement *NetworkExtenderMeasurement, cert *NetworkExtenderMeasurementCert) {
	server.Db(ctx, func(conn server.PgConn) {
		extenderId = nil
		measurement = nil
		cert = nil

		result, err := conn.Query(
			ctx,
			`
			SELECT network_extender.extender_id
			FROM network_extender
			LEFT JOIN LATERAL (
				SELECT MAX(network_extender_address.activate_time) AS activate_time
				FROM network_extender_address
				WHERE network_extender_address.extender_id = network_extender.extender_id
			) AS latest_address ON true
			WHERE
				network_extender.client_id = $1 AND
				($2::bytea IS NULL OR network_extender.public_key = $2)
			ORDER BY
				network_extender.active DESC,
				latest_address.activate_time DESC NULLS LAST,
				network_extender.create_time DESC,
				network_extender.extender_id DESC
			LIMIT 1
			`,
			clientId,
			publicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				extenderId = &id
			}
		})
		if extenderId == nil {
			return
		}

		result, err = conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementColumns+`
			FROM network_extender_measurement
			WHERE extender_id = $1
			`,
			*extenderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				measurement = scanNetworkExtenderMeasurement(result)
			}
		})
		if measurement == nil {
			return
		}

		result, err = conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementCertColumns+`
			FROM network_extender_measurement_cert
			WHERE
				extender_id = $1 AND
				names = $2 AND
				public_key = $3 AND
				withdraw_time IS NULL AND
				$4 < not_after
			ORDER BY issue_time DESC, cert_id DESC
			LIMIT 1
			`,
			*extenderId,
			joinNetworkExtenderMeasurementNames(measurement.Names),
			measurement.PublicKey,
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				cert = scanNetworkExtenderMeasurementCert(result)
			}
		})
	})
	return
}

// The latest expiry of the extender's live certificates that serve, nil when
// it holds none, inside the caller's transaction. A record signed in the same
// transaction says the extender serves the measurement API while this is
// after its issue time (C10). A certificate serves once its name is published
// and A8 has handed it to the extender for the key of its latest request
// (networkExtenderMeasurementCertServesSql). An unpublished name is left out
// for the same reason the lag.dev sample leaves it out (A4): a reader that
// computed it from the record would look it up before it resolves. So the id
// goes from the records signed while publication is cleared -- a withdrawal,
// an opt-out, records gone missing -- and comes back with the reconcile pass
// that publishes the name again (C8). An undelivered certificate is left out
// because the name answers without it until the extender reads it from A8.
func networkExtenderMeasurementCertNotAfterInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
) *time.Time {
	var notAfter *time.Time
	result, err := tx.Query(
		ctx,
		`
		SELECT MAX(network_extender_measurement_cert.not_after)
		FROM network_extender_measurement_cert
		WHERE
			network_extender_measurement_cert.extender_id = $1 AND
			`+networkExtenderMeasurementCertServesSql+`
		`,
		extenderId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&notAfter))
		}
	})
	return notAfter
}

// The condition on a network_extender_measurement_cert row that it serves
// the measurement api, whatever its expiry (C10, A4): it is not withdrawn,
// its name is published (C8), and A8 has handed it to the extender
// (deliver_time) for the key of the extender's latest request. A certificate
// for any other key is one the extender asked to replace, and holds no key
// for once it has a new one.
const networkExtenderMeasurementCertServesSql = `
	network_extender_measurement_cert.withdraw_time IS NULL AND
	network_extender_measurement_cert.publish_time IS NOT NULL AND
	network_extender_measurement_cert.deliver_time IS NOT NULL AND
	EXISTS (
		SELECT 1
		FROM network_extender_measurement
		WHERE
			network_extender_measurement.extender_id = network_extender_measurement_cert.extender_id AND
			network_extender_measurement.public_key = network_extender_measurement_cert.public_key
	)
`

// The same for several extenders on the caller's connection; an extender
// with no live certificate that serves has no entry.
func getNetworkExtenderMeasurementCertNotAftersInConn(
	ctx context.Context,
	conn server.PgConn,
	extenderIds []server.Id,
) map[server.Id]time.Time {
	notAfters := map[server.Id]time.Time{}
	if len(extenderIds) == 0 {
		return notAfters
	}
	result, err := conn.Query(
		ctx,
		`
		SELECT
			network_extender_measurement_cert.extender_id,
			MAX(network_extender_measurement_cert.not_after)
		FROM network_extender_measurement_cert
		WHERE
			network_extender_measurement_cert.extender_id = ANY($1) AND
			`+networkExtenderMeasurementCertServesSql+`
		GROUP BY network_extender_measurement_cert.extender_id
		`,
		extenderIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var extenderId server.Id
			var notAfter time.Time
			server.Raise(result.Scan(&extenderId, &notAfter))
			notAfters[extenderId] = notAfter
		}
	})
	return notAfters
}

// Guards a move of one extender into the gated tier or a canary channel
// inside the caller's transaction, before the caller writes it (A9, C9).
// `exposing` is whether the move puts the extender in either.
//
// An extender that ever held a measurement certificate is refused with
// ErrNetworkExtenderMeasurementExposure unless the operator accepts the
// exposure, and the acceptance is recorded, which is what the table's
// constraint requires of the write that follows. Any accepted move withdraws
// the extender's live certificates at once, with `reason`: a gated extender
// or a canary is never measured, and the issuance task revokes them.
//
// The extender row is locked first, so a certificate flagged concurrently
// (FlagNetworkExtenderMeasurementEverIssued) is either seen here or refused
// there.
func guardNetworkExtenderMeasurementExposureInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
	exposing bool,
	acceptExposure bool,
	reason string,
	now time.Time,
) error {
	if !exposing {
		return nil
	}
	everIssued := false
	found := false
	result, err := tx.Query(
		ctx,
		`
		SELECT measurement_ever_issued
		FROM network_extender
		WHERE extender_id = $1
		FOR UPDATE
		`,
		extenderId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&everIssued))
			found = true
		}
	})
	if !found {
		// the write that follows changes nothing either
		return nil
	}
	if everIssued {
		if !acceptExposure {
			return ErrNetworkExtenderMeasurementExposure
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET measurement_exposure_accepted = true
			WHERE extender_id = $1
			`,
			extenderId,
		))
	}
	withdrawNetworkExtenderMeasurementCertsInTx(ctx, tx, nil, &extenderId, reason, now)
	return nil
}

// Testing_GetNetworkExtenderMeasurementCerts reads every certificate of one
// extender, oldest first, whatever its state.
func Testing_GetNetworkExtenderMeasurementCerts(ctx context.Context, extenderId server.Id) []*NetworkExtenderMeasurementCert {
	certs := []*NetworkExtenderMeasurementCert{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementCertColumns+`
			FROM network_extender_measurement_cert
			WHERE extender_id = $1
			ORDER BY issue_time ASC, cert_id ASC
			`,
			extenderId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				certs = append(certs, scanNetworkExtenderMeasurementCert(result))
			}
		})
	})
	return certs
}

// Testing_SetNetworkExtenderMeasurementCertRenewTime moves one certificate's
// renewal, so a test need not wait two thirds of a lifetime.
func Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx context.Context, certId server.Id, renewTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_measurement_cert
			SET renew_time = $2
			WHERE cert_id = $1
			`,
			certId,
			renewTime.UTC(),
		))
	})
}

// Testing_GetNetworkExtenderMeasurement reads one extender's request, nil for
// none.
func Testing_GetNetworkExtenderMeasurement(ctx context.Context, extenderId server.Id) *NetworkExtenderMeasurement {
	var measurement *NetworkExtenderMeasurement
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+networkExtenderMeasurementColumns+`
			FROM network_extender_measurement
			WHERE extender_id = $1
			`,
			extenderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				measurement = scanNetworkExtenderMeasurement(result)
			}
		})
	})
	return measurement
}

// Testing_GetNetworkExtenderMeasurementExposure reads the two flags of A9.
func Testing_GetNetworkExtenderMeasurementExposure(ctx context.Context, extenderId server.Id) (everIssued bool, accepted bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT measurement_ever_issued, measurement_exposure_accepted
			FROM network_extender
			WHERE extender_id = $1
			`,
			extenderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&everIssued, &accepted))
			}
		})
	})
	return
}
