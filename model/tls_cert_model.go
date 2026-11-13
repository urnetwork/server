package model

// certs' rows in Postgres (TLS-M5-16; §13.8, §13.15; §2.8 cert-1):
// certsservice.CertsStore on network_tls_cert, one row per order and per
// certificate. The engine's suite (sidecartest.RunCertsStoreSuite) holds this
// store and the fake to one set of rules.
//
// A row is sidecar.TlsCertRecord's fields and no other. An order's time is its
// cert_id's, which certs mints on its own clock, so "orders since" is a range
// of ids and reads network_tls_cert's (…, cert_id) indexes. The store keeps no
// clock: every time is the caller's, whole to the microsecond as Postgres
// keeps it, state_time among them, which a row is written with and each change
// of state sets (QD-742).
//
// A state change applies only while the row is in one of the transition's
// From states, in one UPDATE, so two passes cannot both move a row. The same
// UPDATE writes an issuance's chain, and keeps revocation_reason only on a
// revoking row: any other state clears it (QD-741). Nothing here is on a
// connection's path.
//
// The table is MG5's: a binary that does not link the group answers
// ErrCertsStoreUnavailable for every call (server.SidecarMg5Linked).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
)

var _ certsservice.CertsStore = (*CertsStore)(nil)

// This binary does not link MG5, so the table does not exist for it.
var ErrCertsStoreUnavailable = errors.New("the certs store is not linked in this binary")

// Tunables of the store.
type CertsStoreSettings struct {
	// The longest a call waits on Postgres, under the caller's own deadline.
	CallTimeout time.Duration
}

func DefaultCertsStoreSettings() *CertsStoreSettings {
	return &CertsStoreSettings{
		CallTimeout: 10 * time.Second,
	}
}

// See the file header. Safe for concurrent use.
type CertsStore struct {
	settings *CertsStoreSettings
}

// The store certs' host passes its engine. nil settings take the defaults.
func NewCertsStore(settings *CertsStoreSettings) *CertsStore {
	if settings == nil {
		settings = DefaultCertsStoreSettings()
	}
	return &CertsStore{settings: settings}
}

const certsStoreColumns = `
	cert_id,
	network_id,
	client_id,
	sidecar_id,
	identifier_set_sha256,
	spki_sha256,
	attestation,
	csr,
	ca,
	account_uri,
	profile,
	serial,
	not_before,
	not_after,
	ari_window_start,
	ari_window_end,
	state,
	state_time,
	successor_cert_id,
	chain,
	revocation_reason
`

// Runs callback under the call's deadline and returns what it raised, as an
// error. A binary without MG5 runs nothing.
func (self *CertsStore) run(ctx context.Context, callback func(ctx context.Context)) (returnErr error) {
	if !server.SidecarMg5Linked() {
		return ErrCertsStoreUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, self.settings.CallTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			if err := callCtx.Err(); err != nil {
				returnErr = err
				return
			}
			if recoveredErr, ok := r.(error); ok {
				returnErr = recoveredErr
			} else {
				returnErr = fmt.Errorf("certs store: %v", r)
			}
		}
	}()
	callback(callCtx)
	return nil
}

func certsStoreNullString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func certsStoreNullTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func certsStoreNullId(value sidecar.Id) *server.Id {
	if value == (sidecar.Id{}) {
		return nil
	}
	id := server.Id(value)
	return &id
}

func certsStoreNullChain(value [][]byte) [][]byte {
	if len(value) == 0 {
		return nil
	}
	return value
}

func certsStoreNullBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return value
}

// The lowest id whose time is at or after at: at's millisecond, rounded up,
// and zero below it.
func certsStoreIdFloor(at time.Time) server.Id {
	ms := at.UnixMilli()
	if at.After(time.UnixMilli(ms)) {
		ms += 1
	}
	id := server.Id{}
	if ms < 0 {
		return id
	}
	for index := range 6 {
		id[index] = byte(uint64(ms) >> (8 * (5 - index)))
	}
	return id
}

func certsStoreScan(row pgx.Row) (*sidecar.TlsCertRecord, error) {
	var certId, networkId, clientId, sidecarId server.Id
	var successorCertId *server.Id
	var ca, accountUri, profile, serial, revocationReason *string
	var notBefore, notAfter, ariWindowStart, ariWindowEnd *time.Time
	var state string
	var stateTime time.Time
	record := &sidecar.TlsCertRecord{}
	if err := row.Scan(
		&certId,
		&networkId,
		&clientId,
		&sidecarId,
		&record.IdentifierSetSha256,
		&record.SpkiSha256,
		&record.Attestation,
		&record.Csr,
		&ca,
		&accountUri,
		&profile,
		&serial,
		&notBefore,
		&notAfter,
		&ariWindowStart,
		&ariWindowEnd,
		&state,
		&stateTime,
		&successorCertId,
		&record.Chain,
		&revocationReason,
	); err != nil {
		return nil, err
	}
	record.CertId = sidecar.Id(certId)
	record.NetworkId = sidecar.Id(networkId)
	record.ClientId = sidecar.Id(clientId)
	record.SidecarId = sidecar.Id(sidecarId)
	record.State = sidecar.TlsCertState(state)
	record.StateTime = stateTime.UTC()
	if revocationReason != nil {
		record.RevocationReason = sidecar.TlsCertRevocationReason(*revocationReason)
	}
	if ca != nil {
		record.Ca = *ca
	}
	if accountUri != nil {
		record.AccountUri = *accountUri
	}
	if profile != nil {
		record.Profile = *profile
	}
	if serial != nil {
		record.Serial = *serial
	}
	for _, timeField := range []struct {
		column *time.Time
		field  *time.Time
	}{
		{notBefore, &record.NotBefore},
		{notAfter, &record.NotAfter},
		{ariWindowStart, &record.AriWindowStart},
		{ariWindowEnd, &record.AriWindowEnd},
	} {
		if timeField.column != nil {
			*timeField.field = timeField.column.UTC()
		}
	}
	if successorCertId != nil {
		record.SuccessorCertId = sidecar.Id(*successorCertId)
	}
	if len(record.Csr) == 0 {
		record.Csr = nil
	}
	if len(record.Chain) == 0 {
		record.Chain = nil
	}
	return record, nil
}

// The rows a query answers, in its order.
func (self *CertsStore) list(ctx context.Context, sql string, args ...any) (rows []*sidecar.TlsCertRecord, returnErr error) {
	rows = []*sidecar.TlsCertRecord{}
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, sql, args...)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					record, err := certsStoreScan(result)
					server.Raise(err)
					rows = append(rows, record)
				}
			})
		})
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return rows, nil
}

// The first row a query answers, nil for none.
func (self *CertsStore) first(ctx context.Context, sql string, args ...any) (*sidecar.TlsCertRecord, error) {
	rows, err := self.list(ctx, sql, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (self *CertsStore) InsertCert(ctx context.Context, row *sidecar.TlsCertRecord) error {
	if row == nil || row.CertId == (sidecar.Id{}) {
		return errors.New("certs store: a row needs an id")
	}
	if row.StateTime.IsZero() {
		return certsservice.ErrCertsStateTime
	}
	// a reason waits only in a revoking row
	revocationReason := ""
	if row.State == sidecar.TlsCertStateRevoking {
		revocationReason = string(row.RevocationReason)
	}
	inserted := false
	err := self.run(ctx, func(ctx context.Context) {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_tls_cert (`+certsStoreColumns+`)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
					ON CONFLICT (cert_id) DO NOTHING
				`,
				server.Id(row.CertId),
				server.Id(row.NetworkId),
				server.Id(row.ClientId),
				server.Id(row.SidecarId),
				row.IdentifierSetSha256,
				row.SpkiSha256,
				row.Attestation,
				certsStoreNullBytes(row.Csr),
				certsStoreNullString(row.Ca),
				certsStoreNullString(row.AccountUri),
				certsStoreNullString(row.Profile),
				certsStoreNullString(row.Serial),
				certsStoreNullTime(row.NotBefore),
				certsStoreNullTime(row.NotAfter),
				certsStoreNullTime(row.AriWindowStart),
				certsStoreNullTime(row.AriWindowEnd),
				string(row.State),
				row.StateTime.UTC(),
				certsStoreNullId(row.SuccessorCertId),
				certsStoreNullChain(row.Chain),
				certsStoreNullString(revocationReason),
			))
			inserted = tag.RowsAffected() == 1
		})
	})
	if err != nil {
		return err
	}
	if !inserted {
		return certsservice.ErrCertsConflict
	}
	return nil
}

func (self *CertsStore) GetCert(ctx context.Context, certId sidecar.Id) (*sidecar.TlsCertRecord, error) {
	row, err := self.first(
		ctx,
		`SELECT `+certsStoreColumns+` FROM network_tls_cert WHERE cert_id = $1`,
		server.Id(certId),
	)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, certsservice.ErrCertsNotFound
	}
	return row, nil
}

func (self *CertsStore) ListNetworkCerts(ctx context.Context, networkId sidecar.Id) ([]*sidecar.TlsCertRecord, error) {
	return self.list(
		ctx,
		`SELECT `+certsStoreColumns+` FROM network_tls_cert WHERE network_id = $1 ORDER BY cert_id`,
		server.Id(networkId),
	)
}

func (self *CertsStore) FindOpenOrder(ctx context.Context, networkId sidecar.Id, sidecarId sidecar.Id, spkiSha256 []byte, identifierSetSha256 []byte) (*sidecar.TlsCertRecord, error) {
	return self.first(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert
			WHERE
				identifier_set_sha256 = $1 AND
				network_id = $2 AND
				sidecar_id = $3 AND
				spki_sha256 = $4 AND
				state IN ('queued', 'ordering', 'waiting_dns')
			ORDER BY cert_id DESC
			LIMIT 1
		`,
		identifierSetSha256,
		server.Id(networkId),
		server.Id(sidecarId),
		spkiSha256,
	)
}

func (self *CertsStore) FindLiveCertBySerial(ctx context.Context, networkId sidecar.Id, serial string, now time.Time) (*sidecar.TlsCertRecord, error) {
	if serial == "" {
		return nil, nil
	}
	return self.first(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert
			WHERE
				network_id = $1 AND
				serial = $2 AND
				state = 'issued' AND
				$3 < not_after
			ORDER BY cert_id DESC
			LIMIT 1
		`,
		server.Id(networkId),
		serial,
		now.UTC(),
	)
}

func (self *CertsStore) GetPredecessor(ctx context.Context, certId sidecar.Id) (*sidecar.TlsCertRecord, error) {
	if certId == (sidecar.Id{}) {
		return nil, nil
	}
	return self.first(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert
			WHERE successor_cert_id = $1
			ORDER BY cert_id
			LIMIT 1
		`,
		server.Id(certId),
	)
}

func (self *CertsStore) SetIssuedBefore(ctx context.Context, identifierSetSha256 []byte, beforeCertId sidecar.Id) (issued bool, returnErr error) {
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
					SELECT EXISTS (
						SELECT 1
						FROM network_tls_cert
						WHERE
							identifier_set_sha256 = $1 AND
							cert_id < $2 AND
							serial IS NOT NULL
					)
				`,
				identifierSetSha256,
				server.Id(beforeCertId),
			).Scan(&issued))
		})
	})
	return
}

func (self *CertsStore) ListSetOrders(ctx context.Context, identifierSetSha256 []byte, ca certsservice.CertsCa, since time.Time) ([]*sidecar.TlsCertRecord, error) {
	return self.list(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert
			WHERE
				identifier_set_sha256 = $1 AND
				$2 <= cert_id AND
				ca = $3
			ORDER BY cert_id
		`,
		identifierSetSha256,
		certsStoreIdFloor(since),
		string(ca),
	)
}

func (self *CertsStore) ListNewSetOrders(ctx context.Context, ca certsservice.CertsCa, since time.Time) ([]*sidecar.TlsCertRecord, error) {
	return self.list(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert t
			WHERE
				ca = $1 AND
				(
					state IN ('ordering', 'waiting_dns') OR
					(serial IS NOT NULL AND not_before IS NOT NULL AND $2 <= not_before)
				) AND
				NOT EXISTS (
					SELECT 1
					FROM network_tls_cert older
					WHERE
						older.identifier_set_sha256 = t.identifier_set_sha256 AND
						older.cert_id < t.cert_id AND
						older.serial IS NOT NULL
				)
			ORDER BY cert_id
		`,
		string(ca),
		since.UTC(),
	)
}

// Reads network_client_owner.ephemeral, MG1's. A client with no row, a legacy
// client, is not ephemeral.
func (self *CertsStore) ClientEphemeral(ctx context.Context, clientId sidecar.Id) (ephemeral bool, returnErr error) {
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Db(ctx, func(conn server.PgConn) {
			err := conn.QueryRow(
				ctx,
				`SELECT ephemeral FROM network_client_owner WHERE client_id = $1`,
				server.Id(clientId),
			).Scan(&ephemeral)
			if errors.Is(err, pgx.ErrNoRows) {
				ephemeral = false
				return
			}
			server.Raise(err)
		})
	})
	return
}

func (self *CertsStore) ListQueued(ctx context.Context, afterCertId sidecar.Id, count int) ([]*sidecar.TlsCertRecord, error) {
	if count <= 0 {
		return []*sidecar.TlsCertRecord{}, nil
	}
	return self.list(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert
			WHERE state = 'queued' AND $1 < cert_id
			ORDER BY cert_id
			LIMIT $2
		`,
		server.Id(afterCertId),
		count,
	)
}

func (self *CertsStore) ListInStates(ctx context.Context, states []sidecar.TlsCertState, afterCertId sidecar.Id, count int) ([]*sidecar.TlsCertRecord, error) {
	if count <= 0 || len(states) == 0 {
		return []*sidecar.TlsCertRecord{}, nil
	}
	stateNames := make([]string, 0, len(states))
	for _, state := range states {
		stateNames = append(stateNames, string(state))
	}
	return self.list(
		ctx,
		`
			SELECT `+certsStoreColumns+`
			FROM network_tls_cert
			WHERE state = ANY($1) AND $2 < cert_id
			ORDER BY cert_id
			LIMIT $3
		`,
		stateNames,
		server.Id(afterCertId),
		count,
	)
}

func (self *CertsStore) TransitionCert(ctx context.Context, transition *certsservice.CertsTransition) (applied bool, returnErr error) {
	if transition == nil || len(transition.From) == 0 || transition.To == "" {
		return false, errors.New("certs store: a transition needs its states")
	}
	if transition.At.IsZero() {
		return false, certsservice.ErrCertsStateTime
	}
	from := make([]string, 0, len(transition.From))
	for _, state := range transition.From {
		from = append(from, string(state))
	}
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_tls_cert
					SET
						state = $3,
						state_time = CASE WHEN state = $3::varchar THEN state_time ELSE $12::timestamp END,
						ca = COALESCE($4, ca),
						account_uri = COALESCE($5, account_uri),
						profile = COALESCE($6, profile),
						serial = COALESCE($7, serial),
						not_before = COALESCE($8, not_before),
						not_after = COALESCE($9, not_after),
						ari_window_start = COALESCE($10, ari_window_start),
						ari_window_end = COALESCE($11, ari_window_end),
						chain = COALESCE($13::bytea[], chain),
						revocation_reason = CASE
							WHEN $3::varchar = 'revoking' THEN COALESCE($14, revocation_reason)
							ELSE NULL
						END
					WHERE cert_id = $1 AND state = ANY($2)
				`,
				server.Id(transition.CertId),
				from,
				string(transition.To),
				certsStoreNullString(string(transition.Ca)),
				certsStoreNullString(transition.AccountUri),
				certsStoreNullString(transition.Profile),
				certsStoreNullString(transition.Serial),
				certsStoreNullTime(transition.NotBefore),
				certsStoreNullTime(transition.NotAfter),
				certsStoreNullTime(transition.AriWindowStart),
				certsStoreNullTime(transition.AriWindowEnd),
				transition.At.UTC(),
				certsStoreNullChain(transition.Chain),
				certsStoreNullString(string(transition.RevocationReason)),
			))
			applied = tag.RowsAffected() == 1
		})
	})
	return
}

func (self *CertsStore) SetSuccessor(ctx context.Context, certId sidecar.Id, successorCertId sidecar.Id) (linked bool, returnErr error) {
	found := false
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_tls_cert
					SET successor_cert_id = $2
					WHERE cert_id = $1 AND successor_cert_id IS NULL
				`,
				server.Id(certId),
				server.Id(successorCertId),
			))
			linked = tag.RowsAffected() == 1
			if linked {
				found = true
				return
			}
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT EXISTS (SELECT 1 FROM network_tls_cert WHERE cert_id = $1)`,
				server.Id(certId),
			).Scan(&found))
		})
	})
	if returnErr == nil && !found {
		return false, certsservice.ErrCertsNotFound
	}
	return
}

// A revoked or failed row ended at its state's time, and a row in any other
// state when its certificate's validity did (cert-1's ttl; QD-742). Two
// statements, each on its own partial index.
func (self *CertsStore) PurgeEnded(ctx context.Context, before time.Time) (removedCount int, returnErr error) {
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_tls_cert WHERE state IN ('revoked', 'failed') AND state_time < $1`,
				before.UTC(),
			))
			removedCount = int(tag.RowsAffected())
			tag = server.RaisePgResult(tx.Exec(
				ctx,
				`
					DELETE FROM network_tls_cert
					WHERE
						not_after IS NOT NULL AND
						not_after < $1 AND
						state NOT IN ('revoked', 'failed')
				`,
				before.UTC(),
			))
			removedCount += int(tag.RowsAffected())
		})
	})
	return
}

// A revoked row's certificate ended at its revocation, and any other's at its
// validity's end. A failed row never had a chain.
func (self *CertsStore) PurgeChains(ctx context.Context, before time.Time) (droppedCount int, returnErr error) {
	returnErr = self.run(ctx, func(ctx context.Context) {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_tls_cert
					SET chain = NULL
					WHERE
						chain IS NOT NULL AND
						(
							(state = 'revoked' AND state_time < $1) OR
							(state NOT IN ('revoked', 'failed') AND not_after IS NOT NULL AND not_after < $1)
						)
				`,
				before.UTC(),
			))
			droppedCount = int(tag.RowsAffected())
		})
	})
	return
}

// What asks for certificates to be revoked (§13.8's Revocation; I-15): a
// takedown or a retiring alias, through §21's and §12's own paths.
type TlsCertRevocationRequest struct {
	NetworkId server.Id
	// One certificate of the network. Zero: every issued certificate of it.
	CertId server.Id
	// Why, for the log alone: abuse:<case_id>, alias:<name>.
	Reason string
	// The key is compromised: the CA is told keyCompromise. Without it the CA
	// is told cessationOfOperation, as §21.6's takedown and a retiring alias
	// have it (§13.8). Either waits in the row until the CA has taken it.
	KeyCompromise bool
}

// Marks the network's issued certificates, or the one named, revoking with the
// request's reason, which certs' revocation chain (TLS-M5-17) then takes to its
// CA; no renewal follows a row that is not issued. Nothing here reaches a CA.
// Returns how many rows it marked: a certificate of another network, one
// already on its way out and one that never issued are none.
func RequestTlsCertRevocation(ctx context.Context, request *TlsCertRevocationRequest) (markedCount int, returnErr error) {
	if !server.SidecarMg5Linked() {
		return 0, ErrCertsStoreUnavailable
	}
	if request == nil || request.NetworkId == (server.Id{}) {
		return 0, errors.New("certs store: a revocation names its network")
	}
	var certId *server.Id
	if request.CertId != (server.Id{}) {
		certId = &request.CertId
	}
	revocationReason := sidecar.TlsCertRevocationReasonCessationOfOperation
	if request.KeyCompromise {
		revocationReason = sidecar.TlsCertRevocationReasonKeyCompromise
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_tls_cert
				SET
					state = 'revoking',
					state_time = $3,
					revocation_reason = $4
				WHERE
					network_id = $1 AND
					state = 'issued' AND
					($2::uuid IS NULL OR cert_id = $2)
			`,
			request.NetworkId,
			certId,
			server.NowUtc(),
			string(revocationReason),
		))
		markedCount = int(tag.RowsAffected())
	})
	return
}
